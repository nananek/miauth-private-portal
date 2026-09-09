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
	// of Kind and enqueues one poll job per source. It is only the
	// *initial* value: Run reloads it on every tick via
	// ReloadPollInterval, if set.
	PollInterval time.Duration

	// ReloadPollInterval, if non-nil, is called at the start of every
	// tick (Issue #76 PR4a, ADR-0006 Tier A) to get the current
	// effective poll interval; Run calls the ticker's Reset when it
	// differs from the value currently in effect. Nil disables reload
	// entirely: PollInterval never changes after construction, exactly
	// this type's pre-#76 behavior. cmd/server passes a closure reading
	// through internal/configstore.Store, falling back to the same
	// bootstrap PollInterval above; internal/ingest itself has no
	// dependency on internal/configstore.
	ReloadPollInterval func(ctx context.Context) time.Duration

	// DesiredURIs, if non-nil, is called at the start of every tick to
	// get Kind's current full set of configured source URIs, which is
	// then reconciled via ExternalSourceRepository.ReconcileFromConfig
	// before sources are listed for enqueueing — a URI no longer
	// present is deactivated (no longer polled), one newly present is
	// created or reactivated. Nil disables reconciliation entirely: an
	// existing caller unaware of Issue #76's DB overlay keeps exactly
	// its pre-#76 seed-once-at-startup behavior (a feed added or
	// removed from config after startup is simply not reflected until a
	// restart).
	DesiredURIs func(ctx context.Context) []string

	// EnsureActors, if non-nil, is called once per tick, right after
	// DesiredURIs' reconciliation (Issue #134): it provisions a real actor
	// for any source of Kind that does not have one yet — a Kind whose
	// sources are never individually identified this way (imap) simply
	// leaves this nil, the same "nil disables the feature entirely"
	// convention DesiredURIs itself uses. A failure here is logged and
	// swallowed, exactly like a ReconcileFromConfig or List failure below:
	// a source without an actor yet must still be polled (it just keeps
	// authoring as the fallback system actor until the next successful
	// tick), never skipped.
	EnsureActors func(ctx context.Context) error
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
	// interval is the poll interval currently in effect: cfg.PollInterval
	// until ReloadPollInterval (if set) reports a different value. Only
	// Run's own goroutine ever reads or writes it, so it needs no lock.
	interval time.Duration
	// ticker is Run's own ticker, stored here (rather than kept as a
	// local variable in Run) purely so reloadInterval can Reset it; it
	// is nil until Run starts.
	ticker *time.Ticker
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
	return &Scheduler{sources: sources, jobsRepo: jobsRepo, cfg: cfg, logger: logger, now: time.Now, interval: cfg.PollInterval}
}

// Run enqueues one poll job per configured source immediately, then
// again every interval (initially cfg.PollInterval, reloaded via
// cfg.ReloadPollInterval — see SchedulerConfig's own doc comment), until
// ctx is cancelled. It never returns a non-nil error except through ctx
// cancellation reporting nil (mirroring internal/jobs.Manager.Run's own
// "blocks until ctx is cancelled, then returns nil" contract, so
// cmd/server's shared errCh/wg shutdown handling treats every
// long-running service identically).
func (s *Scheduler) Run(ctx context.Context) error {
	s.tick(ctx)

	s.ticker = time.NewTicker(s.interval)
	defer s.ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.ticker.C:
			s.reloadInterval(ctx)
			s.tick(ctx)
		}
	}
}

// reloadInterval applies cfg.ReloadPollInterval, if set, resetting
// s.ticker when the effective interval actually changed. Split out from
// Run so the reload decision is testable without waiting on a real
// ticker.
func (s *Scheduler) reloadInterval(ctx context.Context) {
	if s.cfg.ReloadPollInterval == nil {
		return
	}
	if next := s.cfg.ReloadPollInterval(ctx); next > 0 && next != s.interval {
		s.interval = next
		s.ticker.Reset(s.interval)
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	if s.cfg.DesiredURIs != nil {
		uris := s.cfg.DesiredURIs(ctx)
		if err := s.sources.ReconcileFromConfig(ctx, s.cfg.Kind, uris, s.now().UTC()); err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("ingest scheduler: reconcile sources failed", "error_category", "storage_error")
			}
			// Reconciliation failing (a transient storage error) must
			// not skip enqueueing jobs for whatever sources already
			// exist — fall through to List/enqueue below regardless.
		}
	}

	if s.cfg.EnsureActors != nil {
		if err := s.cfg.EnsureActors(ctx); err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("ingest scheduler: ensure source actors failed", "error_category", "storage_error")
			}
			// Best-effort, like the reconcile failure above: must not
			// skip enqueueing jobs for sources that already have a
			// working actor (or, for that matter, ones that don't yet
			// but must still be polled).
		}
	}

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
	window := now.Truncate(s.interval).Unix()

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
