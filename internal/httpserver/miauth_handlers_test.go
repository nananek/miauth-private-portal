package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func doRequest(ts *miauthTestServer, req *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ts.Handler().ServeHTTP(recorder, req)
	return recorder
}

func startLocalSession(t *testing.T, ts *miauthTestServer, id, permission, callback string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/miauth/" + id + "?permission=" + url.QueryEscape(permission)
	if callback != "" {
		target += "&callback=" + url.QueryEscape(callback)
	}
	return doRequest(ts, httptest.NewRequest(http.MethodGet, target, nil))
}

func TestHandleMiAuthStart_WaitsForSSHApprovalWithoutCallback(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	recorder := startLocalSession(t, ts, "route-1", "read:account,write:notes", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "approve") {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	session, err := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	if err != nil || session.Status != domain.MiAuthCreated {
		t.Fatalf("session = %+v, err = %v", session, err)
	}
}

func TestHandleMiAuthStart_RedirectsImmediatelyToAllowedClientCallback(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	recorder := startLocalSession(t, ts, "route-1", "read:account", "aria://aria/miauth")
	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Scheme != "aria" || location.Query().Get("session") != "route-1" {
		t.Fatalf("Location = %q", location.String())
	}
	session, err := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	if err != nil || session.Status != domain.MiAuthCreated {
		t.Fatalf("redirect incorrectly authorized session: %+v, err=%v", session, err)
	}
}

func TestHandleMiAuthStart_PreservesCallbackQuery(t *testing.T) {
	const callback = "aria://aria/miauth?intent=login"
	cfg := defaultMiAuthTestConfig()
	cfg.ClientCallbacks = []string{callback}
	ts := newMiAuthTestServer(t, cfg)
	recorder := startLocalSession(t, ts, "route-1", "read:account", callback)
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("intent") != "login" || location.Query().Get("session") != "route-1" {
		t.Fatalf("Location = %q", location.String())
	}
}

func TestHandleMiAuthStart_RejectsDisallowedCallback(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	recorder := startLocalSession(t, ts, "route-1", "read:account", "https://evil.example/cb")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

// TestHandleMiAuthStart_NeverReflectsQueryValuesInResponseBody is Issue
// #13 AC8's XSS regression test for the one HTTP response in this
// service that renders anything other than a Misskey-compatible JSON
// body: handleMiAuthStart's waiting/error page. Aria fully controls the
// permission and callback query values, so if either were ever
// interpolated into the response, a crafted value could inject markup.
// Today they never are (writePlainTextPage always renders one of a
// handful of fixed constant strings), and the Content-Type is always
// text/plain, not text/html, so nothing here is ever eligible for
// browser script execution even if that changed. This test pins both
// properties by exercising both the success page (permission-only
// request) and the disallowed-callback error page (a distinct code
// path, ErrClientCallbackNotAllowed) with the same payload.
func TestHandleMiAuthStart_NeverReflectsQueryValuesInResponseBody(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	payload := `<script>alert(document.cookie)</script>`

	recorder := startLocalSession(t, ts, "route-1", payload, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain (a browser never executes script from a non-HTML response)", ct)
	}
	if strings.Contains(recorder.Body.String(), payload) {
		t.Errorf("response body echoed the query-supplied permission value verbatim: %q", recorder.Body.String())
	}

	errRecorder := startLocalSession(t, ts, "route-2", "read:account", payload)
	if errRecorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", errRecorder.Code)
	}
	if ct := errRecorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if strings.Contains(errRecorder.Body.String(), payload) {
		t.Errorf("error response body echoed the query-supplied callback value verbatim: %q", errRecorder.Body.String())
	}
}

func TestRemovedUpstreamRoutesAreNotRegistered(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	// The old bootstrap route has an extra path segment and must no
	// longer resolve. The old literal /miauth/callback path now naturally
	// matches Aria's /miauth/{session} route and has no callback semantics.
	if recorder := doRequest(ts, httptest.NewRequest(http.MethodGet, "/miauth/bootstrap/gate", nil)); recorder.Code != http.StatusNotFound {
		t.Errorf("bootstrap route status = %d, want 404", recorder.Code)
	}
}

func TestHandleMiAuthCheck_ApprovalSuccessAndReplay(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	startLocalSession(t, ts, "route-1", "read:account,write:notes", "")
	pending := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
	var pendingResponse checkFailureResponse
	if err := json.Unmarshal(pending.Body.Bytes(), &pendingResponse); err != nil || pendingResponse.OK {
		t.Fatalf("pending response = %s, err = %v", pending.Body.String(), err)
	}
	if err := ts.miauth.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatal(err)
	}
	approved := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
	var success checkSuccessResponse
	if err := json.Unmarshal(approved.Body.Bytes(), &success); err != nil || !success.OK || success.Token == "" || success.User.Username != "owner" {
		t.Fatalf("approved response = %s, err = %v", approved.Body.String(), err)
	}
	replay := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
	var replayResponse checkFailureResponse
	if err := json.Unmarshal(replay.Body.Bytes(), &replayResponse); err != nil || replayResponse.OK {
		t.Fatalf("replay response = %s, err = %v", replay.Body.String(), err)
	}
	if strings.Contains(ts.logBuf.String(), "route-1") || strings.Contains(ts.logBuf.String(), success.Token) {
		t.Fatal("session ID or API token leaked to logs")
	}
}

func TestHandleMiAuthCheck_ConcurrentCallsHaveExactlyOneWinner(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	startLocalSession(t, ts, "route-1", "read:account", "")
	if err := ts.miauth.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatal(err)
	}
	const count = 8
	results := make(chan bool, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := doRequest(ts, httptest.NewRequest(http.MethodPost, "/api/miauth/route-1/check", strings.NewReader("{}")))
			var response checkFailureResponse
			_ = json.Unmarshal(recorder.Body.Bytes(), &response)
			results <- response.OK
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
}
