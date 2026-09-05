package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// SchedulerConfig bounds Scheduler's polling interval and scopes it to one
// domain.ExternalSource.Kind.
type SchedulerConfig struct {
	// Kind is the domain.ExternalSource.Kind this Scheduler instance
	// polls (for example "rss" or "imap"). A Scheduler only ever lists
	// and enqueues jobs for sources of this kind: cmd/server runs one
	// Scheduler instance per enabled kind, each with its own
	// PollInterval, and this field is what keeps them from
	// double-enqueueing each other's sources (see
	// domain.ExternalSourceRepository.List's doc comment).
	Kind string
	// PollInterval is how often Scheduler re-lists configured sources
	// of Kind and enqueues one poll job per source.
	PollInterval time.Duration
}

// Scheduler periodically enqueues one JobType job per configured
// domain.ExternalSource of its configured Kind. internal/jobs.Manager
// itself has no periodic-scheduling primitive (every other job producer
// enqueues in reaction to a user action), so this package adds the small
// amount of ticking logic Issue #11's "poll a feed every N minutes"
// requirement needs, without changing internal/jobs.
type Scheduler struct {
	sources  domain.ExternalSourceRepository
	jobsRepo domain.JobRepository
	cfg      SchedulerConfig
	logger   *slog.Logger
	now      func() time.Time
}

// NewScheduler builds a Scheduler. Zero-value PollInterval defaults to
// 15 minutes so a hand-built SchedulerConfig in a test cannot
// accidentally create a busy-looping ticker; cmd/server always passes a
// validated internal/config.RSSConfig.PollInterval.
func NewScheduler(sources domain.ExternalSourceRepository, jobsRepo domain.JobRepository, cfg SchedulerConfig, logger *slog.Logger) *Scheduler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{sources: sources, jobsRepo: jobsRepo, cfg: cfg, logger: logger, now: time.Now}
}

// Run enqueues one poll job per configured source immediately, then
// again every PollInterval, until ctx is cancelled. It never returns a
// non-nil error except through ctx cancellation reporting nil (mirroring
// internal/jobs.Manager.Run's own "blocks until ctx is cancelled, then
// returns nil" contract, so cmd/server's shared errCh/wg shutdown
// handling treats every long-running service identically).
func (s *Scheduler) Run(ctx context.Context) error {
	s.tick(ctx)

	ticker := time.NewTicker(s.cfg.PollInterval)
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

func (s *Scheduler) tick(ctx context.Context) {
	sources, err := s.sources.List(ctx, s.cfg.Kind)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("ingest scheduler: list sources failed", "error_category", "storage_error")
		}
		return
	}

	now := s.now().UTC()
	// Truncating to the poll interval makes every tick within the same
	// interval window compute the same idempotency key, so a scheduler
	// restart (or, in a future multi-instance deployment, a second
	// scheduler) enqueuing at a slightly different wall-clock moment
	// within the same window still collides on Enqueue rather than
	// double-enqueueing a job for the same source and interval.
	window := now.Truncate(s.cfg.PollInterval).Unix()

	for _, source := range sources {
		payload, err := NewJobPayload(source.ID)
		if err != nil {
			s.logger.Warn("ingest scheduler: encode job payload failed", "source_id", source.ID)
			continue
		}
		idempotencyKey := fmt.Sprintf("%s:%s:%d", JobType, source.ID, window)
		job := domain.Job{
			ID:             domain.NewID(),
			JobType:        JobType,
			Payload:        payload,
			PayloadVersion: 1,
			State:          domain.JobPending,
			IdempotencyKey: &idempotencyKey,
			NextRunAt:      now,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := s.jobsRepo.Enqueue(ctx, job); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				continue
			}
			s.logger.Warn("ingest scheduler: enqueue poll job failed", "source_id", source.ID, "error_category", "storage_error")
		}
	}
}
