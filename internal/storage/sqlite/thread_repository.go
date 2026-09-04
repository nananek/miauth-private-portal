package sqlite

import (
	"context"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type threadRepository struct{ q querier }

func (r *threadRepository) Create(ctx context.Context, t domain.Thread) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO threads (id, created_at, updated_at) VALUES (?, ?, ?)`,
		t.ID, formatTime(t.CreatedAt), formatTime(t.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *threadRepository) Get(ctx context.Context, id string) (domain.Thread, error) {
	row := r.q.QueryRowContext(ctx, `SELECT id, created_at, updated_at FROM threads WHERE id = ?`, id)
	var t domain.Thread
	var createdAt, updatedAt string
	if err := row.Scan(&t.ID, &createdAt, &updatedAt); err != nil {
		return domain.Thread{}, mapReadError(err)
	}
	var err error
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Thread{}, err
	}
	if t.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.Thread{}, err
	}
	return t, nil
}

func (r *threadRepository) Touch(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}
