package sqlite

import (
	"context"
	"fmt"
	"time"
)

type userTagRepository struct{ q querier }

func (r *userTagRepository) Add(ctx context.Context, entryID, tag string, at time.Time) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO user_tags (entry_id, tag, created_at) VALUES (?, ?, ?)`, entryID, tag, formatTime(at))
	return mapWriteError(err)
}

func (r *userTagRepository) Remove(ctx context.Context, entryID, tag string) error {
	res, err := r.q.ExecContext(ctx, `DELETE FROM user_tags WHERE entry_id = ? AND tag = ?`, entryID, tag)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *userTagRepository) ListByEntry(ctx context.Context, entryID string) ([]string, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT tag FROM user_tags WHERE entry_id = ? ORDER BY tag`, entryID)
	if err != nil {
		return nil, fmt.Errorf("list user tags: %w", err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("scan user tag: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}
