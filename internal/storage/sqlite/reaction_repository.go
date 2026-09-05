package sqlite

import (
	"context"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type reactionRepository struct{ q querier }

const reactionSelectColumns = `SELECT id, entry_id, reactor_actor_id, emoji, created_at`

// Create is an upsert: a second call for the same (entry_id,
// reactor_actor_id) pair overwrites the emoji/created_at of the first
// (see domain.Reaction's doc comment on why this is not a conflict).
func (r *reactionRepository) Create(ctx context.Context, react domain.Reaction) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO reactions (id, entry_id, reactor_actor_id, emoji, created_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (entry_id, reactor_actor_id) DO UPDATE SET emoji = excluded.emoji, created_at = excluded.created_at`,
		react.ID, react.EntryID, react.ReactorActorID, react.Emoji, formatTime(react.CreatedAt),
	)
	return mapWriteError(err)
}

// Delete is intentionally idempotent (see ReactionRepository.Delete's doc
// comment): it never checks rows affected, unlike
// userTagRepository.Remove.
func (r *reactionRepository) Delete(ctx context.Context, entryID, reactorActorID string) error {
	_, err := r.q.ExecContext(ctx,
		`DELETE FROM reactions WHERE entry_id = ? AND reactor_actor_id = ?`, entryID, reactorActorID)
	return mapWriteError(err)
}

func (r *reactionRepository) Get(ctx context.Context, id string) (domain.Reaction, error) {
	return scanReaction(r.q.QueryRowContext(ctx, reactionSelectColumns+` FROM reactions WHERE id = ?`, id))
}

func (r *reactionRepository) GetByActor(ctx context.Context, entryID, reactorActorID string) (domain.Reaction, error) {
	return scanReaction(r.q.QueryRowContext(ctx,
		reactionSelectColumns+` FROM reactions WHERE entry_id = ? AND reactor_actor_id = ?`, entryID, reactorActorID))
}

func (r *reactionRepository) CountsByEmoji(ctx context.Context, entryID string) (map[string]int, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT emoji, COUNT(*) FROM reactions WHERE entry_id = ? GROUP BY emoji`, entryID)
	if err != nil {
		return nil, fmt.Errorf("count reactions by emoji: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var emoji string
		var count int
		if err := rows.Scan(&emoji, &count); err != nil {
			return nil, fmt.Errorf("scan reaction count: %w", err)
		}
		counts[emoji] = count
	}
	return counts, rows.Err()
}

func (r *reactionRepository) CountAll(ctx context.Context) (int, error) {
	var count int
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM reactions`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count all reactions: %w", err)
	}
	return count, nil
}

func (r *reactionRepository) ListByEntry(ctx context.Context, entryID string, emoji *string, before *domain.Cursor, limit int) ([]domain.Reaction, error) {
	query := reactionSelectColumns + ` FROM reactions WHERE entry_id = ?`
	args := []any{entryID}
	if emoji != nil {
		query += ` AND emoji = ?`
		args = append(args, *emoji)
	}
	if before != nil {
		// Mirrors entryRepository.ListTimelineDesc's reversed row-value
		// cursor comparison: strictly older than before in
		// (created_at, id) order.
		query += ` AND (created_at, id) < (?, ?)`
		args = append(args, formatTime(before.CreatedAt), before.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list reactions by entry: %w", err)
	}
	defer rows.Close()

	var out []domain.Reaction
	for rows.Next() {
		react, err := scanReaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, react)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reactions: %w", err)
	}
	return out, nil
}

func scanReaction(row rowScanner) (domain.Reaction, error) {
	var react domain.Reaction
	var createdAt string
	if err := row.Scan(&react.ID, &react.EntryID, &react.ReactorActorID, &react.Emoji, &createdAt); err != nil {
		return domain.Reaction{}, mapReadError(err)
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return domain.Reaction{}, err
	}
	react.CreatedAt = t
	return react, nil
}
