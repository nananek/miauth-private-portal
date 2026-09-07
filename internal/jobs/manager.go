package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

const (
	queueDepthLogEvery = 10
	transitionTimeout  = 5 * time.Second
	maxLastErrorRunes  = 4096
)

var errUnregisteredHandler = errors.New("no handler registered for job type")

// Config controls worker polling, leases, retries, concurrency, and shutdown.
// Callers normally populate it from internal/config's validated JobsConfig.
type Config struct {
	WorkerID            string
	PollInterval        time.Duration
	ClaimBatchSize      int
	LeaseDuration       time.Duration
	LeaseRenewMargin    time.Duration
	MaxAttempts         int
	BackoffBase         time.Duration
	BackoffMax          time.Duration
	BackoffJitter       float64
	MaxConcurrentJobs   int
	ShutdownGracePeriod time.Duration

	// Reload, if non-nil, is called at the start of every poll (Issue
	// #76 PR4b, ADR-0006 Tier A) to get the current effective values for
	// every field above except WorkerID and BackoffJitter (neither is
	// db-eligible — see internal/config.IsDBEligibleKey). Its result
	// replaces Manager's whole Config for every field it reads from
	// then on, run through withDefaults exactly like a Config built at
	// construction (so LeaseRenewMargin/BackoffMax's existing
	// cross-field clamps still apply even when an operator changes one
	// Tier A key independently of the other through miauthctl config).
	//
	// A job's own lease renewal cadence and retry backoff are decided
	// from whichever Config is current at the moment each decision is
	// made (confirmLease, finish), not frozen at claim time: a
	// long-running job's retry, on failure, uses the latest
	// MaxAttempts/BackoffBase/BackoffMax rather than what was in effect
	// when it was claimed. This is a deliberate simplification over
	// snapshotting Config per job — see reloadConfig's own comment for
	// the one bounded imprecision it accepts (a job's lease-renewal
	// ticker interval, computed once when it starts, does not itself
	// speed up or slow down if LeaseDuration/LeaseRenewMargin change
	// mid-flight, though the lease it renews to always reflects the
	// live LeaseDuration).
	//
	// Nil disables reload entirely: Config's fields never change after
	// Manager is constructed, exactly this type's pre-#76 behavior.
	Reload func(ctx context.Context) Config
}

// Manager polls a durable repository and dispatches claimed jobs to registered
// handlers. Register all handlers before calling Run.
type Manager struct {
	repo     domain.JobRepository
	handlers map[string]Handler
	// cfg is an atomic.Pointer, not a plain Config, because Run's own
	// goroutine can replace it wholesale on every poll (reloadConfig)
	// while job goroutines spawned by earlier polls concurrently read
	// it (confirmLease, finish, ...); config() is the only accessor,
	// returning a consistent snapshot for a caller to read multiple
	// fields from without tearing.
	cfg    atomic.Pointer[Config]
	logger *slog.Logger
	now    func() time.Time
	// running counts in-flight handler goroutines. It replaces what
	// used to be a fixed-capacity semaphore channel (Issue #76 PR4b):
	// a channel's capacity cannot change after creation, which would
	// make MaxConcurrentJobs unreloadable, so poll instead compares
	// this counter against the current Config's MaxConcurrentJobs on
	// every call.
	running atomic.Int64
	// ticker is Run's own ticker, stored here (rather than kept as a
	// local variable in Run) purely so reloadConfig can Reset it; it is
	// nil until Run starts.
	ticker *time.Ticker
}

// NewManager constructs a Manager. Zero values receive conservative defaults
// so tests and small tools cannot accidentally create an invalid ticker; the
// server still validates every operator setting at startup.
func NewManager(repo domain.JobRepository, cfg Config, logger *slog.Logger) *Manager {
	cfg = withDefaults(cfg)
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		repo:     repo,
		handlers: make(map[string]Handler),
		logger:   logger,
		now:      time.Now,
	}
	m.cfg.Store(&cfg)
	return m
}

// config returns a consistent snapshot of Manager's current
// configuration. Callers needing more than one field from it in a single
// logical operation should call this once and read the local copy,
// rather than calling it repeatedly, so a concurrent reloadConfig cannot
// be observed partway through.
func (m *Manager) config() Config {
	return *m.cfg.Load()
}

// Register associates jobType with h. Registration is a startup operation and
// must not race with Run.
func (m *Manager) Register(jobType string, h Handler) {
	if jobType == "" {
		panic("jobs: register empty job type")
	}
	if h == nil {
		panic("jobs: register nil handler")
	}
	m.handlers[jobType] = h
}

// Run blocks until ctx is cancelled. It stops claiming immediately, permits
// in-flight handlers to finish during ShutdownGracePeriod, then cancels and
// requeues any remaining work.
func (m *Manager) Run(ctx context.Context) error {
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorkers()

	var wg sync.WaitGroup
	pollCount := 0

	if ctx.Err() == nil {
		m.poll(ctx, workerCtx, &wg)
		pollCount++
	}

	m.ticker = time.NewTicker(m.config().PollInterval)
	defer m.ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.logger.Info("job worker stopping", "worker_id", m.config().WorkerID)
			m.drain(&wg, cancelWorkers)
			return nil
		case <-m.ticker.C:
			m.reloadConfig(ctx)
			m.poll(ctx, workerCtx, &wg)
			pollCount++
			if pollCount%queueDepthLogEvery == 0 {
				m.logQueueDepth(ctx)
			}
		}
	}
}

// reloadConfig applies cfg.Reload, if set, resetting m.ticker when
// PollInterval actually changed. Split out from Run so the reload
// decision is testable without waiting on a real ticker (mirrors
// internal/ingest.Scheduler.reloadInterval /
// internal/openwebui.CatalogScheduler.reloadInterval).
func (m *Manager) reloadConfig(ctx context.Context) {
	prev := m.config()
	if prev.Reload == nil {
		return
	}
	next := withDefaults(prev.Reload(ctx))
	next.Reload = prev.Reload
	m.cfg.Store(&next)
	if next.PollInterval != prev.PollInterval {
		m.ticker.Reset(next.PollInterval)
	}
}

func (m *Manager) poll(claimCtx, workerCtx context.Context, wg *sync.WaitGroup) {
	cfg := m.config()
	available := cfg.MaxConcurrentJobs - int(m.running.Load())
	if available <= 0 {
		return
	}
	limit := min(available, cfg.ClaimBatchSize)
	now := m.now().UTC()
	// WorkerID alone is not a sufficient fencing value: this same process can
	// reclaim one of its expired leases while the old handler is still winding
	// down, and operators can accidentally reuse an ID across processes. Give
	// every claim operation a new generation so stale transitions cannot match
	// a later lease, even when the human-readable worker identity is unchanged.
	leaseOwner := m.newLeaseOwner()
	claimed, err := m.repo.Claim(claimCtx, leaseOwner, limit, now, now.Add(cfg.LeaseDuration))
	if err != nil {
		if claimCtx.Err() == nil {
			m.logger.Warn("job claim failed", "worker_id", cfg.WorkerID, "error_category", errorCategory(err))
		}
		return
	}
	claimedAt := m.now().UTC()

	for _, job := range claimed {
		m.running.Add(1)
		wg.Add(1)
		queueLatency := claimedAt.Sub(job.NextRunAt)
		if queueLatency < 0 {
			queueLatency = 0
		}
		m.logger.Info("job claimed", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "queue_latency_ms", queueLatency.Milliseconds())
		go func() {
			defer wg.Done()
			defer m.running.Add(-1)
			m.runOne(workerCtx, job)
		}()
	}
}

func (m *Manager) newLeaseOwner() string {
	return m.config().WorkerID + ":" + domain.NewID()
}

func (m *Manager) runOne(parent context.Context, job domain.Job) {
	jobCtx, cancel := context.WithCancel(parent)
	defer cancel()
	// A goroutine can be delayed after Claim long enough for its lease to
	// expire and be reclaimed. Fence that stale dispatch before invoking a
	// handler, whose side effects cannot be undone by a later CAS failure.
	if !m.confirmLease(jobCtx, job, false) {
		return
	}

	result := make(chan error, 1)
	go func() {
		h, ok := m.handlers[job.JobType]
		if !ok {
			result <- errUnregisteredHandler
			return
		}
		result <- h(jobCtx, job)
	}()

	cfg := m.config()
	renewEvery := cfg.LeaseDuration - cfg.LeaseRenewMargin
	renewTicker := time.NewTicker(renewEvery)
	defer renewTicker.Stop()

	for {
		select {
		case err := <-result:
			// Only cancellation of this worker context is a shutdown retry.
			// A handler-owned timeout returned while jobCtx is still live is an
			// ordinary retryable failure and must still obey MaxAttempts.
			if jobCtx.Err() != nil {
				m.retryCancelled(job, boundedErrorOr(err, "worker shutdown cancelled handler"))
				return
			}
			m.finish(jobCtx, job, err)
			return
		case <-jobCtx.Done():
			m.retryCancelled(job, "worker shutdown cancelled handler")
			return
		case <-renewTicker.C:
			if m.confirmLease(jobCtx, job, true) {
				continue
			}
			cancel()
			return
		}
	}
}

func (m *Manager) confirmLease(ctx context.Context, job domain.Job, periodic bool) bool {
	now := m.now().UTC()
	err := m.repo.Renew(ctx, job.ID, claimedLeaseOwner(job), now.Add(m.config().LeaseDuration), now)
	if err == nil {
		if periodic {
			m.logger.Debug("job lease renewed", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt)
		}
		return true
	}

	// Once renewal fails the worker can no longer prove exclusive ownership.
	// Leave the row running so Claim's expiry recovery, rather than this stale
	// worker, chooses its next state. Final transitions independently compare
	// lease ownership, so a reclaim racing handler completion is also fenced.
	category := errorCategory(err)
	if errors.Is(err, domain.ErrConflict) {
		category = "lease_conflict"
	}
	m.logger.Warn("job lease lost", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "error_category", category)
	return false
}

func (m *Manager) finish(ctx context.Context, job domain.Job, handlerErr error) {
	now := m.now().UTC()
	if handlerErr == nil {
		if err := m.repo.Succeed(ctx, job.ID, claimedLeaseOwner(job), now); err != nil {
			m.logTransitionFailure(job, "succeed", err)
			return
		}
		m.logger.Info("job succeeded", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt)
		return
	}

	lastError := boundedError(handlerErr)
	var permanent *PermanentError
	if errors.As(handlerErr, &permanent) {
		if err := m.repo.Fail(ctx, job.ID, claimedLeaseOwner(job), lastError, now); err != nil {
			m.logTransitionFailure(job, "fail", err)
			return
		}
		m.logger.Info("job failed permanently", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "error_category", errorCategory(handlerErr))
		return
	}

	cfg := m.config()
	if job.Attempt+1 >= cfg.MaxAttempts {
		if err := m.repo.Kill(ctx, job.ID, claimedLeaseOwner(job), lastError, now); err != nil {
			m.logTransitionFailure(job, "kill", err)
			return
		}
		m.logger.Info("job retries exhausted", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "error_category", errorCategory(handlerErr))
		return
	}

	m.retry(ctx, job, now.Add(backoff(cfg, job.Attempt)), lastError, errorCategory(handlerErr))
}

func (m *Manager) retry(ctx context.Context, job domain.Job, nextRunAt time.Time, lastError, category string) {
	if err := m.repo.Retry(ctx, job.ID, claimedLeaseOwner(job), nextRunAt, lastError, m.now().UTC()); err != nil {
		m.logTransitionFailure(job, "retry", err)
		return
	}
	m.logger.Info("job scheduled for retry", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt+1, "error_category", category)
}

func claimedLeaseOwner(job domain.Job) string {
	if job.LeaseOwner == nil {
		return ""
	}
	return *job.LeaseOwner
}

func (m *Manager) retryCancelled(job domain.Job, lastError string) {
	ctx, cancel := context.WithTimeout(context.Background(), transitionTimeout)
	defer cancel()
	now := m.now().UTC()
	m.retry(ctx, job, now, lastError, "shutdown")
}

func (m *Manager) drain(wg *sync.WaitGroup, cancelWorkers context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(m.config().ShutdownGracePeriod)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		m.logger.Warn("job worker grace period exceeded; cancelling in-flight jobs", "worker_id", m.config().WorkerID)
		cancelWorkers()
		// runOne performs a bounded, detached Retry before it exits. Waiting
		// here prevents cmd/server from closing the shared DB underneath that
		// final durable transition.
		<-done
	}
}

func (m *Manager) logQueueDepth(ctx context.Context) {
	counts, err := m.repo.CountByState(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Warn("job queue depth query failed", "worker_id", m.config().WorkerID, "error_category", errorCategory(err))
		}
		return
	}
	m.logger.Info("job queue depth",
		"pending", counts[domain.JobPending],
		"running", counts[domain.JobRunning],
		"succeeded", counts[domain.JobSucceeded],
		"failed", counts[domain.JobFailed],
		"dead", counts[domain.JobDead],
	)
}

func (m *Manager) logTransitionFailure(job domain.Job, transition string, err error) {
	m.logger.Warn("job state transition failed", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "transition", transition, "error_category", errorCategory(err))
}

func withDefaults(cfg Config) Config {
	if cfg.WorkerID == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "localhost"
		}
		cfg.WorkerID = fmt.Sprintf("%s:%d", host, os.Getpid())
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.ClaimBatchSize <= 0 {
		cfg.ClaimBatchSize = 10
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.LeaseRenewMargin <= 0 || cfg.LeaseRenewMargin >= cfg.LeaseDuration {
		cfg.LeaseRenewMargin = cfg.LeaseDuration / 3
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 10 * time.Minute
	}
	if cfg.BackoffMax < cfg.BackoffBase {
		cfg.BackoffMax = cfg.BackoffBase
	}
	if cfg.BackoffJitter < 0 || cfg.BackoffJitter > 1 {
		cfg.BackoffJitter = 0.2
	}
	if cfg.MaxConcurrentJobs <= 0 {
		cfg.MaxConcurrentJobs = 4
	}
	if cfg.ShutdownGracePeriod <= 0 {
		cfg.ShutdownGracePeriod = 15 * time.Second
	}
	return cfg
}

func boundedError(err error) string {
	runes := []rune(err.Error())
	if len(runes) > maxLastErrorRunes {
		runes = runes[:maxLastErrorRunes]
	}
	return string(runes)
}

func boundedErrorOr(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return boundedError(err)
}

func errorCategory(err error) string {
	if errors.Is(err, errUnregisteredHandler) {
		return "unregistered_handler"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var permanent *PermanentError
	if errors.As(err, &permanent) {
		return "permanent"
	}
	if errors.Is(err, domain.ErrConflict) {
		return "conflict"
	}
	return "unknown"
}
