package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type jobRepository struct{ q querier }

const jobSelectColumns = `SELECT id, job_type, payload, payload_version, state, attempt, idempotency_key,
	next_run_at, lease_owner, lease_expires_at, last_error, source_entry_id, created_at, updated_at
	FROM jobs`

func (r *jobRepository) Enqueue(ctx context.Context, j domain.Job) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO jobs (id, job_type, payload, payload_version, state, attempt, idempotency_key,
			next_run_at, lease_owner, lease_expires_at, last_error, source_entry_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.JobType, j.Payload, j.PayloadVersion, string(j.State), j.Attempt, nullableString(j.IdempotencyKey),
		formatTime(j.NextRunAt), nullableString(j.LeaseOwner), formatTimePtr(j.LeaseExpiresAt),
		nullableString(j.LastError), nullableString(j.SourceEntryID), formatTime(j.CreatedAt), formatTime(j.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *jobRepository) Get(ctx context.Context, id string) (domain.Job, error) {
	row := r.q.QueryRowContext(ctx, jobSelectColumns+` WHERE id = ?`, id)
	return scanJob(row)
}

// Claim atomically leases up to limit pending jobs whose next_run_at has
// passed, or previously leased jobs whose lease has expired (a crashed
// worker's abandoned job), transitioning them to running. The candidate
// selection and the UPDATE are one SQL statement (RETURNING reports which
// rows it touched), so this is safe against a concurrent Claim call
// without needing its own transaction.
func (r *jobRepository) Claim(ctx context.Context, leaseOwner string, limit int, now, leaseExpiresAt time.Time) ([]domain.Job, error) {
	rows, err := r.q.QueryContext(ctx,
		`UPDATE jobs SET state = 'running', lease_owner = ?, lease_expires_at = ?, updated_at = ?
		 WHERE id IN (
			SELECT id FROM jobs
			WHERE (state = 'pending' AND next_run_at <= ?)
			   OR (state = 'running' AND lease_expires_at <= ?)
			ORDER BY next_run_at
			LIMIT ?
		 )
		 RETURNING id, job_type, payload, payload_version, state, attempt, idempotency_key, next_run_at,
			lease_owner, lease_expires_at, last_error, source_entry_id, created_at, updated_at`,
		leaseOwner, formatTime(leaseExpiresAt), formatTime(now), formatTime(now), formatTime(now), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	defer rows.Close()

	var jobs []domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (r *jobRepository) Succeed(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'succeeded', lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ?`,
		formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *jobRepository) Retry(ctx context.Context, id string, nextRunAt time.Time, lastError string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'pending', attempt = attempt + 1, next_run_at = ?, last_error = ?,
			lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ?`,
		formatTime(nextRunAt), lastError, formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *jobRepository) Kill(ctx context.Context, id, lastError string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'dead', last_error = ?, lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ?`,
		lastError, formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanJob(row rowScanner) (domain.Job, error) {
	var j domain.Job
	var state string
	var idempotencyKey sql.NullString
	var nextRunAt string
	var leaseOwner, leaseExpiresAt, lastError, sourceEntryID sql.NullString
	var createdAt, updatedAt string

	if err := row.Scan(&j.ID, &j.JobType, &j.Payload, &j.PayloadVersion, &state, &j.Attempt, &idempotencyKey,
		&nextRunAt, &leaseOwner, &leaseExpiresAt, &lastError, &sourceEntryID, &createdAt, &updatedAt); err != nil {
		return domain.Job{}, mapReadError(err)
	}

	j.State = domain.JobState(state)
	j.IdempotencyKey = stringPtr(idempotencyKey)
	j.LeaseOwner = stringPtr(leaseOwner)
	j.LastError = stringPtr(lastError)
	j.SourceEntryID = stringPtr(sourceEntryID)

	var err error
	if j.NextRunAt, err = parseTime(nextRunAt); err != nil {
		return domain.Job{}, err
	}
	if j.LeaseExpiresAt, err = parseTimePtr(leaseExpiresAt); err != nil {
		return domain.Job{}, err
	}
	if j.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Job{}, err
	}
	if j.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.Job{}, err
	}
	return j, nil
}
