package domain

import (
	"context"
	"time"
)

// JobState is a durable job's lifecycle state.
type JobState string

const (
	JobPending   JobState = "pending"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobDead      JobState = "dead"
)

// Job is one durable unit of asynchronous work (LLM generation,
// classification, ingestion, ...). JobType and Payload are intentionally
// unconstrained by the schema: a job-type registry belongs in Go, not
// SQL, so adding a job type never requires a migration. The actual worker
// loop that claims and executes jobs is out of this issue's scope (see
// Issue #8); this package only defines the durable, restart-safe record.
type Job struct {
	ID             string
	JobType        string
	Payload        string // JSON, opaque to this package
	PayloadVersion int
	State          JobState
	Attempt        int
	// IdempotencyKey, when set, is enforced unique so retrying an
	// at-least-once enqueue never creates a duplicate job.
	IdempotencyKey *string
	NextRunAt      time.Time
	LeaseOwner     *string
	LeaseExpiresAt *time.Time
	LastError      *string
	SourceEntryID  *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// JobRepository persists durable jobs.
type JobRepository interface {
	// Enqueue inserts a new pending job. It returns ErrConflict if
	// j.IdempotencyKey is set and already used by another job.
	Enqueue(ctx context.Context, j Job) error
	Get(ctx context.Context, id string) (Job, error)
	// Claim atomically leases up to limit pending jobs whose next_run_at
	// has passed, or whose previous lease has expired, transitioning them
	// to running under leaseOwner until leaseExpiresAt.
	Claim(ctx context.Context, leaseOwner string, limit int, now, leaseExpiresAt time.Time) ([]Job, error)
	Succeed(ctx context.Context, id string, at time.Time) error
	// Retry transitions a running job back to pending with an
	// incremented attempt count, a bounded-backoff nextRunAt, and a
	// recorded last error.
	Retry(ctx context.Context, id string, nextRunAt time.Time, lastError string, at time.Time) error
	// Kill transitions a job to its terminal dead state after retries are
	// exhausted.
	Kill(ctx context.Context, id, lastError string, at time.Time) error
}
