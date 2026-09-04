package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	id, state := startLocalSession(t, ts, "route-1", "read:account,write:notes", "")
	if rec := authorizeWithUser(ts, id, state, testAllowedUserID); rec.Code != http.StatusOK {
		t.Fatalf("callback failed: %d %s", rec.Code, rec.Body.String())
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

	handler := RequireScope(ts.miauth, miauth.ScopeReadAccount)(next)
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

	handler := RequireScope(ts.miauth, miauth.ScopeReadAccount)(next)
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

	handler := RequireScope(ts.miauth, miauth.ScopeReadAccount)(next)
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
	handler := RequireScope(ts.miauth, "write:account")(next)
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
	handler := RequireScope(ts.miauth, miauth.ScopeReadAccount)(next)
	req := httptest.NewRequest(http.MethodPost, "/protected", strings.NewReader(`{"i":"`+token+`"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestLocalActorIDFromContext_EmptyWhenUnset(t *testing.T) {
	if got := LocalActorIDFromContext(context.Background()); got != "" {
		t.Errorf("LocalActorIDFromContext() = %q, want empty for a context with no verified actor", got)
	}
}
