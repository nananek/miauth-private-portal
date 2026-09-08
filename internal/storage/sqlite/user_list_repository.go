package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type userListRepository struct{ q querier }

const userListSelectColumns = `SELECT id, name, is_public, created_at, updated_at`

func (r *userListRepository) Create(ctx context.Context, l domain.UserList) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO user_lists (id, name, is_public, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		l.ID, l.Name, boolToInt(l.IsPublic), formatTime(l.CreatedAt), formatTime(l.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *userListRepository) Get(ctx context.Context, id string) (domain.UserList, error) {
	return scanUserList(r.q.QueryRowContext(ctx, userListSelectColumns+` FROM user_lists WHERE id = ?`, id))
}

func (r *userListRepository) ListAll(ctx context.Context) ([]domain.UserList, error) {
	rows, err := r.q.QueryContext(ctx, userListSelectColumns+` FROM user_lists ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list user lists: %w", err)
	}
	defer rows.Close()

	var out []domain.UserList
	for rows.Next() {
		l, err := scanUserList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Update applies a partial change: COALESCE keeps name/is_public
// unchanged when the corresponding argument is nil, the same partial-
// update shape POST /api/users/lists/update's request accepts.
func (r *userListRepository) Update(ctx context.Context, id string, name *string, isPublic *bool, updatedAt time.Time) (domain.UserList, error) {
	var isPublicParam any
	if isPublic != nil {
		isPublicParam = boolToInt(*isPublic)
	}
	res, err := r.q.ExecContext(ctx,
		`UPDATE user_lists SET name = COALESCE(?, name), is_public = COALESCE(?, is_public), updated_at = ? WHERE id = ?`,
		nullableString(name), isPublicParam, formatTime(updatedAt), id,
	)
	if err != nil {
		return domain.UserList{}, mapWriteError(err)
	}
	if err := requireRowAffected(res); err != nil {
		return domain.UserList{}, err
	}
	return r.Get(ctx, id)
}

// Delete removes id's membership rows before the list row itself: see
// migration 0033_user_lists.sql's doc comment for why this is explicit
// rather than an ON DELETE CASCADE.
func (r *userListRepository) Delete(ctx context.Context, id string) error {
	if _, err := r.q.ExecContext(ctx, `DELETE FROM user_list_members WHERE list_id = ?`, id); err != nil {
		return mapWriteError(err)
	}
	res, err := r.q.ExecContext(ctx, `DELETE FROM user_lists WHERE id = ?`, id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *userListRepository) AddMember(ctx context.Context, listID, actorID string, addedAt time.Time) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO user_list_members (list_id, actor_id, added_at) VALUES (?, ?, ?)
		 ON CONFLICT (list_id, actor_id) DO NOTHING`,
		listID, actorID, formatTime(addedAt),
	)
	return mapWriteError(err)
}

func (r *userListRepository) RemoveMember(ctx context.Context, listID, actorID string) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM user_list_members WHERE list_id = ? AND actor_id = ?`, listID, actorID)
	return mapWriteError(err)
}

func (r *userListRepository) MemberActorIDs(ctx context.Context, listID string) ([]string, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT actor_id FROM user_list_members WHERE list_id = ? ORDER BY added_at, actor_id`, listID)
	if err != nil {
		return nil, fmt.Errorf("list user list member actor ids: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var actorID string
		if err := rows.Scan(&actorID); err != nil {
			return nil, fmt.Errorf("scan user list member actor id: %w", err)
		}
		out = append(out, actorID)
	}
	return out, rows.Err()
}

func scanUserList(row rowScanner) (domain.UserList, error) {
	var l domain.UserList
	var isPublic int
	var createdAt, updatedAt string
	if err := row.Scan(&l.ID, &l.Name, &isPublic, &createdAt, &updatedAt); err != nil {
		return domain.UserList{}, mapReadError(err)
	}
	l.IsPublic = isPublic != 0

	var err error
	if l.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.UserList{}, err
	}
	if l.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.UserList{}, err
	}
	return l, nil
}
