package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type fileRepository struct{ q querier }

const fileSelectColumns = `SELECT id, owner_actor_id, purpose, mime, byte_size, sha256, md5, storage_key, width, height, name, comment, is_sensitive, folder_id, created_at`

func (r *fileRepository) Create(ctx context.Context, f domain.File) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO files (id, owner_actor_id, purpose, mime, byte_size, sha256, md5, storage_key, width, height, name, comment, is_sensitive, folder_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, nullableString(f.OwnerActorID), string(f.Purpose), f.MIME, f.ByteSize, f.SHA256, f.MD5, f.StorageKey,
		nullableInt(f.Width), nullableInt(f.Height), f.Name, nullableString(f.Comment), boolToInt(f.IsSensitive),
		nullableString(f.FolderID), formatTime(f.CreatedAt),
	)
	return mapWriteError(err)
}

func (r *fileRepository) Get(ctx context.Context, id string) (domain.File, error) {
	return scanFile(r.q.QueryRowContext(ctx, fileSelectColumns+` FROM files WHERE id = ?`, id))
}

// ListByOwner mirrors mentionRepository.ListEntriesByMentionedActor's
// reversed row-value cursor comparison: strictly older than before in
// (created_at, id) order, the same stable-cursor shape every paginated
// list in this package uses.
func (r *fileRepository) ListByOwner(ctx context.Context, ownerActorID string, folderID *string, before *domain.Cursor, limit int) ([]domain.File, error) {
	query := fileSelectColumns + ` FROM files WHERE owner_actor_id = ?`
	args := []any{ownerActorID}
	if folderID == nil {
		query += ` AND folder_id IS NULL`
	} else {
		query += ` AND folder_id = ?`
		args = append(args, *folderID)
	}
	if before != nil {
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
		return nil, fmt.Errorf("list files by owner: %w", err)
	}
	defer rows.Close()
	var out []domain.File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *fileRepository) Update(ctx context.Context, f domain.File) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE files SET name = ?, comment = ?, is_sensitive = ?, folder_id = ? WHERE id = ?`,
		f.Name, nullableString(f.Comment), boolToInt(f.IsSensitive), nullableString(f.FolderID), f.ID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *fileRepository) Delete(ctx context.Context, id string) error {
	res, err := r.q.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *fileRepository) SumByteSizeByOwner(ctx context.Context, ownerActorID string) (int64, error) {
	var sum sql.NullInt64
	if err := r.q.QueryRowContext(ctx,
		`SELECT SUM(byte_size) FROM files WHERE owner_actor_id = ?`, ownerActorID,
	).Scan(&sum); err != nil {
		return 0, fmt.Errorf("sum file bytes by owner: %w", err)
	}
	return sum.Int64, nil
}

func (r *fileRepository) CountByFolder(ctx context.Context, folderID string) (int, error) {
	var n int
	if err := r.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE folder_id = ?`, folderID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count files by folder: %w", err)
	}
	return n, nil
}

func (r *fileRepository) ListStorageKeys(ctx context.Context) (map[string]bool, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT storage_key FROM files`)
	if err != nil {
		return nil, fmt.Errorf("list file storage keys: %w", err)
	}
	defer rows.Close()

	keys := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan file storage key: %w", err)
		}
		keys[key] = true
	}
	return keys, rows.Err()
}

func scanFile(row rowScanner) (domain.File, error) {
	var f domain.File
	var ownerActorID, comment, folderID sql.NullString
	var width, height sql.NullInt64
	var purpose, createdAt string
	var isSensitive int
	if err := row.Scan(&f.ID, &ownerActorID, &purpose, &f.MIME, &f.ByteSize, &f.SHA256, &f.MD5, &f.StorageKey,
		&width, &height, &f.Name, &comment, &isSensitive, &folderID, &createdAt); err != nil {
		return domain.File{}, mapReadError(err)
	}
	f.OwnerActorID = stringPtr(ownerActorID)
	f.Purpose = domain.FilePurpose(purpose)
	f.Width = intPtr(width)
	f.Height = intPtr(height)
	f.Comment = stringPtr(comment)
	f.IsSensitive = isSensitive != 0
	f.FolderID = stringPtr(folderID)

	var err error
	if f.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.File{}, err
	}
	return f, nil
}
