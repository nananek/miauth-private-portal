package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type folderRepository struct{ q querier }

const folderSelectColumns = `SELECT id, owner_actor_id, name, parent_id, created_at`

func (r *folderRepository) Create(ctx context.Context, f domain.Folder) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO folders (id, owner_actor_id, name, parent_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		f.ID, f.OwnerActorID, f.Name, nullableString(f.ParentID), formatTime(f.CreatedAt),
	)
	return mapWriteError(err)
}

func (r *folderRepository) Get(ctx context.Context, id string) (domain.Folder, error) {
	return scanFolder(r.q.QueryRowContext(ctx, folderSelectColumns+` FROM folders WHERE id = ?`, id))
}

func (r *folderRepository) ListByOwner(ctx context.Context, ownerActorID string, parentID *string, before *domain.Cursor, limit int) ([]domain.Folder, error) {
	query := folderSelectColumns + ` FROM folders WHERE owner_actor_id = ?`
	args := []any{ownerActorID}
	if parentID == nil {
		query += ` AND parent_id IS NULL`
	} else {
		query += ` AND parent_id = ?`
		args = append(args, *parentID)
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
		return nil, fmt.Errorf("list folders by owner: %w", err)
	}
	defer rows.Close()
	var out []domain.Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *folderRepository) Update(ctx context.Context, f domain.Folder) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE folders SET name = ?, parent_id = ? WHERE id = ?`,
		f.Name, nullableString(f.ParentID), f.ID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *folderRepository) Delete(ctx context.Context, id string) error {
	res, err := r.q.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *folderRepository) CountChildFolders(ctx context.Context, parentID string) (int, error) {
	var n int
	if err := r.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM folders WHERE parent_id = ?`, parentID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count child folders: %w", err)
	}
	return n, nil
}

func scanFolder(row rowScanner) (domain.Folder, error) {
	var f domain.Folder
	var parentID sql.NullString
	var createdAt string
	if err := row.Scan(&f.ID, &f.OwnerActorID, &f.Name, &parentID, &createdAt); err != nil {
		return domain.Folder{}, mapReadError(err)
	}
	f.ParentID = stringPtr(parentID)

	var err error
	if f.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Folder{}, err
	}
	return f, nil
}
