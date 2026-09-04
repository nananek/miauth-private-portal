package miauth

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// fakeClock is a settable Clock for TTL/expiry tests that must not
// depend on real wall-clock timing.
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

// fakeProvider is a func-backed UpstreamProvider fake, so each test can
// script exactly the Check behavior it needs without a real network
// call (AGENTS.md requires normal tests not depend on real credentials
// or network access).
type fakeProvider struct {
	mu    sync.Mutex
	calls int
	check func(ctx context.Context, upstreamSessionID string) (string, bool, error)
}

func (f *fakeProvider) Check(ctx context.Context, upstreamSessionID string) (string, bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.check(ctx, upstreamSessionID)
}

func fixedProvider(userID string, ok bool, err error) *fakeProvider {
	return &fakeProvider{check: func(context.Context, string) (string, bool, error) {
		return userID, ok, err
	}}
}

// testService bundles a Service under test with the fakes and real
// SQLite database backing it, so tests can both call Service methods
// and directly inspect repository state (e.g. to assert a session was
// denied, not merely left pending).
type testService struct {
	*Service
	db       *sqlite.DB
	clock    *fakeClock
	provider *fakeProvider
}

const (
	testIdentityOrigin = "https://misskey.example"
	testAllowedUserID  = "allowed-upstream-user"
)

func newTestService(t *testing.T, cfg Config) *testService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
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

	if cfg.IdentityOrigin == "" {
		cfg.IdentityOrigin = testIdentityOrigin
	}

	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	provider := fixedProvider("", false, nil)

	svc := NewService(db, db.Repos, provider, cfg)
	svc.clock = clock

	return &testService{Service: svc, db: db, clock: clock, provider: provider}
}
