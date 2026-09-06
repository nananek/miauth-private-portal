package sqlite

import (
	"context"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type notificationRepository struct{ q querier }

const notificationSelectColumns = `SELECT id, type, related_entry_id, created_at`

func (r *notificationRepository) Create(ctx context.Context, n domain.Notification) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO notifications (id, type, related_entry_id, created_at) VALUES (?, ?, ?, ?)`,
		n.ID, string(n.Type), n.RelatedEntryID, formatTime(n.CreatedAt),
	)
	return mapWriteError(err)
}

func (r *notificationRepository) Get(ctx context.Context, id string) (domain.Notification, error) {
	return scanNotification(r.q.QueryRowContext(ctx, notificationSelectColumns+` FROM notifications WHERE id = ?`, id))
}

func (r *notificationRepository) ListDesc(ctx context.Context, before *domain.Cursor, limit int) ([]domain.Notification, error) {
	query := notificationSelectColumns + ` FROM notifications`
	var args []any
	if before != nil {
		// Mirrors reactionRepository.ListByEntry's reversed row-value
		// cursor comparison: strictly older than before in
		// (created_at, id) order.
		query += ` WHERE (created_at, id) < (?, ?)`
		args = append(args, formatTime(before.CreatedAt), before.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	var out []domain.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notifications: %w", err)
	}
	return out, nil
}

func scanNotification(row rowScanner) (domain.Notification, error) {
	var n domain.Notification
	var notifType, createdAt string
	if err := row.Scan(&n.ID, &notifType, &n.RelatedEntryID, &createdAt); err != nil {
		return domain.Notification{}, mapReadError(err)
	}
	n.Type = domain.NotificationType(notifType)
	t, err := parseTime(createdAt)
	if err != nil {
		return domain.Notification{}, err
	}
	n.CreatedAt = t
	return n, nil
}
