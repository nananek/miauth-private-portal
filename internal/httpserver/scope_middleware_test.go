package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// testHashToken replicates internal/miauth's unexported hashAPIToken so
// this test can look a minted token up by its stored hash without
// exposing that helper outside its package.
func testHashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// mintToken drives a full MiAuth flow through ts's real handlers and
// returns the raw local API token plus the owner actor ID it was
// issued to.
func mintToken(t *testing.T, ts *miauthTestServer) (token, ownerActorID string) {
	t.Helper()
	if rec := startLocalSession(t, ts, "route-1", "read:account,write:notes", ""); rec.Code != http.StatusOK {
		t.Fatalf("session start failed: %d %s", rec.Code, rec.Body.String())
	}
	if err := ts.miauth.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatalf("ApproveSession: %v", err)
	}
	result, err := ts.miauth.Check(t.Context(), "route-1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return result.Token, result.OwnerActorID
}

func TestRequireScope_AllowsValidTokenAndPreservesBody(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	token, ownerActorID := mintToken(t, ts)

	var gotActorID string
	var gotBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActorID = LocalActorIDFromContext(r.Context())
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	})

	handler := RequireScope(ts.logger, ts.miauth, miauth.ScopeReadAccount)(next)
	req := httptest.NewRequest(http.MethodPost, "/protected", strings.NewReader(`{"i":"`+token+`","other":"field"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotActorID != ownerActorID {
		t.Errorf("LocalActorIDFromContext = %q, want %q", gotActorID, ownerActorID)
	}
	if !strings.Contains(gotBody, "other") {
		t.Errorf("downstream handler did not see the preserved body: %q", gotBody)
	}
}

func TestRequireScope_RejectsMissingToken(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	handler := RequireScope(ts.logger, ts.miauth, miauth.ScopeReadAccount)(next)
	req := httptest.NewRequest(http.MethodPost, "/protected", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if called {
		t.Error("downstream handler was called despite a missing token")
	}
}

func TestRequireScope_RejectsUnknownToken(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	handler := RequireScope(ts.logger, ts.miauth, miauth.ScopeReadAccount)(next)
	req := httptest.NewRequest(http.MethodPost, "/protected", strings.NewReader(`{"i":"not-a-real-token"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRequireScope_RejectsInsufficientScope(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	token, _ := mintToken(t, ts)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	// The token was issued without "write:account" in its request, so a
	// handler requiring it must be rejected even though the token is
	// otherwise valid.
	handler := RequireScope(ts.logger, ts.miauth, "write:account")(next)
	req := httptest.NewRequest(http.MethodPost, "/protected", strings.NewReader(`{"i":"`+token+`"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRequireScope_RejectsRevokedToken(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	token, _ := mintToken(t, ts)

	tok, err := ts.db.APITokens.GetByTokenHash(t.Context(), testHashToken(token))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.db.APITokens.Revoke(t.Context(), tok.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := RequireScope(ts.logger, ts.miauth, miauth.ScopeReadAccount)(next)
	req := httptest.NewRequest(http.MethodPost, "/protected", strings.NewReader(`{"i":"`+token+`"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRequireScope_StorageFailureIsLoggedAndReturnsServerError(t *testing.T) {
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	token, _ := mintToken(t, ts)
	if err := ts.db.Close(); err != nil {
		t.Fatal(err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := RequireScope(ts.logger, ts.miauth, miauth.ScopeReadAccount)(next)
	req := httptest.NewRequest(http.MethodPost, "/protected", strings.NewReader(`{"i":"`+token+`"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(ts.logBuf.String(), "local API token verification failed") {
		t.Errorf("storage failure was not logged: %s", ts.logBuf.String())
	}
	if strings.Contains(ts.logBuf.String(), token) || strings.Contains(rec.Body.String(), token) {
		t.Error("raw API token leaked in log or response")
	}
}

func TestVerifyTokenFromQuery_StorageFailureIsReturnedNotSwallowed(t *testing.T) {
	// verifyTokenFromQuery is handleStreaming's (streaming_handlers.go)
	// only caller; streaming_handlers_test.go exercises it end-to-end
	// through a real WebSocket handshake for the missing/invalid/
	// insufficient-scope cases, but a storage failure is impractical to
	// force through a live listener the way TestRequireScope_
	// StorageFailureIsLoggedAndReturnsServerError does for the JSON-body
	// sibling above. Test it directly instead: it must surface the raw
	// error (for handleStreaming to log and answer with 500), not the
	// sentinel miauth.ErrTokenInvalid a genuinely bad token produces —
	// conflating the two would make handleStreaming answer a storage
	// outage with a misleading 401.
	ts := newMiAuthTestServer(t, defaultMiAuthTestConfig())
	token, _ := mintToken(t, ts)
	if err := ts.db.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/streaming?i="+token, nil)
	if _, err := verifyTokenFromQuery(req.Context(), ts.miauth, req, miauth.ScopeReadAccount); err == nil {
		t.Fatal("expected a storage-failure error")
	} else if errors.Is(err, miauth.ErrTokenInvalid) {
		t.Errorf("storage failure was reported as ErrTokenInvalid, want the underlying error: %v", err)
	}
}

func TestLocalActorIDFromContext_EmptyWhenUnset(t *testing.T) {
	if got := LocalActorIDFromContext(context.Background()); got != "" {
		t.Errorf("LocalActorIDFromContext() = %q, want empty for a context with no verified actor", got)
	}
}
