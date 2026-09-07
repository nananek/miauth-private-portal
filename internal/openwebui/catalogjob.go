// This file is Issue #75 PR4's durable-job side of catalog sync:
// CatalogSyncJob is the thin internal/jobs.Manager handler wrapping
// Registry.SyncCatalog (catalog.go), and CatalogScheduler is what
// periodically enqueues one such job — the same "a scheduler ticks, a
// job actually does the work" split internal/ingest.Scheduler/Service
// already use for RSS/IMAP polling, reduced to this feature's simpler
// shape: one sync round per tick, not one job per configured source.
package openwebui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// JobTypeCatalogSync is the durable job type CatalogSyncJob registers
// against and CatalogScheduler enqueues.
const JobTypeCatalogSync = "openwebui_catalog_sync"

// CatalogSyncJob adapts Registry.SyncCatalog to internal/jobs.Handler.
// It carries no retry logic of its own: a failure is returned to
// internal/jobs.Manager, which already has bounded retry/backoff for
// every job type, so a transient provider outage is retried on jobs' own
// schedule rather than this package growing a second one.
type CatalogSyncJob struct {
	registry     *Registry
	provider     CatalogProvider
	toolCache    *ToolConfigCache
	featureCache *FeatureDefaultCache
	logger       *slog.Logger
}

// NewCatalogSyncJob builds a CatalogSyncJob. toolCache/featureCache may
// each be nil to skip that half of per-model resolution entirely (see
// Registry.SyncCatalog); a nil logger defaults to slog.Default().
func NewCatalogSyncJob(registry *Registry, provider CatalogProvider, toolCache *ToolConfigCache, featureCache *FeatureDefaultCache, logger *slog.Logger) *CatalogSyncJob {
	if logger == nil {
		logger = slog.Default()
	}
	return &CatalogSyncJob{registry: registry, provider: provider, toolCache: toolCache, featureCache: featureCache, logger: logger}
}

// Handle implements internal/jobs.Handler.
func (j *CatalogSyncJob) Handle(ctx context.Context, job domain.Job) error {
	result, err := j.registry.SyncCatalog(ctx, j.provider, j.toolCache, j.featureCache, j.logger)
	if err != nil {
		return fmt.Errorf("openwebui: catalog sync job: %w", err)
	}
	j.logger.Info("openwebui catalog sync succeeded",
		"created", result.Created, "reactivated", result.Reactivated,
		"updated", result.Updated, "deactivated", result.Deactivated)
	return nil
}

// CatalogSchedulerConfig bounds CatalogScheduler's ticking interval.
type CatalogSchedulerConfig struct {
	// Interval is how often CatalogScheduler enqueues a JobTypeCatalogSync
	// job. Mirrors OPENWEBUI_CATALOG_SYNC_INTERVAL.
	Interval time.Duration
}

// CatalogScheduler periodically enqueues one JobTypeCatalogSync job — a
// reduced internal/ingest.Scheduler: that type lists one
// domain.ExternalSource per configured feed/mailbox and enqueues a job
// per source, but catalog sync has no per-source concept at all (one
// account, one GET /api/models call covers every model), so this ticks
// and enqueues exactly one job per interval instead.
type CatalogScheduler struct {
	jobsRepo domain.JobRepository
	cfg      CatalogSchedulerConfig
	logger   *slog.Logger
	now      func() time.Time
}

// NewCatalogScheduler builds a CatalogScheduler. A non-positive Interval
// defaults to 10 minutes (the same "never busy-loop from a zero-value
// config" guard internal/ingest.NewScheduler applies); cmd/server always
// passes a validated, positive internal/config value. A nil logger
// defaults to slog.Default().
func NewCatalogScheduler(jobsRepo domain.JobRepository, cfg CatalogSchedulerConfig, logger *slog.Logger) *CatalogScheduler {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &CatalogScheduler{jobsRepo: jobsRepo, cfg: cfg, logger: logger, now: time.Now}
}

// Run enqueues one sync job immediately, then again every Interval,
// until ctx is cancelled — mirroring internal/ingest.Scheduler.Run's
// "blocks until cancelled, then returns nil" contract, so cmd/server's
// shared errCh/wg shutdown handling treats every long-running service
// identically.
func (s *CatalogScheduler) Run(ctx context.Context) error {
	s.tick(ctx)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick enqueues one JobTypeCatalogSync job, idempotency-keyed to the
// current interval window so a scheduler restart within the same window
// collides on Enqueue rather than double-enqueueing a redundant sync
// (mirrors internal/ingest.Scheduler.tick's own window truncation).
func (s *CatalogScheduler) tick(ctx context.Context) {
	now := s.now().UTC()
	window := now.Truncate(s.cfg.Interval).Unix()
	idempotencyKey := fmt.Sprintf("%s:%d", JobTypeCatalogSync, window)
	job := domain.Job{
		ID:             domain.NewID(),
		JobType:        JobTypeCatalogSync,
		Payload:        "{}",
		PayloadVersion: 1,
		State:          domain.JobPending,
		IdempotencyKey: &idempotencyKey,
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.jobsRepo.Enqueue(ctx, job); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return
		}
		s.logger.Warn("openwebui catalog scheduler: enqueue sync job failed", "error_category", "storage_error")
	}
}
