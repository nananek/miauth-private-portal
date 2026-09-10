package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// authedAdminRequest wires up a fully logged-in admin session (a
// registered dynamic WebAuthn credential + a completed login ceremony)
// and returns the session cookie plus its CSRF token, ready to attach to
// any request against the four mutating routes this file tests.
func authedAdminRequest(t *testing.T, ts *webAdminTestServer) (*http.Cookie, string) {
	t.Helper()
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredentialViaHTTP(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("admin-actions-registration-chal")), 0)
	cookie := loginViaHTTP(t, ts, vector, 1)

	indexReq := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	indexReq.AddCookie(cookie)
	indexRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(indexRec, indexReq)
	if indexRec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ status = %d, body = %q", indexRec.Code, indexRec.Body.String())
	}
	return cookie, extractCSRFToken(t, indexRec.Body.String())
}

func postAdminAction(t *testing.T, ts *webAdminTestServer, path string, cookie *http.Cookie, csrfToken string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrfToken != "" {
		req.Header.Set("X-Admin-CSRF-Token", csrfToken)
	}
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	return rec
}

func seedPendingSession(t *testing.T, ts *webAdminTestServer, routeSessionID, requestedPermissions string, clientCallback *string) domain.LocalMiAuthSession {
	t.Helper()
	now := time.Now().UTC()
	session := domain.LocalMiAuthSession{
		RouteSessionID: routeSessionID, Status: domain.MiAuthCreated, RequestedPermissions: requestedPermissions,
		ClientCallback: clientCallback, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := ts.db.LocalMiAuth.Create(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	return session
}

func seedAPIToken(t *testing.T, ts *webAdminTestServer, tokenID, routeSessionID, scopes string) domain.APIToken {
	t.Helper()
	tok := domain.APIToken{
		ID: tokenID, TokenHash: "hash-" + tokenID, LocalActorID: ts.ownerID,
		MiAuthLocalSessionID: &routeSessionID, Scopes: scopes, CreatedAt: time.Now().UTC(),
	}
	if err := ts.db.APITokens.Create(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

// rawAuditRows re-queries web_admin_action_audit through a second raw
// connection to the test DB file — the same cross-package pattern
// internal/miauth/service_test.go and internal/webadmin/audit_test.go
// use, since the repository interface is record-only (no List/Get).
type rawAuditRow struct {
	OwnerActorID, CredentialID, Action, Target, BeforeValue, AfterValue string
}

func rawAuditRows(t *testing.T, ts *webAdminTestServer, target string) []rawAuditRow {
	t.Helper()
	rawDB, err := sql.Open("sqlite", "file:"+ts.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	rows, err := rawDB.QueryContext(context.Background(),
		`SELECT owner_actor_id, COALESCE(credential_id, ''), action, target, COALESCE(before_value, ''), COALESCE(after_value, '')
		 FROM web_admin_action_audit WHERE target = ? ORDER BY changed_at`, target)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rawAuditRow
	for rows.Next() {
		var r rawAuditRow
		if err := rows.Scan(&r.OwnerActorID, &r.CredentialID, &r.Action, &r.Target, &r.BeforeValue, &r.AfterValue); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestHandleAdminIndex_RendersPendingSessionsAndTokens(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, _ := authedAdminRequest(t, ts)
	session := seedPendingSession(t, ts, "route-session-render", "read:account,write:notes", nil)
	tok := seedAPIToken(t, ts, "token-render", "route-session-render", "read:notes")

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, session.RouteSessionID) {
		t.Errorf("body does not contain pending session id %q: %q", session.RouteSessionID, body)
	}
	if !strings.Contains(body, tok.ID) {
		t.Errorf("body does not contain token id %q: %q", tok.ID, body)
	}
}

// TestHandleAdminIndex_EscapesUntrustedRequestedPermissionsAndCallback is
// the security-critical test for this phase. RequestedPermissions is a
// genuinely attacker-reachable field: handleMiAuthStart
// (internal/httpserver/miauth_handlers.go) persists the "permission"
// query param verbatim, with no validation. ClientCallback only reaches
// storage at all if it exactly matches an operator-configured
// ARIA_CLIENT_CALLBACKS entry (internal/miauth.Service.StartLocalSession's
// callbackAllowed check) — it is exercised here as defense-in-depth for
// a misconfigured allowlist or a future, less-restricted write path, not
// because today's HTTP flow lets an attacker put arbitrary markup there.
// Either way, html/template's auto-escaping must actually be engaged for
// both fields, not just assumed.
func TestHandleAdminIndex_EscapesUntrustedRequestedPermissionsAndCallback(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, _ := authedAdminRequest(t, ts)
	maliciousCallback := "https://evil.example/callback\"><script>alert(1)</script>"
	seedPendingSession(t, ts, "route-session-xss", `<script>alert(1)</script>`, &maliciousCallback)

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("raw <script> tag leaked unescaped into response body: %q", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("expected HTML-escaped form of the payload, got: %q", body)
	}
}

func TestHandleAdminSessionsApprove_HappyPath_ApprovesAndRecordsAudit(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)
	seedPendingSession(t, ts, "route-session-approve", "read:account", nil)

	rec := postAdminAction(t, ts, "/admin/sessions/approve", cookie, csrfToken, map[string]string{"routeSessionId": "route-session-approve"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}

	pending, err := ts.db.LocalMiAuth.ListPending(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.RouteSessionID == "route-session-approve" {
			t.Fatalf("session still pending after approve: %+v", p)
		}
	}

	rows := rawAuditRows(t, ts, "route-session-approve")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Action != "approve_session" || got.BeforeValue != "created" || got.AfterValue != "authorized" {
		t.Fatalf("audit row = %+v, want approve_session created->authorized", got)
	}
	if got.CredentialID == "" {
		t.Fatalf("audit row missing credential id: %+v", got)
	}
}

func TestHandleAdminSessionsApprove_AlreadyGoneReturns409(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)
	seedPendingSession(t, ts, "route-session-double-approve", "read:account", nil)

	first := postAdminAction(t, ts, "/admin/sessions/approve", cookie, csrfToken, map[string]string{"routeSessionId": "route-session-double-approve"})
	if first.Code != http.StatusOK {
		t.Fatalf("first approve status = %d, body = %q", first.Code, first.Body.String())
	}
	second := postAdminAction(t, ts, "/admin/sessions/approve", cookie, csrfToken, map[string]string{"routeSessionId": "route-session-double-approve"})
	if second.Code != http.StatusConflict {
		t.Fatalf("second approve status = %d, want 409, body = %q", second.Code, second.Body.String())
	}

	unknown := postAdminAction(t, ts, "/admin/sessions/approve", cookie, csrfToken, map[string]string{"routeSessionId": "route-session-never-existed"})
	if unknown.Code != http.StatusConflict {
		t.Fatalf("unknown session approve status = %d, want 409, body = %q", unknown.Code, unknown.Body.String())
	}
}

func TestHandleAdminSessionsReject_HappyPath_RejectsAndRecordsAudit(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)
	seedPendingSession(t, ts, "route-session-reject", "read:account", nil)

	rec := postAdminAction(t, ts, "/admin/sessions/reject", cookie, csrfToken, map[string]string{"routeSessionId": "route-session-reject"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}

	got, err := ts.db.LocalMiAuth.Get(context.Background(), "route-session-reject")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.MiAuthDenied {
		t.Fatalf("session status = %q, want denied", got.Status)
	}

	rows := rawAuditRows(t, ts, "route-session-reject")
	if len(rows) != 1 || rows[0].Action != "reject_session" || rows[0].BeforeValue != "created" || rows[0].AfterValue != "denied" {
		t.Fatalf("audit rows = %+v, want one reject_session created->denied", rows)
	}
}

func TestHandleAdminTokensRevoke_HappyPath_RevokesAndRecordsAudit(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)
	seedPendingSession(t, ts, "route-session-for-revoke", "read:account", nil)
	tok := seedAPIToken(t, ts, "token-to-revoke", "route-session-for-revoke", "read:notes read:account")

	rec := postAdminAction(t, ts, "/admin/tokens/revoke", cookie, csrfToken, map[string]string{"tokenId": tok.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}

	got, err := ts.db.APITokens.Get(context.Background(), tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt == nil {
		t.Fatal("token not revoked")
	}

	rows := rawAuditRows(t, ts, tok.ID)
	if len(rows) != 1 || rows[0].Action != "revoke_token" {
		t.Fatalf("audit rows = %+v, want one revoke_token", rows)
	}
}

func TestHandleAdminTokensRevoke_UnknownTokenIDReturns404(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)

	rec := postAdminAction(t, ts, "/admin/tokens/revoke", cookie, csrfToken, map[string]string{"tokenId": "does-not-exist"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %q", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminTokensReflectScopes_ChangedTrue_RecordsAuditWithOldNewScopes(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)
	// The session's own requested permissions include write:account, but
	// the token's stored scopes only carry read:notes — reflect-scopes
	// must additively pick write:account up.
	seedPendingSession(t, ts, "route-session-reflect", "write:account", nil)
	tok := seedAPIToken(t, ts, "token-reflect", "route-session-reflect", "read:notes")

	rec := postAdminAction(t, ts, "/admin/tokens/reflect-scopes", cookie, csrfToken, map[string]string{"tokenId": tok.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got struct {
		OK      bool `json:"ok"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !got.Changed {
		t.Fatalf("response = %+v, want ok=true changed=true", got)
	}

	rows := rawAuditRows(t, ts, tok.ID)
	if len(rows) != 1 || rows[0].Action != "reflect_scopes" || rows[0].BeforeValue != "read:notes" {
		t.Fatalf("audit rows = %+v", rows)
	}
	if !strings.Contains(rows[0].AfterValue, "write:account") {
		t.Fatalf("audit AfterValue = %q, want it to contain write:account", rows[0].AfterValue)
	}
}

func TestHandleAdminTokensReflectScopes_NoOpDoesNotRecordAudit(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)
	// Session requests nothing beyond what the token already has, so
	// reflect-scopes is a no-op.
	seedPendingSession(t, ts, "route-session-reflect-noop", "", nil)
	tok := seedAPIToken(t, ts, "token-reflect-noop", "route-session-reflect-noop", "read:notes")

	rec := postAdminAction(t, ts, "/admin/tokens/reflect-scopes", cookie, csrfToken, map[string]string{"tokenId": tok.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got struct {
		OK      bool `json:"ok"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Changed {
		t.Fatalf("response = %+v, want ok=true changed=false", got)
	}

	rows := rawAuditRows(t, ts, tok.ID)
	if len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none on a no-op", rows)
	}
}

func TestHandleAdminTokensReflectScopes_RevokedTokenReturns409(t *testing.T) {
	ts := newWebAdminTestServer(t)
	cookie, csrfToken := authedAdminRequest(t, ts)
	seedPendingSession(t, ts, "route-session-reflect-revoked", "write:account", nil)
	tok := seedAPIToken(t, ts, "token-reflect-revoked", "route-session-reflect-revoked", "read:notes")
	if err := ts.db.APITokens.Revoke(context.Background(), tok.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	rec := postAdminAction(t, ts, "/admin/tokens/reflect-scopes", cookie, csrfToken, map[string]string{"tokenId": tok.ID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %q", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminMutatingRoutes_RequireCSRF(t *testing.T) {
	routes := []struct {
		path string
		body map[string]string
	}{
		{"/admin/sessions/approve", map[string]string{"routeSessionId": "x"}},
		{"/admin/sessions/reject", map[string]string{"routeSessionId": "x"}},
		{"/admin/tokens/revoke", map[string]string{"tokenId": "x"}},
		{"/admin/tokens/reflect-scopes", map[string]string{"tokenId": "x"}},
	}
	for _, rt := range routes {
		t.Run(rt.path, func(t *testing.T) {
			ts := newWebAdminTestServer(t)
			cookie, _ := authedAdminRequest(t, ts)
			rec := postAdminAction(t, ts, rt.path, cookie, "", rt.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403, body = %q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleAdminMutatingRoutes_RequireSession(t *testing.T) {
	routes := []struct {
		path string
		body map[string]string
	}{
		{"/admin/sessions/approve", map[string]string{"routeSessionId": "x"}},
		{"/admin/sessions/reject", map[string]string{"routeSessionId": "x"}},
		{"/admin/tokens/revoke", map[string]string{"tokenId": "x"}},
		{"/admin/tokens/reflect-scopes", map[string]string{"tokenId": "x"}},
	}
	for _, rt := range routes {
		t.Run(rt.path, func(t *testing.T) {
			ts := newWebAdminTestServer(t)
			rec := postAdminAction(t, ts, rt.path, nil, "", rt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401, body = %q", rec.Code, rec.Body.String())
			}
		})
	}
}

// auditFailingRepo wraps the real WebAdminActionAuditRepository so its
// Record call always errors, letting
// TestRecordAdminAction_FailureDoesNotFailTheHTTPResponse prove
// recordAdminAction's failure is swallowed rather than surfaced. called
// is set on every Record call so the test can confirm this decorator —
// not the real repository — is the one the running service actually
// used (see newWebAdminTestServer's mutateRepos doc comment: a
// domain.Repos swap made after service construction is silently
// invisible to it, since the service holds its own copy of the value).
type auditFailingRepo struct {
	domain.WebAdminActionAuditRepository
	called *bool
}

var errAuditWriteFailed = errors.New("simulated audit write failure")

func (r auditFailingRepo) Record(ctx context.Context, entry domain.WebAdminActionAuditEntry) error {
	*r.called = true
	return errAuditWriteFailed
}

func TestRecordAdminAction_FailureDoesNotFailTheHTTPResponse(t *testing.T) {
	var failingRepoCalled bool
	// The failing decorator must be wired into the domain.Repos value
	// BEFORE webadmin.NewService/miauth.NewService run inside
	// newWebAdminTestServer, not swapped in on ts.db.Repos afterwards —
	// see that helper's doc comment.
	ts := newWebAdminTestServer(t, func(r *domain.Repos) {
		r.WebAdminActionAudit = auditFailingRepo{r.WebAdminActionAudit, &failingRepoCalled}
	})
	cookie, csrfToken := authedAdminRequest(t, ts)
	seedPendingSession(t, ts, "route-session-audit-fail", "read:account", nil)

	rec := postAdminAction(t, ts, "/admin/sessions/approve", cookie, csrfToken, map[string]string{"routeSessionId": "route-session-audit-fail"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200 even though the audit write fails", rec.Code, rec.Body.String())
	}
	if !failingRepoCalled {
		t.Fatal("auditFailingRepo.Record was never invoked: the failing repo never reached the running service, so this test did not actually exercise the audit-failure path")
	}
	var got struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.OK {
		t.Fatalf("response = %q, err = %v, want ok=true", rec.Body.String(), err)
	}

	pending, err := ts.db.LocalMiAuth.ListPending(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.RouteSessionID == "route-session-audit-fail" {
			t.Fatalf("session still pending: the underlying action must have genuinely succeeded despite the audit failure")
		}
	}
}
