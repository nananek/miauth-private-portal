package openwebui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestCatalogScheduler_TickEnqueuesOneJob(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}

	scheduler := NewCatalogScheduler(tr.db.Jobs, CatalogSchedulerConfig{Interval: time.Hour}, nil)
	scheduler.tick(t.Context())

	jobs, err := tr.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if jobs[0].JobType != JobTypeCatalogSync {
		t.Errorf("JobType = %q, want %q", jobs[0].JobType, JobTypeCatalogSync)
	}
}

// TestCatalogScheduler_TickWithinSameWindowDoesNotDoubleEnqueue mirrors
// internal/ingest.Scheduler's own idempotency-key coverage: two ticks
// landing in the same truncated interval window must not enqueue a
// second sync job.
func TestCatalogScheduler_TickWithinSameWindowDoesNotDoubleEnqueue(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}

	scheduler := NewCatalogScheduler(tr.db.Jobs, CatalogSchedulerConfig{Interval: time.Hour}, nil)
	scheduler.tick(t.Context())
	scheduler.tick(t.Context())

	jobs, err := tr.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("len(jobs) = %d, want 1 after two ticks in the same window", len(jobs))
	}
}

func TestCatalogScheduler_RunStopsOnCancel(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}

	scheduler := NewCatalogScheduler(tr.db.Jobs, CatalogSchedulerConfig{Interval: time.Hour}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := scheduler.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestCatalogSyncJob_HandleCallsSyncCatalog backs the thin-handler
// contract: Handle is nothing but Registry.SyncCatalog against the
// provider it was built with.
func TestCatalogSyncJob_HandleCallsSyncCatalog(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seededDefault, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	provider := &fakeCatalogProvider{models: []RemoteModel{
		{ID: "gpt-oss:20b", Name: "GPT OSS 20B"},
		{ID: "gpt-oss:120b", Name: "GPT OSS 120B"},
	}}
	job := NewCatalogSyncJob(tr.Registry, provider, nil, nil, nil)
	if err := job.Handle(t.Context(), domain.Job{ID: domain.NewID(), JobType: JobTypeCatalogSync}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, err := tr.db.OpenWebUIModels.Get(t.Context(), seededDefault.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "GPT OSS 20B" {
		t.Errorf("DisplayName = %q, want the synced name (Handle did not call SyncCatalog)", got.DisplayName)
	}
	if _, err := tr.db.OpenWebUIModels.GetByExternalID(t.Context(), workspace.ID, "gpt-oss:120b"); err != nil {
		t.Errorf("the second model was not registered: %v", err)
	}
}

// TestCatalogSyncJob_HandleReturnsProviderError backs internal/jobs.
// Manager's own retry contract: a provider failure must surface as an
// error rather than being swallowed, so jobs' bounded retry/backoff
// actually gets to run it again.
func TestCatalogSyncJob_HandleReturnsProviderError(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}

	job := NewCatalogSyncJob(tr.Registry, &fakeCatalogProvider{err: errors.New("boom")}, nil, nil, nil)
	if err := job.Handle(t.Context(), domain.Job{ID: domain.NewID(), JobType: JobTypeCatalogSync}); err == nil {
		t.Fatal("Handle should return the provider error")
	}
}
