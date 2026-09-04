package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
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
//
// SQLite does not guarantee RETURNING reports rows in any particular
// order, so the subquery's ORDER BY next_run_at, id (kept for
// readability and to make the candidate selection deterministic) does
// not by itself guarantee the order jobs come back in here. The actual
// ordering callers can rely on - next_run_at ascending, ties broken by
// id ascending so equal-next_run_at jobs cannot starve each other - is
// enforced below in Go.
func (r *jobRepository) Claim(ctx context.Context, leaseOwner string, limit int, now, leaseExpiresAt time.Time) ([]domain.Job, error) {
	rows, err := r.q.QueryContext(ctx,
		`UPDATE jobs SET state = 'running', lease_owner = ?, lease_expires_at = ?, updated_at = ?
		 WHERE id IN (
			SELECT id FROM jobs
			WHERE (state = 'pending' AND next_run_at <= ?)
			   OR (state = 'running' AND lease_expires_at <= ?)
			ORDER BY next_run_at, id
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
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].NextRunAt.Equal(jobs[j].NextRunAt) {
			return jobs[i].NextRunAt.Before(jobs[j].NextRunAt)
		}
		return jobs[i].ID < jobs[j].ID
	})
	return jobs, nil
}

func (r *jobRepository) Renew(ctx context.Context, id, leaseOwner string, leaseExpiresAt, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = ?, updated_at = ?
		 WHERE id = ? AND lease_owner = ? AND state = 'running'`,
		formatTime(leaseExpiresAt), formatTime(at), id, leaseOwner,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *jobRepository) Succeed(ctx context.Context, id, leaseOwner string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'succeeded', lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND lease_owner = ? AND state = 'running'`,
		formatTime(at), id, leaseOwner,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *jobRepository) Retry(ctx context.Context, id, leaseOwner string, nextRunAt time.Time, lastError string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'pending', attempt = attempt + 1, next_run_at = ?, last_error = ?,
			lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND lease_owner = ? AND state = 'running'`,
		formatTime(nextRunAt), lastError, formatTime(at), id, leaseOwner,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *jobRepository) Kill(ctx context.Context, id, leaseOwner, lastError string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'dead', last_error = ?, lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND lease_owner = ? AND state = 'running'`,
		lastError, formatTime(at), id, leaseOwner,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *jobRepository) Fail(ctx context.Context, id, leaseOwner, lastError string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'failed', last_error = ?, lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND lease_owner = ? AND state = 'running'`,
		lastError, formatTime(at), id, leaseOwner,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *jobRepository) List(ctx context.Context, filter domain.JobFilter) ([]domain.Job, error) {
	var where []string
	var args []any
	if filter.State != nil {
		where = append(where, "state = ?")
		args = append(args, string(*filter.State))
	}
	if filter.JobType != nil {
		where = append(where, "job_type = ?")
		args = append(args, *filter.JobType)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	query := jobSelectColumns
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY updated_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return jobs, nil
}

func (r *jobRepository) Requeue(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE jobs SET state = 'pending', next_run_at = ?, lease_owner = NULL,
			lease_expires_at = NULL, last_error = NULL, updated_at = ?
		 WHERE id = ? AND state IN ('dead', 'failed')`,
		formatTime(at), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *jobRepository) CountByState(ctx context.Context) (map[domain.JobState]int, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("count jobs by state: %w", err)
	}
	defer rows.Close()

	counts := make(map[domain.JobState]int)
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, fmt.Errorf("count jobs by state: %w", err)
		}
		counts[domain.JobState(state)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count jobs by state: %w", err)
	}
	return counts, nil
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
