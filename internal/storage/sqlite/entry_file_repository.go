package sqlite

import (
	"context"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type entryFileRepository struct{ q querier }

// Create inserts one row per fileID, at position 0, 1, 2, ... in
// fileIDs' own order (see domain.EntryFileRepository.Create's doc
// comment). Always called with a handful of IDs (a single note's
// attachments), so a plain per-row INSERT loop needs no batching.
func (r *entryFileRepository) Create(ctx context.Context, entryID string, fileIDs []string) error {
	for i, fileID := range fileIDs {
		if _, err := r.q.ExecContext(ctx,
			`INSERT INTO entry_files (entry_id, file_id, position) VALUES (?, ?, ?)`,
			entryID, fileID, i,
		); err != nil {
			return mapWriteError(err)
		}
	}
	return nil
}

// ListFilesByEntry joins through entry_files to return entryID's
// attached files in Position order. None of the files table's own
// column names (fileSelectColumns) collide with entry_files'
// (entry_id/file_id/position), so the unqualified select list is
// unambiguous even across this join.
func (r *entryFileRepository) ListFilesByEntry(ctx context.Context, entryID string) ([]domain.File, error) {
	rows, err := r.q.QueryContext(ctx,
		fileSelectColumns+` FROM entry_files JOIN files ON entry_files.file_id = files.id
		 WHERE entry_files.entry_id = ? ORDER BY entry_files.position ASC`,
		entryID,
	)
	if err != nil {
		return nil, fmt.Errorf("list files by entry: %w", err)
	}
	defer rows.Close()

	out := []domain.File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *entryFileRepository) CountByFile(ctx context.Context, fileID string) (int, error) {
	var n int
	if err := r.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entry_files WHERE file_id = ?`, fileID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count entry_files by file: %w", err)
	}
	return n, nil
}

// ListEntriesByFile joins through entry_files to return every entry
// fileID is attached to, in no particular guaranteed order (callers
// needing a stable presentation order, such as attached-notes, sort
// after the fact if they ever need to — no current caller does).
func (r *entryFileRepository) ListEntriesByFile(ctx context.Context, fileID string) ([]domain.Entry, error) {
	rows, err := r.q.QueryContext(ctx,
		entrySelectColumns+` FROM entry_files JOIN entries ON entry_files.entry_id = entries.id
		 WHERE entry_files.file_id = ?`,
		fileID,
	)
	if err != nil {
		return nil, fmt.Errorf("list entries by file: %w", err)
	}
	defer rows.Close()

	out := []domain.Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
