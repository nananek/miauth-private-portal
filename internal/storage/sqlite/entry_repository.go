package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type entryRepository struct{ q querier }

const entrySelectColumns = `SELECT id, thread_id, parent_entry_id, kind, author_actor_id, body,
	processing_status, archived_at, hidden_at, created_at, updated_at`

func (r *entryRepository) Create(ctx context.Context, e domain.Entry) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO entries (id, thread_id, parent_entry_id, kind, author_actor_id, body,
			processing_status, archived_at, hidden_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ThreadID, nullableString(e.ParentEntryID), string(e.Kind), e.AuthorActorID, e.Body,
		string(e.ProcessingStatus), formatTimePtr(e.ArchivedAt), formatTimePtr(e.HiddenAt),
		formatTime(e.CreatedAt), formatTime(e.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *entryRepository) Get(ctx context.Context, id string) (domain.Entry, error) {
	row := r.q.QueryRowContext(ctx, entrySelectColumns+` FROM entries WHERE id = ?`, id)
	return scanEntry(row)
}

func (r *entryRepository) ListByThread(ctx context.Context, threadID string) ([]domain.Entry, error) {
	rows, err := r.q.QueryContext(ctx,
		entrySelectColumns+` FROM entries WHERE thread_id = ? ORDER BY created_at, id`, threadID)
	if err != nil {
		return nil, fmt.Errorf("list entries by thread: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (r *entryRepository) ListChildren(ctx context.Context, parentEntryID string) ([]domain.Entry, error) {
	rows, err := r.q.QueryContext(ctx,
		entrySelectColumns+` FROM entries WHERE parent_entry_id = ? ORDER BY created_at, id`, parentEntryID)
	if err != nil {
		return nil, fmt.Errorf("list child entries: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (r *entryRepository) ListTimeline(ctx context.Context, page domain.Page, includeHidden bool) ([]domain.Entry, error) {
	query := entrySelectColumns + ` FROM entries WHERE 1 = 1`
	var args []any
	if !includeHidden {
		query += ` AND archived_at IS NULL AND hidden_at IS NULL`
	}
	if page.After != nil {
		// A row-value comparison against the (created_at, id) tie-breaker
		// is what makes this cursor stable across same-timestamp entries;
		// see docs/compat/aria-v1.5.11.md.
		query += ` AND (created_at, id) > (?, ?)`
		args = append(args, formatTime(page.After.CreatedAt), page.After.ID)
	}
	query += ` ORDER BY created_at, id`
	if page.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, page.Limit)
	}

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list timeline: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (r *entryRepository) ListTimelineDesc(ctx context.Context, before *domain.Cursor, limit int, includeHidden bool) ([]domain.Entry, error) {
	query := entrySelectColumns + ` FROM entries WHERE 1 = 1`
	var args []any
	if !includeHidden {
		query += ` AND archived_at IS NULL AND hidden_at IS NULL`
	}
	if before != nil {
		// The row-value comparison mirrors ListTimeline's cursor, just
		// reversed: strictly older than before in (created_at, id) order.
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
		return nil, fmt.Errorf("list timeline desc: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (r *entryRepository) CountByAuthor(ctx context.Context, actorID string) (int, error) {
	var count int
	if err := r.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE author_actor_id = ?`, actorID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count entries by author: %w", err)
	}
	return count, nil
}

func (r *entryRepository) UpdateBody(ctx context.Context, id, body string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE entries SET body = ?, updated_at = ? WHERE id = ?`, body, formatTime(at), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *entryRepository) SetProcessingStatus(ctx context.Context, id string, status domain.ProcessingStatus, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE entries SET processing_status = ?, updated_at = ? WHERE id = ?`,
		string(status), formatTime(at), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *entryRepository) SetArchived(ctx context.Context, id string, archived bool, at time.Time) error {
	var archivedAt any
	if archived {
		archivedAt = formatTime(at)
	}
	res, err := r.q.ExecContext(ctx,
		`UPDATE entries SET archived_at = ?, updated_at = ? WHERE id = ?`, archivedAt, formatTime(at), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *entryRepository) SetHidden(ctx context.Context, id string, hidden bool, at time.Time) error {
	var hiddenAt any
	if hidden {
		hiddenAt = formatTime(at)
	}
	res, err := r.q.ExecContext(ctx,
		`UPDATE entries SET hidden_at = ?, updated_at = ? WHERE id = ?`, hiddenAt, formatTime(at), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanEntry(row rowScanner) (domain.Entry, error) {
	var e domain.Entry
	var parentEntryID sql.NullString
	var kind, processingStatus string
	var archivedAt, hiddenAt sql.NullString
	var createdAt, updatedAt string

	if err := row.Scan(&e.ID, &e.ThreadID, &parentEntryID, &kind, &e.AuthorActorID, &e.Body,
		&processingStatus, &archivedAt, &hiddenAt, &createdAt, &updatedAt); err != nil {
		return domain.Entry{}, mapReadError(err)
	}

	e.ParentEntryID = stringPtr(parentEntryID)
	e.Kind = domain.EntryKind(kind)
	e.ProcessingStatus = domain.ProcessingStatus(processingStatus)

	var err error
	if e.ArchivedAt, err = parseTimePtr(archivedAt); err != nil {
		return domain.Entry{}, err
	}
	if e.HiddenAt, err = parseTimePtr(hiddenAt); err != nil {
		return domain.Entry{}, err
	}
	if e.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Entry{}, err
	}
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.Entry{}, err
	}
	return e, nil
}

func scanEntries(rows *sql.Rows) ([]domain.Entry, error) {
	var entries []domain.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entries: %w", err)
	}
	return entries, nil
}
