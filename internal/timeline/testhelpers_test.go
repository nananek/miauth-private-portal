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

// newTestServiceWithOwnerUsername builds a testService the same way
// newTestService does, but with Issue #23 PR5's self-mention detection
// enabled for ownerUsername. It reuses newTestService's DB/owner/clock
// setup and only rebuilds the Service itself, so tests that need mention
// detection do not have to duplicate the rest of the fixture.
func newTestServiceWithOwnerUsername(t *testing.T, ownerUsername string) *testService {
	t.Helper()
	ts := newTestService(t)
	ts.Service = NewService(ts.db, ts.db.Repos, Config{Clock: ts.clock, OwnerUsername: ownerUsername})
	return ts
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

func newTestGeneration(targetEntryID string, kind domain.GenerationKind, at time.Time) domain.LLMGeneration {
	return domain.LLMGeneration{
		ID:            domain.NewID(),
		TargetEntryID: targetEntryID,
		Kind:          kind,
		Provider:      "test-provider",
		Model:         "test-model",
		PromptVersion: "test-v1",
		Status:        domain.GenerationPending,
		RequestedAt:   at,
	}
}

// mustCreateVirtualActor seeds a minimal, active, enabled Open WebUI
// workspace/model pair and returns the actor id of the resulting
// domain.ActorOpenWebUIModel row, for CreateGeneratedReplyBy's
// eligibility tests. It writes the same shape internal/openwebui.
// Registry.Seed would, but directly through the repositories: this
// package does not import internal/openwebui (AGENTS.md's narrow
// use-case boundary applies both ways).
func mustCreateVirtualActor(t *testing.T, db *sqlite.DB, at time.Time) domain.Actor {
	t.Helper()
	actor := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: at}
	if err := db.Actors.Create(t.Context(), actor); err != nil {
		t.Fatalf("create model actor: %v", err)
	}
	workspace := domain.OpenWebUIWorkspace{
		ID: domain.NewID(), Name: "Open WebUI", BaseURL: "https://openwebui.example.net",
		SecretRef: "OPENWEBUI_API_KEY", PresentationHost: "openwebui.example.net",
		Enabled:            true,
		ChatCreateStatus:   domain.CapabilityUnverified,
		ChatContinueStatus: domain.CapabilityUnverified,
		CreatedAt:          at, UpdatedAt: at,
	}
	if err := db.OpenWebUIWorkspaces.Create(t.Context(), workspace); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	model := domain.OpenWebUIModel{
		ID: domain.NewID(), WorkspaceID: workspace.ID, ExternalModelID: "gpt-oss:20b",
		DisplayName: "GPT-OSS 20B", ActorSlug: "model", ActorID: actor.ID, Active: true,
		CreatedAt: at, UpdatedAt: at,
	}
	if err := db.OpenWebUIModels.Create(t.Context(), model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := db.OpenWebUIWorkspaces.SetDefaultModel(t.Context(), workspace.ID, model.ID, at); err != nil {
		t.Fatalf("set default model: %v", err)
	}
	return actor
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
