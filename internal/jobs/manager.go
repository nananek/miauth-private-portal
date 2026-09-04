package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
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
}

// Manager polls a durable repository and dispatches claimed jobs to registered
// handlers. Register all handlers before calling Run.
type Manager struct {
	repo     domain.JobRepository
	handlers map[string]Handler
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time
}

// NewManager constructs a Manager. Zero values receive conservative defaults
// so tests and small tools cannot accidentally create an invalid ticker; the
// server still validates every operator setting at startup.
func NewManager(repo domain.JobRepository, cfg Config, logger *slog.Logger) *Manager {
	cfg = withDefaults(cfg)
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		repo:     repo,
		handlers: make(map[string]Handler),
		cfg:      cfg,
		logger:   logger,
		now:      time.Now,
	}
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

	sem := make(chan struct{}, m.cfg.MaxConcurrentJobs)
	var wg sync.WaitGroup
	pollCount := 0

	if ctx.Err() == nil {
		m.poll(ctx, workerCtx, sem, &wg)
		pollCount++
	}

	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.logger.Info("job worker stopping", "worker_id", m.cfg.WorkerID)
			m.drain(&wg, cancelWorkers)
			return nil
		case <-ticker.C:
			m.poll(ctx, workerCtx, sem, &wg)
			pollCount++
			if pollCount%queueDepthLogEvery == 0 {
				m.logQueueDepth(ctx)
			}
		}
	}
}

func (m *Manager) poll(claimCtx, workerCtx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	available := cap(sem) - len(sem)
	if available <= 0 {
		return
	}
	limit := min(available, m.cfg.ClaimBatchSize)
	now := m.now().UTC()
	claimed, err := m.repo.Claim(claimCtx, m.cfg.WorkerID, limit, now, now.Add(m.cfg.LeaseDuration))
	if err != nil {
		if claimCtx.Err() == nil {
			m.logger.Warn("job claim failed", "worker_id", m.cfg.WorkerID, "error_category", errorCategory(err))
		}
		return
	}
	claimedAt := m.now().UTC()

	for _, job := range claimed {
		sem <- struct{}{}
		wg.Add(1)
		queueLatency := claimedAt.Sub(job.NextRunAt)
		if queueLatency < 0 {
			queueLatency = 0
		}
		m.logger.Info("job claimed", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "queue_latency_ms", queueLatency.Milliseconds())
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			m.runOne(workerCtx, job)
		}()
	}
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

	renewEvery := m.cfg.LeaseDuration - m.cfg.LeaseRenewMargin
	renewTicker := time.NewTicker(renewEvery)
	defer renewTicker.Stop()

	for {
		select {
		case err := <-result:
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
	err := m.repo.Renew(ctx, job.ID, m.cfg.WorkerID, now.Add(m.cfg.LeaseDuration), now)
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
		if err := m.repo.Succeed(ctx, job.ID, m.cfg.WorkerID, now); err != nil {
			m.logTransitionFailure(job, "succeed", err)
			return
		}
		m.logger.Info("job succeeded", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt)
		return
	}

	lastError := boundedError(handlerErr)
	if errors.Is(handlerErr, context.Canceled) || errors.Is(handlerErr, context.DeadlineExceeded) {
		m.retry(ctx, job, now, lastError, "handler_cancelled")
		return
	}

	var permanent *PermanentError
	if errors.As(handlerErr, &permanent) {
		if err := m.repo.Fail(ctx, job.ID, m.cfg.WorkerID, lastError, now); err != nil {
			m.logTransitionFailure(job, "fail", err)
			return
		}
		m.logger.Info("job failed permanently", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "error_category", errorCategory(handlerErr))
		return
	}

	if job.Attempt+1 >= m.cfg.MaxAttempts {
		if err := m.repo.Kill(ctx, job.ID, m.cfg.WorkerID, lastError, now); err != nil {
			m.logTransitionFailure(job, "kill", err)
			return
		}
		m.logger.Info("job retries exhausted", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt, "error_category", errorCategory(handlerErr))
		return
	}

	m.retry(ctx, job, now.Add(backoff(m.cfg, job.Attempt)), lastError, errorCategory(handlerErr))
}

func (m *Manager) retry(ctx context.Context, job domain.Job, nextRunAt time.Time, lastError, category string) {
	if err := m.repo.Retry(ctx, job.ID, m.cfg.WorkerID, nextRunAt, lastError, m.now().UTC()); err != nil {
		m.logTransitionFailure(job, "retry", err)
		return
	}
	m.logger.Info("job scheduled for retry", "job_id", job.ID, "job_type", job.JobType, "attempt", job.Attempt+1, "error_category", category)
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

	timer := time.NewTimer(m.cfg.ShutdownGracePeriod)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		m.logger.Warn("job worker grace period exceeded; cancelling in-flight jobs", "worker_id", m.cfg.WorkerID)
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
			m.logger.Warn("job queue depth query failed", "worker_id", m.cfg.WorkerID, "error_category", errorCategory(err))
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
	if cfg.BackoffMax < cfg.BackoffBase {
		cfg.BackoffMax = 10 * time.Minute
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
