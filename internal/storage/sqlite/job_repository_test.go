package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func mustEnqueueJob(t *testing.T, db *DB, idempotencyKey *string, nextRunAt time.Time) domain.Job {
	t.Helper()
	now := time.Now()
	j := domain.Job{
		ID: domain.NewID(), JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
		IdempotencyKey: idempotencyKey, NextRunAt: nextRunAt, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Jobs.Enqueue(t.Context(), j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return j
}

func TestJobRepository_EnqueueAndGet(t *testing.T) {
	db := newTestDB(t)
	j := mustEnqueueJob(t, db, nil, time.Now())

	got, err := db.Jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobPending {
		t.Errorf("State = %q, want pending", got.State)
	}
}

func TestJobRepository_Enqueue_RejectsDuplicateIdempotencyKey(t *testing.T) {
	db := newTestDB(t)
	key := "same-key"
	mustEnqueueJob(t, db, &key, time.Now())

	dup := domain.Job{
		ID: domain.NewID(), JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
		IdempotencyKey: &key, NextRunAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	err := db.Jobs.Enqueue(t.Context(), dup)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Enqueue() error = %v, want ErrConflict", err)
	}
}

func TestJobRepository_Enqueue_AllowsMultipleNilIdempotencyKeys(t *testing.T) {
	db := newTestDB(t)
	mustEnqueueJob(t, db, nil, time.Now())
	mustEnqueueJob(t, db, nil, time.Now())
}

func TestJobRepository_Claim_LeasesDuePendingJobsInOrder(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	a := mustEnqueueJob(t, db, nil, now)
	b := mustEnqueueJob(t, db, nil, now.Add(time.Minute))
	_ = mustEnqueueJob(t, db, nil, now.Add(time.Hour)) // not yet due

	claimed, err := db.Jobs.Claim(t.Context(), "worker-1", 10, now.Add(2*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("len(claimed) = %d, want 2", len(claimed))
	}
	if claimed[0].ID != a.ID || claimed[1].ID != b.ID {
		t.Errorf("claimed order = [%s, %s], want [%s, %s]", claimed[0].ID, claimed[1].ID, a.ID, b.ID)
	}
	for _, j := range claimed {
		if j.State != domain.JobRunning {
			t.Errorf("job %s state = %q, want running", j.ID, j.State)
		}
		if j.LeaseOwner == nil || *j.LeaseOwner != "worker-1" {
			t.Errorf("job %s LeaseOwner = %v, want worker-1", j.ID, j.LeaseOwner)
		}
	}

	// A second claim right away must not re-claim jobs already leased and
	// not yet expired.
	again, err := db.Jobs.Claim(t.Context(), "worker-2", 10, now.Add(2*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("second claim returned %d jobs, want 0", len(again))
	}
}

// TestJobRepository_Claim_OrdersDueJobsByNextRunAtAscendingRegardlessOfEnqueueOrder
// backs Claim's documented ordering guarantee: SQLite's RETURNING clause
// does not itself promise any row order, so this enqueues jobs in a
// scrambled order and checks Claim still returns them sorted by
// next_run_at ascending.
func TestJobRepository_Claim_OrdersDueJobsByNextRunAtAscendingRegardlessOfEnqueueOrder(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	offsets := []time.Duration{4 * time.Minute, 1 * time.Minute, 5 * time.Minute, 0, 3 * time.Minute, 2 * time.Minute}
	jobs := make([]domain.Job, len(offsets))
	for i, off := range offsets {
		jobs[i] = mustEnqueueJob(t, db, nil, now.Add(off))
	}

	claimed, err := db.Jobs.Claim(t.Context(), "worker-1", 10, now.Add(time.Hour), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != len(offsets) {
		t.Fatalf("len(claimed) = %d, want %d", len(claimed), len(offsets))
	}

	// Enqueue order was offsets 4,1,5,0,3,2 minutes; ascending next_run_at
	// order is the jobs at indices 3,1,5,4,0,2.
	wantOrder := []string{jobs[3].ID, jobs[1].ID, jobs[5].ID, jobs[4].ID, jobs[0].ID, jobs[2].ID}
	for i, want := range wantOrder {
		if claimed[i].ID != want {
			t.Errorf("claimed[%d].ID = %s, want %s", i, claimed[i].ID, want)
		}
	}
}

// TestJobRepository_Claim_TiesBreakByIDAscending backs Claim's tie-break
// rule for jobs sharing the same next_run_at: without a deterministic
// tie-break, jobs at the back of an arbitrary return order could starve.
// The job IDs are enqueued out of order so a correct implementation must
// actually sort by ID rather than happen to match insertion order.
func TestJobRepository_Claim_TiesBreakByIDAscending(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	ids := []string{"job-c", "job-a", "job-e", "job-b", "job-d"}
	for _, id := range ids {
		j := domain.Job{
			ID: id, JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
			NextRunAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Jobs.Enqueue(t.Context(), j); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}

	claimed, err := db.Jobs.Claim(t.Context(), "worker-1", 10, now, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"job-a", "job-b", "job-c", "job-d", "job-e"}
	if len(claimed) != len(want) {
		t.Fatalf("len(claimed) = %d, want %d", len(claimed), len(want))
	}
	for i, id := range want {
		if claimed[i].ID != id {
			t.Errorf("claimed[%d].ID = %s, want %s", i, claimed[i].ID, id)
		}
	}
}

func TestJobRepository_Claim_ReclaimsExpiredLease(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	j := mustEnqueueJob(t, db, nil, now)

	if _, err := db.Jobs.Claim(t.Context(), "worker-1", 10, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// The lease has now expired; a later claim should reclaim it (a
	// crashed worker's abandoned job must be recoverable).
	reclaimed, err := db.Jobs.Claim(t.Context(), "worker-2", 10, now.Add(2*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != j.ID {
		t.Fatalf("reclaimed = %v, want [%s]", reclaimed, j.ID)
	}
	if reclaimed[0].LeaseOwner == nil || *reclaimed[0].LeaseOwner != "worker-2" {
		t.Errorf("LeaseOwner = %v, want worker-2", reclaimed[0].LeaseOwner)
	}
}

func TestJobRepository_Renew(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	j := mustEnqueueJob(t, db, nil, now)
	if _, err := db.Jobs.Claim(t.Context(), "worker-1", 1, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	wantExpiry := now.Add(2 * time.Minute)
	if err := db.Jobs.Renew(t.Context(), j.ID, "worker-1", wantExpiry, now.Add(30*time.Second)); err != nil {
		t.Fatalf("Renew(): %v", err)
	}
	got, err := db.Jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(wantExpiry) {
		t.Errorf("LeaseExpiresAt = %v, want %v", got.LeaseExpiresAt, wantExpiry)
	}

	if err := db.Jobs.Renew(t.Context(), j.ID, "worker-2", wantExpiry, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Renew() with wrong owner error = %v, want ErrConflict", err)
	}
	if err := db.Jobs.Succeed(t.Context(), j.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Jobs.Renew(t.Context(), j.ID, "worker-1", wantExpiry, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Renew() after success error = %v, want ErrConflict", err)
	}
}

func TestJobRepository_Succeed(t *testing.T) {
	db := newTestDB(t)
	j := mustEnqueueJob(t, db, nil, time.Now())

	if err := db.Jobs.Succeed(t.Context(), j.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := db.Jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobSucceeded {
		t.Errorf("State = %q, want succeeded", got.State)
	}
}

func TestJobRepository_Retry_IncrementsAttemptAndClearsLease(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	j := mustEnqueueJob(t, db, nil, now)
	if _, err := db.Jobs.Claim(t.Context(), "worker-1", 10, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	nextRun := now.Add(5 * time.Minute)
	if err := db.Jobs.Retry(t.Context(), j.ID, nextRun, "transient failure", now); err != nil {
		t.Fatal(err)
	}

	got, err := db.Jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobPending {
		t.Errorf("State = %q, want pending", got.State)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", got.Attempt)
	}
	if got.LeaseOwner != nil {
		t.Errorf("LeaseOwner = %v, want nil", got.LeaseOwner)
	}
	if got.LastError == nil || *got.LastError != "transient failure" {
		t.Errorf("LastError = %v, want \"transient failure\"", got.LastError)
	}
}

func TestJobRepository_Kill(t *testing.T) {
	db := newTestDB(t)
	j := mustEnqueueJob(t, db, nil, time.Now())

	if err := db.Jobs.Kill(t.Context(), j.ID, "retries exhausted", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := db.Jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobDead {
		t.Errorf("State = %q, want dead", got.State)
	}
}

func TestJobRepository_Fail(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	j := mustEnqueueJob(t, db, nil, now)
	if _, err := db.Jobs.Claim(t.Context(), "worker-1", 1, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := db.Jobs.Fail(t.Context(), j.ID, "invalid payload", now); err != nil {
		t.Fatal(err)
	}
	got, err := db.Jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.LastError == nil || *got.LastError != "invalid payload" {
		t.Errorf("LastError = %v, want invalid payload", got.LastError)
	}
	if got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
		t.Errorf("lease was not cleared: owner=%v expires=%v", got.LeaseOwner, got.LeaseExpiresAt)
	}
}

func TestJobRepository_ListFiltersOrdersAndLimits(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	jobs := []domain.Job{
		{ID: "job-a", JobType: "alpha", Payload: "{}", PayloadVersion: 1, State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "job-b", JobType: "beta", Payload: "{}", PayloadVersion: 1, State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now.Add(time.Second)},
		{ID: "job-c", JobType: "alpha", Payload: "{}", PayloadVersion: 1, State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now.Add(2 * time.Second)},
	}
	for _, j := range jobs {
		if err := db.Jobs.Enqueue(t.Context(), j); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Jobs.Succeed(t.Context(), "job-c", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	all, err := db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != "job-c" || all[1].ID != "job-b" || all[2].ID != "job-a" {
		t.Errorf("List(all) = %#v, want job-c, job-b, job-a", all)
	}

	state := domain.JobPending
	pending, err := db.Jobs.List(t.Context(), domain.JobFilter{State: &state})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != "job-b" || pending[1].ID != "job-a" {
		t.Errorf("List(pending) IDs = %v, want [job-b job-a]", jobIDs(pending))
	}

	jobType := "alpha"
	alpha, err := db.Jobs.List(t.Context(), domain.JobFilter{JobType: &jobType})
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha) != 2 || alpha[0].ID != "job-c" || alpha[1].ID != "job-a" {
		t.Errorf("List(alpha) IDs = %v, want [job-c job-a]", jobIDs(alpha))
	}

	limited, err := db.Jobs.List(t.Context(), domain.JobFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].ID != "job-c" {
		t.Errorf("List(limit=1) IDs = %v, want [job-c]", jobIDs(limited))
	}
}

func TestJobRepository_RequeueTerminalJobs(t *testing.T) {
	for _, terminal := range []domain.JobState{domain.JobDead, domain.JobFailed} {
		t.Run(string(terminal), func(t *testing.T) {
			db := newTestDB(t)
			now := time.Now()
			j := mustEnqueueJob(t, db, nil, now)
			if err := db.Jobs.Retry(t.Context(), j.ID, now.Add(time.Hour), "first failure", now); err != nil {
				t.Fatal(err)
			}
			if terminal == domain.JobDead {
				if err := db.Jobs.Kill(t.Context(), j.ID, "terminal", now); err != nil {
					t.Fatal(err)
				}
			} else if err := db.Jobs.Fail(t.Context(), j.ID, "terminal", now); err != nil {
				t.Fatal(err)
			}

			requeuedAt := now.Add(2 * time.Hour)
			if err := db.Jobs.Requeue(t.Context(), j.ID, requeuedAt); err != nil {
				t.Fatal(err)
			}
			got, err := db.Jobs.Get(t.Context(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.JobPending || got.Attempt != 1 || !got.NextRunAt.Equal(requeuedAt) {
				t.Errorf("requeued job = %+v, want pending attempt=1 next_run_at=%v", got, requeuedAt)
			}
			if got.LastError != nil || got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
				t.Errorf("requeued job retained transient state: %+v", got)
			}
		})
	}

	db := newTestDB(t)
	j := mustEnqueueJob(t, db, nil, time.Now())
	if err := db.Jobs.Requeue(t.Context(), j.ID, time.Now()); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Requeue(pending) error = %v, want ErrConflict", err)
	}
}

func TestJobRepository_CountByState(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	for range 3 {
		mustEnqueueJob(t, db, nil, now)
	}
	all, err := db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Jobs.Succeed(t.Context(), all[0].ID, now); err != nil {
		t.Fatal(err)
	}

	counts, err := db.Jobs.CountByState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if counts[domain.JobPending] != 2 || counts[domain.JobSucceeded] != 1 {
		t.Errorf("CountByState() = %v, want pending=2 succeeded=1", counts)
	}
}

func TestJobRepository_ConcurrentClaimAndRenewToleratesWriterContention(t *testing.T) {
	db := newTestDB(t)
	const jobCount = 20
	now := time.Now()
	for range jobCount {
		mustEnqueueJob(t, db, nil, now)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var completed atomic.Int32
	var seenMu sync.Mutex
	seen := make(map[string]bool)
	errCh := make(chan error, 4)
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", worker)
			for completed.Load() < jobCount {
				at := time.Now()
				claimed, err := db.Jobs.Claim(ctx, owner, 1, at, at.Add(time.Minute))
				if err != nil {
					errCh <- fmt.Errorf("claim as %s: %w", owner, err)
					return
				}
				if len(claimed) == 0 {
					select {
					case <-ctx.Done():
						errCh <- ctx.Err()
						return
					case <-time.After(time.Millisecond):
					}
					continue
				}
				job := claimed[0]
				if err := db.Jobs.Renew(ctx, job.ID, owner, at.Add(2*time.Minute), at); err != nil {
					errCh <- fmt.Errorf("renew as %s: %w", owner, err)
					return
				}
				seenMu.Lock()
				if seen[job.ID] {
					seenMu.Unlock()
					errCh <- fmt.Errorf("job %s claimed twice", job.ID)
					return
				}
				seen[job.ID] = true
				seenMu.Unlock()
				if err := db.Jobs.Succeed(ctx, job.ID, at); err != nil {
					errCh <- fmt.Errorf("succeed as %s: %w", owner, err)
					return
				}
				completed.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if completed.Load() != jobCount {
		t.Errorf("completed = %d, want %d", completed.Load(), jobCount)
	}
}

func jobIDs(jobs []domain.Job) []string {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

func TestJobRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Jobs.Get(t.Context(), "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}
