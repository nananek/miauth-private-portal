// This file is Issue #77 PR7's durable-job side of Service.RunOrphanGC:
// OrphanGCJob is the thin internal/jobs.Manager handler wrapping it, and
// GCScheduler is what periodically enqueues one such job — the same "a
// scheduler ticks, a job actually does the work" split
// internal/openwebui.CatalogScheduler/CatalogSyncJob already use for
// catalog sync (itself likewise a single global sweep, not one job per
// configured entity).
package drive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// JobTypeOrphanGC is the durable job type OrphanGCJob registers against
// and GCScheduler enqueues.
const JobTypeOrphanGC = "drive_orphan_gc"

// OrphanGCJob adapts Service.RunOrphanGC to internal/jobs.Handler. It
// carries no retry logic of its own: a failure is returned to
// internal/jobs.Manager, which already has bounded retry/backoff for
// every job type, so a transient storage-backend outage is retried on
// jobs' own schedule rather than this package growing a second one —
// and the next scheduled sweep would catch the same orphan anyway even
// if every retry of this run failed.
type OrphanGCJob struct {
	service *Service
	logger  *slog.Logger
}

// NewOrphanGCJob builds an OrphanGCJob. A nil logger defaults to
// slog.Default().
func NewOrphanGCJob(service *Service, logger *slog.Logger) *OrphanGCJob {
	if logger == nil {
		logger = slog.Default()
	}
	return &OrphanGCJob{service: service, logger: logger}
}

// Handle implements internal/jobs.Handler.
func (j *OrphanGCJob) Handle(ctx context.Context, job domain.Job) error {
	deleted, err := j.service.RunOrphanGC(ctx)
	if err != nil {
		// A partial failure (some orphans deleted, some delete calls
		// failed) still returns deleted > 0 alongside a non-nil err —
		// reported here, not treated as if nothing happened, since jobs
		// only sees Handle's single return value.
		j.logger.Warn("drive orphan gc completed with errors", "deleted", deleted, "error", err.Error())
		return fmt.Errorf("drive: orphan gc: %w", err)
	}
	j.logger.Info("drive orphan gc succeeded", "deleted", deleted)
	return nil
}

// GCSchedulerConfig bounds GCScheduler's ticking interval.
type GCSchedulerConfig struct {
	// Interval is how often GCScheduler enqueues a JobTypeOrphanGC job.
	// Mirrors DRIVE_ORPHAN_GC_INTERVAL.
	Interval time.Duration
}

// GCScheduler periodically enqueues one JobTypeOrphanGC job — a sweep
// has no per-entity concept (it enumerates the whole configured backend
// each time), so, like CatalogScheduler, this ticks and enqueues exactly
// one job per interval rather than one job per something.
type GCScheduler struct {
	jobsRepo domain.JobRepository
	cfg      GCSchedulerConfig
	logger   *slog.Logger
	now      func() time.Time
}

// NewGCScheduler builds a GCScheduler. A non-positive Interval defaults
// to 24 hours — orphan GC only exists to reclaim disk/bucket space a
// rare best-effort-cleanup failure left behind (see RunOrphanGC's own
// doc comment), not a correctness-critical sweep, so an infrequent
// default is deliberately chosen over a busy-looping one; cmd/server
// always passes a validated, positive internal/config value. A nil
// logger defaults to slog.Default().
func NewGCScheduler(jobsRepo domain.JobRepository, cfg GCSchedulerConfig, logger *slog.Logger) *GCScheduler {
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GCScheduler{jobsRepo: jobsRepo, cfg: cfg, logger: logger, now: time.Now}
}

// Run enqueues one orphan-gc job immediately, then again every Interval,
// until ctx is cancelled — mirroring internal/ingest.Scheduler.Run's and
// CatalogScheduler.Run's own "blocks until cancelled, then returns nil"
// contract, so cmd/server's shared errCh/wg shutdown handling treats
// every long-running service identically.
func (s *GCScheduler) Run(ctx context.Context) error {
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

// tick enqueues one JobTypeOrphanGC job, idempotency-keyed to the
// current interval window so a scheduler restart within the same window
// collides on Enqueue rather than double-enqueueing a redundant sweep
// (mirrors CatalogScheduler.tick's own window truncation).
func (s *GCScheduler) tick(ctx context.Context) {
	now := s.now().UTC()
	window := now.Truncate(s.cfg.Interval).Unix()
	idempotencyKey := fmt.Sprintf("%s:%d", JobTypeOrphanGC, window)
	job := domain.Job{
		ID:             domain.NewID(),
		JobType:        JobTypeOrphanGC,
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
		s.logger.Warn("drive gc scheduler: enqueue orphan gc job failed", "error_category", "storage_error")
	}
}
