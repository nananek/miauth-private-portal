package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// doRequest runs req through the server's full handler chain (mux +
// access logging), returning the recorder.
func doRequest(ts *miauthTestServer, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	return rec
}

// startLocalSession issues GET /miauth/{routeSessionID} and returns the
// redirect Location's id and state query parameters (extracted from the
// internal callback URL embedded in the identity-origin redirect's own
// `callback` query parameter), so a test can then drive
// handleMiAuthCallback directly.
func startLocalSession(t *testing.T, ts *miauthTestServer, routeSessionID, permission, callback string) (id, state string) {
	t.Helper()
	target := "/miauth/" + routeSessionID + "?permission=" + url.QueryEscape(permission)
	if callback != "" {
		target += "&callback=" + url.QueryEscape(callback)
	}
	rec := doRequest(ts, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET %s = %d %q, want %d", target, rec.Code, rec.Body.String(), http.StatusFound)
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location header %q: %v", rec.Header().Get("Location"), err)
	}
	if got := loc.Scheme + "://" + loc.Host; got != testIdentityOrigin {
		t.Errorf("redirect origin = %q, want %q", got, testIdentityOrigin)
	}
	if got := loc.Query().Get("permission"); got != upstreamMinimalPermission {
		t.Errorf("upstream permission = %q, want %q (Aria's broad request must never be forwarded)", got, upstreamMinimalPermission)
	}

	cb, err := url.Parse(loc.Query().Get("callback"))
	if err != nil {
		t.Fatalf("parse embedded callback %q: %v", loc.Query().Get("callback"), err)
	}
	if got := cb.Scheme + "://" + cb.Host + cb.Path; got != testLocalOrigin+"/miauth/callback" {
		t.Errorf("internal callback base = %q, want %s/miauth/callback", got, testLocalOrigin)
	}
	return cb.Query().Get("id"), cb.Query().Get("state")
}

func TestHandleMiAuthStart_RedirectsToUpstreamWithMinimalPermission(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account,write:notes,write:drive", "")
	if id == "" || state == "" {
		t.Fatalf("id/state not embedded in redirect: id=%q state=%q", id, state)
	}
}

func TestHandleMiAuthStart_RejectsDisallowedCallback(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	req := httptest.NewRequest(http.MethodGet, "/miauth/route-1?permission=read:account&callback=https://evil.example/cb", nil)
	rec := doRequest(ts, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// authorize drives id/state through handleMiAuthCallback with the
// provider scripted to approve testAllowedUserID, and returns the
// callback response so the caller can assert on it.
func authorizeWithUser(ts *miauthTestServer, id, state, userID string) *httptest.ResponseRecorder {
	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return userID, true, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/miauth/callback?id="+url.QueryEscape(id)+"&state="+url.QueryEscape(state), nil)
	return doRequest(ts, req)
}

func TestHandleMiAuthCallback_SuccessWithoutClientCallbackShowsPage(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account", "")

	rec := authorizeWithUser(ts, id, state, testAllowedUserID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "Aria") {
		t.Errorf("body = %q, expected a return-to-Aria message", rec.Body.String())
	}
}

func TestHandleMiAuthCallback_SuccessWithClientCallbackRedirects(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account", "aria://aria/miauth")

	rec := authorizeWithUser(ts, id, state, testAllowedUserID)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusFound)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "aria://aria/miauth?session=") {
		t.Errorf("Location = %q, want aria://aria/miauth?session=route-1", loc)
	}
	if !strings.Contains(loc, "route-1") {
		t.Errorf("Location = %q, expected the route session ID", loc)
	}
}

func TestHandleMiAuthCallback_PreservesExistingClientCallbackQuery(t *testing.T) {
	const callback = "aria://aria/miauth?intent=login"
	cfg := defaultMiAuthTestConfig()
	cfg.ClientCallbacks = []string{callback}
	ts := newMiAuthTestServer(t, cfg)
	id, state := startLocalSession(t, ts, "route-1", "read:account", callback)

	rec := authorizeWithUser(ts, id, state, testAllowedUserID)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusFound)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := loc.Query().Get("intent"); got != "login" {
		t.Errorf("intent = %q, want login", got)
	}
	if got := loc.Query().Get("session"); got != "route-1" {
		t.Errorf("session = %q, want route-1", got)
	}
}

func TestHandleMiAuthCallback_WrongUserIsGenericFailure(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account", "")

	rec := authorizeWithUser(ts, id, state, "someone-else")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	// The response must never leak the allowlisted ID.
	if strings.Contains(rec.Body.String(), testAllowedUserID) {
		t.Errorf("body leaked the allowlisted user ID: %q", rec.Body.String())
	}
}

func TestHandleMiAuthCallback_WrongStateIsGenericFailure(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, _ := startLocalSession(t, ts, "route-1", "read:account", "")

	rec := authorizeWithUser(ts, id, "wrong-state", testAllowedUserID)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleMiAuthCallback_UnknownIDIsGenericFailure(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	rec := authorizeWithUser(ts, "does-not-exist", "any-state", testAllowedUserID)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleMiAuthCheck_Success(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account,write:notes", "")
	if rec := authorizeWithUser(ts, id, state, testAllowedUserID); rec.Code != http.StatusOK {
		t.Fatalf("callback failed: %d %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}"))
	rec := doRequest(ts, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	var resp checkSuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if !resp.OK {
		t.Fatal("ok = false, want true")
	}
	if resp.Token == "" {
		t.Error("token is empty")
	}
	if resp.User.Username != "owner" {
		t.Errorf("user.username = %q, want owner", resp.User.Username)
	}
	if resp.User.ID == "" {
		t.Error("user.id is empty")
	}
	if resp.User.CreatedAt == "" {
		t.Error("user.createdAt is empty")
	}
}

func TestHandleMiAuthCheck_PendingIsUniformFalse(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	startLocalSession(t, ts, "route-1", "read:account", "")

	req := httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}"))
	rec := doRequest(ts, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (uniform 200 for every non-success outcome)", rec.Code, http.StatusOK)
	}

	var resp checkFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OK {
		t.Error("ok = true, want false for a still-pending session")
	}
}

func TestHandleMiAuthCheck_UnknownSessionIsUniformFalse(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	req := httptest.NewRequest(http.MethodPost, "/api/miauth/does-not-exist/check", strings.NewReader("{}"))
	rec := doRequest(ts, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp checkFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OK {
		t.Error("ok = true, want false")
	}
}

func TestHandleMiAuthCheck_ReplayIsUniformFalse(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account", "")
	if rec := authorizeWithUser(ts, id, state, testAllowedUserID); rec.Code != http.StatusOK {
		t.Fatalf("callback failed: %d", rec.Code)
	}

	first := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
	if first.Code != http.StatusOK {
		t.Fatalf("first check failed: %d", first.Code)
	}
	var firstResp checkSuccessResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil || !firstResp.OK {
		t.Fatalf("first check did not succeed: err=%v body=%s", err, first.Body.String())
	}

	second := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
	var secondResp checkFailureResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if secondResp.OK {
		t.Error("replayed check succeeded a second time")
	}
}

// TestHandleMiAuthCheck_ConcurrentCallsHaveExactlyOneWinner backs
// ADR-0001's "a check that races another check can have only one
// winner" at the HTTP layer, exercising the real handler rather than
// calling Service.Check directly.
func TestHandleMiAuthCheck_ConcurrentCallsHaveExactlyOneWinner(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account", "")
	if rec := authorizeWithUser(ts, id, state, testAllowedUserID); rec.Code != http.StatusOK {
		t.Fatalf("callback failed: %d", rec.Code)
	}

	const n = 8
	var wg sync.WaitGroup
	successes := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
			var resp checkFailureResponse // OK is the only field we need
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			successes[i] = resp.OK
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range successes {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
}

// TestMiAuthFlow_NeverLogsSensitiveValues backs AGENTS.md's logging
// redaction rule and ADR-0001's requirement that the route session ID,
// state, and token never reach a log line, across a full successful
// flow.
func TestMiAuthFlow_NeverLogsSensitiveValues(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	id, state := startLocalSession(t, ts, "route-1", "read:account", "")
	if rec := authorizeWithUser(ts, id, state, testAllowedUserID); rec.Code != http.StatusOK {
		t.Fatalf("callback failed: %d", rec.Code)
	}
	checkRec := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
	var resp checkSuccessResponse
	if err := json.Unmarshal(checkRec.Body.Bytes(), &resp); err != nil || !resp.OK || resp.Token == "" {
		t.Fatalf("check did not succeed: err=%v ok=%v token=%q", err, resp.OK, resp.Token)
	}

	logged := ts.logBuf.String()
	for _, secret := range []string{"route-1", state, resp.Token} {
		if strings.Contains(logged, secret) {
			t.Errorf("log output contains a sensitive value %q:\n%s", secret, logged)
		}
	}
}
