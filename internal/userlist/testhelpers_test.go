package userlist

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type testService struct {
	*Service
	db    *sqlite.DB
	clock *fakeClock
}

// newTestService mirrors internal/timeline's own newTestService: a fresh
// migrated database, a fixed clock, and the reserved actors seeded so
// tests have a valid actors.id to add as a list member.
func newTestService(t *testing.T) *testService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: path, BusyTimeout: 5 * time.Second, MaxOpenConns: 4,
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}

	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return &testService{
		Service: NewService(db.Repos, Config{Clock: clock}),
		db:      db,
		clock:   clock,
	}
}

// mustCreateMemberActor returns a fresh, distinct actor id (Open WebUI
// model, this schema's only non-singleton type) suitable as an
// AddMember/RemoveMember target — mirrors internal/storage/sqlite's own
// mustCreateDistinctActor.
func mustCreateMemberActor(t *testing.T, db *sqlite.DB) string {
	t.Helper()
	a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), a); err != nil {
		t.Fatalf("create member actor: %v", err)
	}
	return a.ID
}
