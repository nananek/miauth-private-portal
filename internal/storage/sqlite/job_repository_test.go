package sqlite

import (
	"errors"
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

func TestJobRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Jobs.Get(t.Context(), "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}
