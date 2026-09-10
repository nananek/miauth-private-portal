package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// hashAdminSessionToken duplicates internal/webadmin's unexported
// hashSessionToken (same 32-byte-random-token/SHA-256-hash-at-rest shape)
// so this test package can insert an active session row directly without
// depending on internal/webadmin's unexported API surface.
func hashAdminSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// createActiveAdminSession inserts an active WebAdminSession row directly
// for raw's hash, bypassing the WebAuthn ceremony entirely — these tests
// only exercise RequireAdminSession/RequireAdminCSRF's own cookie/session
// lookup, not the login ceremony webadmin_handlers_test.go's dynamic
// vector already covers.
func createActiveAdminSession(t *testing.T, ts *webAdminTestServer, raw, csrfToken string, expiresAt time.Time) domain.WebAdminSession {
	t.Helper()
	hash := hashAdminSessionToken(raw)
	s := domain.WebAdminSession{
		ID: domain.NewID(), OwnerActorID: ts.ownerID, Status: domain.WebAdminSessionActive,
		SessionTokenHash: &hash, CSRFToken: &csrfToken, CreatedAt: time.Now(), ExpiresAt: expiresAt,
	}
	if err := ts.db.WebAdminSessions.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRequireAdminSession_MissingCookieReturns401(t *testing.T) {
	ts := newWebAdminTestServer(t)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) })
	handler := RequireAdminSession(ts.logger, ts.svc)(next)

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("downstream handler was called despite a missing cookie")
	}
}

func TestRequireAdminSession_InvalidOrExpiredSessionReturns401(t *testing.T) {
	ts := newWebAdminTestServer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := RequireAdminSession(ts.logger, ts.svc)(next)

	unknown := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	unknown.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: "not-a-real-token"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, unknown)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token status = %d, want 401", rec.Code)
	}

	createActiveAdminSession(t, ts, "raw-expired", "csrf", time.Now().Add(-time.Hour))
	expired := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	expired.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: "raw-expired"})
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, expired)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expired session status = %d, want 401", rec2.Code)
	}
}

func TestRequireAdminSession_ValidSessionCallsNextWithSessionInContext(t *testing.T) {
	ts := newWebAdminTestServer(t)
	sess := createActiveAdminSession(t, ts, "raw-valid", "csrf-valid", time.Now().Add(time.Hour))

	var gotSession domain.WebAdminSession
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = AdminSessionFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := RequireAdminSession(ts.logger, ts.svc)(next)

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: "raw-valid"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotSession.ID != sess.ID {
		t.Fatalf("AdminSessionFromContext = %+v, want session %s", gotSession, sess.ID)
	}
}

func TestRequireAdminCSRF_MissingOrWrongTokenReturns403EvenWithValidSessionCookie(t *testing.T) {
	ts := newWebAdminTestServer(t)
	createActiveAdminSession(t, ts, "raw-csrf", "correct-csrf-token", time.Now().Add(time.Hour))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := RequireAdminSession(ts.logger, ts.svc)(RequireAdminCSRF(next))

	for _, headerValue := range []string{"", "wrong-token"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
		req.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: "raw-csrf"})
		if headerValue != "" {
			req.Header.Set("X-Admin-CSRF-Token", headerValue)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("headerValue=%q: status = %d, want 403", headerValue, rec.Code)
		}
	}
}

func TestRequireAdminCSRF_CorrectTokenAllowsRequest(t *testing.T) {
	ts := newWebAdminTestServer(t)
	createActiveAdminSession(t, ts, "raw-csrf-ok", "correct-csrf-token", time.Now().Add(time.Hour))

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) })
	handler := RequireAdminSession(ts.logger, ts.svc)(RequireAdminCSRF(next))

	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: "raw-csrf-ok"})
	req.Header.Set("X-Admin-CSRF-Token", "correct-csrf-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !called {
		t.Fatalf("status = %d, called = %v, want 200/true", rec.Code, called)
	}
}
