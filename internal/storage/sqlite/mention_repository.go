package sqlite

import (
	"context"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type mentionRepository struct{ q querier }

func (r *mentionRepository) Create(ctx context.Context, m domain.Mention) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO mentions (id, entry_id, mentioned_actor_id, created_at) VALUES (?, ?, ?, ?)`,
		m.ID, m.EntryID, m.MentionedActorID, formatTime(m.CreatedAt),
	)
	return mapWriteError(err)
}

// mentionEntrySelectColumns is entrySelectColumns qualified with the
// entries. prefix: the plain unqualified constant would be ambiguous
// once joined against mentions, which has its own id and created_at
// columns. The column order matches entrySelectColumns exactly, so
// scanEntry/scanEntries (positional Scan, not by name) work unchanged.
const mentionEntrySelectColumns = `SELECT entries.id, entries.thread_id, entries.parent_entry_id, entries.kind, entries.author_actor_id, entries.body,
	entries.processing_status, entries.archived_at, entries.hidden_at, entries.created_at, entries.updated_at, entries.provenance_url`

func (r *mentionRepository) ListEntriesByMentionedActor(ctx context.Context, actorID string, before *domain.Cursor, limit int) ([]domain.Entry, error) {
	query := mentionEntrySelectColumns + `
		FROM entries
		JOIN mentions ON mentions.entry_id = entries.id
		WHERE mentions.mentioned_actor_id = ? AND entries.archived_at IS NULL AND entries.hidden_at IS NULL`
	args := []any{actorID}
	if before != nil {
		// Mirrors entryRepository.ListTimelineDesc's reversed row-value
		// cursor comparison: strictly older than before in
		// (created_at, id) order.
		query += ` AND (entries.created_at, entries.id) < (?, ?)`
		args = append(args, formatTime(before.CreatedAt), before.ID)
	}
	query += ` ORDER BY entries.created_at DESC, entries.id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list entries by mentioned actor: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}
