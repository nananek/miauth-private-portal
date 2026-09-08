package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// newTestDB opens a fresh, migrated database backed by a temp file. A real
// file is used rather than ":memory:", which SQLite gives a separate,
// empty database per pooled connection unless carefully configured to
// share one — a footgun ordinary repository tests have no reason to
// invite.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(t.Context(), Config{Path: path, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

// mustCreateActor inserts and returns an actor of the given type, for
// tests that need a valid author_actor_id foreign key target.
func mustCreateActor(t *testing.T, db *DB) string {
	t.Helper()
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}
	a, err := db.Actors.GetByType(t.Context(), "system")
	if err != nil {
		t.Fatalf("get system actor: %v", err)
	}
	return a.ID
}

// mustCreateDistinctActor inserts and returns a fresh actor row, unlike
// mustCreateActor: owner/assistant/system are singletons (only one row
// of each type can ever exist), so a test that needs two genuinely
// different actor IDs — for example, to prove a query correctly scopes
// by owner rather than returning every row regardless — must not call
// mustCreateActor twice and assume the results differ. ActorOpenWebUIModel
// is this schema's only non-singleton type (domain/actor.go), so it is
// what this helper uses; the type itself is otherwise irrelevant to
// callers that only need a valid, distinct actors.id foreign key target.
func mustCreateDistinctActor(t *testing.T, db *DB) string {
	t.Helper()
	a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), a); err != nil {
		t.Fatalf("create distinct actor: %v", err)
	}
	return a.ID
}
