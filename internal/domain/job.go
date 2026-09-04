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
// SQL, so adding a job type never requires a migration.
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
	// Renew extends a still-running job's lease. It returns ErrConflict if
	// the job is no longer running under leaseOwner.
	Renew(ctx context.Context, id, leaseOwner string, leaseExpiresAt, at time.Time) error
	// Succeed transitions a running job owned by leaseOwner to succeeded.
	// It returns ErrConflict if ownership or state changed first.
	Succeed(ctx context.Context, id, leaseOwner string, at time.Time) error
	// Retry transitions a running job owned by leaseOwner back to pending with an
	// incremented attempt count, a bounded-backoff nextRunAt, and a
	// recorded last error. It returns ErrConflict if ownership or state changed.
	Retry(ctx context.Context, id, leaseOwner string, nextRunAt time.Time, lastError string, at time.Time) error
	// Kill transitions a running job owned by leaseOwner to its terminal dead
	// state after retries are exhausted. It returns ErrConflict if ownership or
	// state changed.
	Kill(ctx context.Context, id, leaseOwner, lastError string, at time.Time) error
	// Fail transitions a running job owned by leaseOwner directly to its terminal
	// failed state when retrying a classified permanent error would not help. It
	// returns ErrConflict if ownership or state changed.
	Fail(ctx context.Context, id, leaseOwner, lastError string, at time.Time) error
	// List returns jobs matching filter, most recently updated first.
	List(ctx context.Context, filter JobFilter) ([]Job, error)
	// Requeue moves a dead or failed job back to pending for an immediate
	// manual retry while preserving its automatic-attempt count.
	Requeue(ctx context.Context, id string, at time.Time) error
	// CountByState returns the current queue depth grouped by state.
	CountByState(ctx context.Context) (map[JobState]int, error)
}

// JobFilter narrows an administrative job listing. A nil field means no
// filter on that dimension. A non-positive Limit uses the repository's
// bounded default.
type JobFilter struct {
	State   *JobState
	JobType *string
	Limit   int
}
