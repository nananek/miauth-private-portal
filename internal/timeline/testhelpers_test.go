package timeline

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type testService struct {
	*Service
	db    *sqlite.DB
	clock *fakeClock
	owner domain.Actor
}

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

	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: clock.Now()}
	if err := db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatalf("create owner actor: %v", err)
	}

	return &testService{
		Service: NewService(db, db.Repos, Config{Clock: clock}),
		db:      db,
		clock:   clock,
		owner:   owner,
	}
}

func newTestJob(now time.Time, idempotencyKey *string) domain.Job {
	return domain.Job{
		ID:             domain.NewID(),
		JobType:        "test",
		Payload:        "{}",
		PayloadVersion: 1,
		State:          domain.JobPending,
		IdempotencyKey: idempotencyKey,
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func requireEntryBody(t *testing.T, db *sqlite.DB, entryID, want string) domain.Entry {
	t.Helper()
	entry, err := db.Entries.Get(t.Context(), entryID)
	if err != nil {
		t.Fatalf("get entry %s: %v", entryID, err)
	}
	if entry.Body != want {
		t.Fatalf("entry %s Body = %q, want %q", entryID, entry.Body, want)
	}
	return entry
}
