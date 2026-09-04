package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/miauth"
)

func TestHandleMiAuthBootstrapStart_RedirectsForValidGate(t *testing.T) {
	ts := newMiAuthTestServer(t, miauth.Config{OwnerUsername: "owner"})
	gateID, err := ts.miauth.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	rec := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/bootstrap/"+gateID, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusFound)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Scheme + "://" + loc.Host; got != testIdentityOrigin {
		t.Errorf("redirect origin = %q, want %q", got, testIdentityOrigin)
	}
}

func TestHandleMiAuthBootstrapStart_UnknownGateIs404(t *testing.T) {
	ts := newMiAuthTestServer(t, miauth.Config{OwnerUsername: "owner"})
	rec := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/bootstrap/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleMiAuthBootstrapStart_AlreadyBoundIs404(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())

	// Issue a still-valid, unconsumed gate before the deployment becomes
	// bound through the ordinary allowlisted flow.
	gateID, err := ts.miauth.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	id, state := startLocalSession(t, ts, "route-1", "read:account", "")
	if rec := authorizeWithUser(ts, id, state, testAllowedUserID); rec.Code != http.StatusOK {
		t.Fatalf("callback failed: %d", rec.Code)
	}

	// The gate is unconsumed and unexpired, but the deployment is now
	// bound; the bootstrap route must refuse it rather than let it bind
	// a second, different identity.
	rec := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/bootstrap/"+gateID, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBootstrapFlow_CallbackBindsOwnerWithGenericSuccessPage(t *testing.T) {
	ts := newMiAuthTestServer(t, miauth.Config{OwnerUsername: "owner"})
	gateID, err := ts.miauth.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	rec := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/bootstrap/"+gateID, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start: status = %d %q", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	cb, _ := url.Parse(loc.Query().Get("callback"))
	id, state := cb.Query().Get("id"), cb.Query().Get("state")

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return "any-upstream-user", true, nil
	}
	cbRec := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/callback?id="+url.QueryEscape(id)+"&state="+url.QueryEscape(state), nil))
	if cbRec.Code != http.StatusOK {
		t.Fatalf("callback: status = %d %q", cbRec.Code, cbRec.Body.String())
	}
	if !strings.Contains(cbRec.Body.String(), "Aria") {
		t.Errorf("body = %q, expected a generic return-to-Aria message (no Aria route session exists for bootstrap)", cbRec.Body.String())
	}

	binding, err := ts.db.OwnerBindings.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if binding.UpstreamUserID != "any-upstream-user" {
		t.Errorf("binding.UpstreamUserID = %q, want any-upstream-user", binding.UpstreamUserID)
	}
}

// TestBootstrapFlow_ConcurrentCallbackHasExactlyOneWinner backs the
// "concurrent bootstrap race has exactly one winner" scenario ADR-0001
// requires, exercised through the real HTTP handler.
func TestBootstrapFlow_ConcurrentCallbackHasExactlyOneWinner(t *testing.T) {
	ts := newMiAuthTestServer(t, miauth.Config{OwnerUsername: "owner"})
	gateID, err := ts.miauth.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/bootstrap/"+gateID, nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	cb, _ := url.Parse(loc.Query().Get("callback"))
	id, state := cb.Query().Get("id"), cb.Query().Get("state")

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return "any-upstream-user", true, nil
	}

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/callback?id="+url.QueryEscape(id)+"&state="+url.QueryEscape(state), nil))
			statuses[i] = r.Code
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, code := range statuses {
		if code == http.StatusOK {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1 (statuses: %v)", winners, statuses)
	}
}
