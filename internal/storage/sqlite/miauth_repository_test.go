package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func createLocalSession(t *testing.T, db *DB, id string, createdAt time.Time) domain.LocalMiAuthSession {
	t.Helper()
	session := domain.LocalMiAuthSession{
		RouteSessionID: id, Status: domain.MiAuthCreated, RequestedPermissions: "read:account",
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	return session
}

func createOwnerActor(t *testing.T, db *DB, now time.Time) domain.Actor {
	t.Helper()
	actor := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: now}
	if err := db.Actors.Create(t.Context(), actor); err != nil {
		t.Fatal(err)
	}
	return actor
}

func TestLocalMiAuthSessionRepository_StateTransitionsAreAtomic(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := createLocalSession(t, db, "route-1", now)
	owner := createOwnerActor(t, db, now)

	got, err := db.LocalMiAuth.Get(t.Context(), session.RouteSessionID)
	if err != nil || got.Status != domain.MiAuthCreated {
		t.Fatalf("Get = %+v, err = %v", got, err)
	}
	if err := db.LocalMiAuth.Authorize(t.Context(), session.RouteSessionID, owner.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := db.LocalMiAuth.Authorize(t.Context(), session.RouteSessionID, owner.ID, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Authorize error = %v, want ErrConflict", err)
	}
	consumed, err := db.LocalMiAuth.Consume(t.Context(), session.RouteSessionID, now)
	if err != nil || consumed.Status != domain.MiAuthConsumed {
		t.Fatalf("Consume = %+v, err = %v", consumed, err)
	}
	if _, err := db.LocalMiAuth.Consume(t.Context(), session.RouteSessionID, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Consume error = %v, want ErrConflict", err)
	}
}

func TestLocalMiAuthSessionRepository_RejectsExpiredTransitions(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := createLocalSession(t, db, "route-1", now)
	owner := createOwnerActor(t, db, now)
	afterExpiry := now.Add(11 * time.Minute)
	if err := db.LocalMiAuth.Authorize(t.Context(), session.RouteSessionID, owner.ID, afterExpiry); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired Authorize error = %v, want ErrConflict", err)
	}
	if err := db.LocalMiAuth.Deny(t.Context(), session.RouteSessionID, afterExpiry); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired Deny error = %v, want ErrConflict", err)
	}
}

func TestLocalMiAuthSessionRepository_ListPendingFiltersAndOrders(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	createLocalSession(t, db, "older", now.Add(-2*time.Minute))
	createLocalSession(t, db, "newer", now.Add(-time.Minute))
	expired := createLocalSession(t, db, "expired", now.Add(-20*time.Minute))
	rejected := createLocalSession(t, db, "rejected", now)
	if err := db.LocalMiAuth.Deny(t.Context(), rejected.RouteSessionID, now); err != nil {
		t.Fatal(err)
	}
	if expired.ExpiresAt.After(now) {
		t.Fatal("bad test fixture")
	}
	pending, err := db.LocalMiAuth.ListPending(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].RouteSessionID != "newer" || pending[1].RouteSessionID != "older" {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestAPITokenRepository_ListAndRevoke(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)
	for i, id := range []string{"older", "newer"} {
		token := domain.APIToken{
			ID: id, TokenHash: "hash-" + id, LocalActorID: owner.ID,
			Scopes: "read:account", CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}
		if err := db.APITokens.Create(t.Context(), token); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := db.APITokens.List(t.Context())
	if err != nil || len(tokens) != 2 || tokens[0].ID != "newer" {
		t.Fatalf("List = %+v, err = %v", tokens, err)
	}
	if err := db.APITokens.Revoke(t.Context(), "newer", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	tokens, err = db.APITokens.List(t.Context())
	if err != nil || tokens[0].RevokedAt == nil {
		t.Fatalf("revoked List = %+v, err = %v", tokens, err)
	}
}
