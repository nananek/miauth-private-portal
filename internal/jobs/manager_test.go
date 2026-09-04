package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func newJobsTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:         filepath.Join(t.TempDir(), "jobs.db"),
		BusyTimeout:  5 * time.Second,
		MaxOpenConns: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func enqueueTestJob(t *testing.T, db *sqlite.DB, jobType, payload string, key *string) domain.Job {
	t.Helper()
	now := time.Now().UTC()
	job := domain.Job{
		ID:             domain.NewID(),
		JobType:        jobType,
		Payload:        payload,
		PayloadVersion: 1,
		State:          domain.JobPending,
		IdempotencyKey: key,
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.Jobs.Enqueue(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func fastConfig(workerID string) Config {
	return Config{
		WorkerID:            workerID,
		PollInterval:        5 * time.Millisecond,
		ClaimBatchSize:      4,
		LeaseDuration:       5 * time.Second,
		LeaseRenewMargin:    2 * time.Second,
		MaxAttempts:         4,
		BackoffBase:         5 * time.Millisecond,
		BackoffMax:          20 * time.Millisecond,
		MaxConcurrentJobs:   4,
		ShutdownGracePeriod: 100 * time.Millisecond,
	}
}

func startManager(t *testing.T, m *Manager) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- m.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, errCh
}

func stopManager(t *testing.T, cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Manager.Run(): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not stop")
	}
}

func waitForJob(t *testing.T, db *sqlite.DB, id string, match func(domain.Job) bool) domain.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := db.Jobs.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if match(job) {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, err := db.Jobs.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("job %s did not reach expected state; final job: %+v", id, job)
	return domain.Job{}
}

func TestManagerProcessesJobAndNeverLogsPayload(t *testing.T) {
	db := newJobsTestDB(t)
	const secretPayload = `{"body":"private learning content"}`
	job := enqueueTestJob(t, db, "test", secretPayload, nil)
	var logBuf bytes.Buffer
	m := NewManager(db.Jobs, fastConfig("worker-1"), logging.New(&logBuf, logging.Config{Format: "json", Level: "debug"}))
	m.Register("test", func(context.Context, domain.Job) error { return nil })
	cancel, errCh := startManager(t, m)

	waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobSucceeded })
	stopManager(t, cancel, errCh)
	if strings.Contains(logBuf.String(), secretPayload) || strings.Contains(logBuf.String(), "private learning content") {
		t.Fatalf("worker logs leaked payload: %s", logBuf.String())
	}
}

func TestManagerRecoversExpiredLeaseAfterCrash(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	now := time.Now().UTC()
	if _, err := db.Jobs.Claim(t.Context(), "crashed-worker", 1, now, now.Add(40*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	m := NewManager(db.Jobs, fastConfig("replacement-worker"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", func(context.Context, domain.Job) error {
		calls.Add(1)
		return nil
	})
	cancel, errCh := startManager(t, m)
	waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobSucceeded })
	stopManager(t, cancel, errCh)
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1 recovery execution", calls.Load())
	}
}

func TestTwoManagersDoNotExecuteSameJobTwice(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	var calls atomic.Int32
	handler := func(context.Context, domain.Job) error {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		return nil
	}

	m1 := NewManager(db.Jobs, fastConfig("worker-1"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m2 := NewManager(db.Jobs, fastConfig("worker-2"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m1.Register("test", handler)
	m2.Register("test", handler)
	cancel1, errCh1 := startManager(t, m1)
	cancel2, errCh2 := startManager(t, m2)
	waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobSucceeded })
	stopManager(t, cancel1, errCh1)
	stopManager(t, cancel2, errCh2)
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want exactly 1", calls.Load())
	}
}

func TestManagerHonorsConcurrencyLimit(t *testing.T) {
	db := newJobsTestDB(t)
	var jobs []domain.Job
	for range 8 {
		jobs = append(jobs, enqueueTestJob(t, db, "test", "{}", nil))
	}
	cfg := fastConfig("worker-1")
	cfg.ClaimBatchSize = 8
	cfg.MaxConcurrentJobs = 2
	var current atomic.Int32
	var maximum atomic.Int32
	m := NewManager(db.Jobs, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", func(context.Context, domain.Job) error {
		running := current.Add(1)
		for {
			observed := maximum.Load()
			if running <= observed || maximum.CompareAndSwap(observed, running) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		current.Add(-1)
		return nil
	})
	cancel, errCh := startManager(t, m)
	for _, job := range jobs {
		waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobSucceeded })
	}
	stopManager(t, cancel, errCh)
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent handlers = %d, want 2", maximum.Load())
	}
}

func TestDuplicateEnqueueRunsOnlyOnce(t *testing.T) {
	db := newJobsTestDB(t)
	key := "same-logical-work"
	job := enqueueTestJob(t, db, "test", "{}", &key)
	now := time.Now().UTC()
	err := db.Jobs.Enqueue(t.Context(), domain.Job{
		ID: domain.NewID(), JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
		IdempotencyKey: &key, NextRunAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate Enqueue() error = %v, want ErrConflict", err)
	}

	var calls atomic.Int32
	m := NewManager(db.Jobs, fastConfig("worker-1"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", func(context.Context, domain.Job) error { calls.Add(1); return nil })
	cancel, errCh := startManager(t, m)
	waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobSucceeded })
	stopManager(t, cancel, errCh)
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1", calls.Load())
	}
}

func TestManagerRenewsLeaseForLongRunningHandler(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	cfg := fastConfig("worker-1")
	cfg.LeaseDuration = 500 * time.Millisecond
	cfg.LeaseRenewMargin = 250 * time.Millisecond

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	handler := func(ctx context.Context, _ domain.Job) error {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m := NewManager(db.Jobs, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", handler)
	cancel, errCh := startManager(t, m)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	running := waitForJob(t, db, job.ID, func(j domain.Job) bool {
		return j.State == domain.JobRunning && j.LeaseExpiresAt != nil
	})
	initialExpiry := *running.LeaseExpiresAt
	waitForJob(t, db, job.ID, func(j domain.Job) bool {
		return j.LeaseExpiresAt != nil && j.LeaseExpiresAt.After(initialExpiry)
	})
	close(release)
	waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobSucceeded })
	stopManager(t, cancel, errCh)
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1", calls.Load())
	}
}

func TestStaleWorkerCannotFinalizeReclaimedJob(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	now := time.Now().UTC()
	claimed, err := db.Jobs.Claim(t.Context(), "worker-1", 1, now, now.Add(time.Millisecond))
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first Claim() = %v, %v", claimed, err)
	}
	reclaimed, err := db.Jobs.Claim(t.Context(), "worker-2", 1, now.Add(time.Second), now.Add(time.Minute))
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaim = %v, %v", reclaimed, err)
	}

	m := NewManager(db.Jobs, fastConfig("worker-1"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", func(context.Context, domain.Job) error { return nil })
	m.runOne(context.Background(), claimed[0])

	got, err := db.Jobs.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobRunning || got.LeaseOwner == nil || *got.LeaseOwner != "worker-2" {
		t.Fatalf("stale worker changed reclaimed job: %+v", got)
	}
}

func TestManagerExhaustsRetriesIntoDead(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	cfg := fastConfig("worker-1")
	cfg.MaxAttempts = 3
	cfg.BackoffBase = time.Millisecond
	cfg.BackoffMax = 2 * time.Millisecond
	var calls atomic.Int32
	m := NewManager(db.Jobs, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", func(context.Context, domain.Job) error {
		calls.Add(1)
		return errors.New("temporary provider failure")
	})
	cancel, errCh := startManager(t, m)
	got := waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobDead })
	stopManager(t, cancel, errCh)
	if calls.Load() != 3 || got.Attempt != 2 {
		t.Fatalf("calls=%d attempt=%d, want calls=3 attempt=2", calls.Load(), got.Attempt)
	}
}

func TestPermanentErrorFailsImmediately(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	var calls atomic.Int32
	var logBuf bytes.Buffer
	m := NewManager(db.Jobs, fastConfig("worker-1"), slog.New(slog.NewTextHandler(&logBuf, nil)))
	m.Register("test", func(context.Context, domain.Job) error {
		calls.Add(1)
		return Permanent(errors.New("invalid payload containing private-provider-detail"))
	})
	cancel, errCh := startManager(t, m)
	got := waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobFailed })
	stopManager(t, cancel, errCh)
	if calls.Load() != 1 || got.Attempt != 0 {
		t.Fatalf("calls=%d attempt=%d, want calls=1 attempt=0", calls.Load(), got.Attempt)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "private-provider-detail") {
		t.Fatalf("LastError = %v, want detailed database error", got.LastError)
	}
	if strings.Contains(logBuf.String(), "private-provider-detail") {
		t.Fatalf("worker log leaked raw handler error: %s", logBuf.String())
	}
}

func TestUnregisteredJobTypeRetriesWithoutPanic(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "future-handler", "{}", nil)
	cfg := fastConfig("worker-1")
	cfg.BackoffBase = time.Hour
	cfg.BackoffMax = time.Hour
	m := NewManager(db.Jobs, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	cancel, errCh := startManager(t, m)
	got := waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobPending && j.Attempt == 1 })
	stopManager(t, cancel, errCh)
	if got.LastError == nil || !strings.Contains(*got.LastError, "no handler") {
		t.Fatalf("LastError = %v, want unregistered-handler detail", got.LastError)
	}
}

func TestManagerGracefulShutdownWaitsForHandler(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	started := make(chan struct{})
	release := make(chan struct{})
	cfg := fastConfig("worker-1")
	cfg.ShutdownGracePeriod = 500 * time.Millisecond
	m := NewManager(db.Jobs, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", func(context.Context, domain.Job) error {
		close(started)
		<-release
		return nil
	})
	cancel, errCh := startManager(t, m)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	close(release)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("manager did not finish graceful shutdown")
	}
	got, err := db.Jobs.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobSucceeded {
		t.Fatalf("State = %q, want succeeded", got.State)
	}
}

func TestManagerForcedShutdownRequeuesAndIncrementsAttempt(t *testing.T) {
	db := newJobsTestDB(t)
	job := enqueueTestJob(t, db, "test", "{}", nil)
	started := make(chan struct{})
	cfg := fastConfig("worker-1")
	cfg.ShutdownGracePeriod = 20 * time.Millisecond
	m := NewManager(db.Jobs, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	m.Register("test", func(ctx context.Context, _ domain.Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	cancel, errCh := startManager(t, m)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	stopManager(t, cancel, errCh)
	got := waitForJob(t, db, job.ID, func(j domain.Job) bool { return j.State == domain.JobPending })
	if got.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1 for shutdown-triggered Retry", got.Attempt)
	}
}

func TestManagerWithNoRegisteredHandlersStartsAndStops(t *testing.T) {
	db := newJobsTestDB(t)
	m := NewManager(db.Jobs, fastConfig("worker-1"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	cancel, errCh := startManager(t, m)
	stopManager(t, cancel, errCh)
}
