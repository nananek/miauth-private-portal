package openwebui

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// fakeClock is a mutable, test-controlled Clock, mirroring
// internal/timeline's and internal/miauth's identically-named test
// helper.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

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

// testRegistry pairs a Registry under test with the migrated database
// backing it and the clock driving its timestamps, so a test can both
// call through the use-case API and assert on the stored rows directly.
type testRegistry struct {
	*Registry
	db    *sqlite.DB
	clock *fakeClock
}

// validRegistryConfig is the smallest RegistryConfig that Seed accepts.
// Every test below starts from it and overrides only what it is testing,
// the same shape internal/config's validOpenWebUIEnv gives that
// package's tests.
func validRegistryConfig() RegistryConfig {
	return RegistryConfig{
		Enabled:          true,
		BaseURL:          "https://openwebui.example.net",
		SecretRef:        SecretRefAPIKey,
		WorkspaceName:    "Open WebUI",
		PresentationHost: "openwebui.example.net",
		ModelDisplayName: "GPT-OSS 20B",
		ModelSlug:        "model",
		DefaultModelID:   "gpt-oss:20b",
	}
}

// newTestRegistry opens a fresh migrated database and builds a Registry
// against it with cfg. It does not call Seed: tests that need seeded
// rows call it explicitly, so a test of Seed's own behavior is not
// hidden inside setup.
func newTestRegistry(t *testing.T, cfg RegistryConfig) *testRegistry {
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
	return &testRegistry{
		Registry: NewRegistry(db, db.Repos, cfg, clock),
		db:       db,
		clock:    clock,
	}
}

// mustCreateOwner inserts the owner actor and returns its ID, for tests
// exercising the owner-only guard.
func mustCreateOwner(t *testing.T, tr *testRegistry) string {
	t.Helper()
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: tr.clock.Now()}
	if err := tr.db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatalf("create owner actor: %v", err)
	}
	return owner.ID
}
