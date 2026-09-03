package sqlite

import (
	"path/filepath"
	"testing"
	"time"
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
