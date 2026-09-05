package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type actorRepository struct{ q querier }

// EnsureReservedActors idempotently creates the Assistant and System
// actors. The INSERT ... WHERE NOT EXISTS form makes this safe to call on
// every startup without a separate existence check racing the insert.
func (r *actorRepository) EnsureReservedActors(ctx context.Context) error {
	for _, t := range []domain.ActorType{domain.ActorAssistant, domain.ActorSystem} {
		if _, err := r.q.ExecContext(ctx,
			`INSERT INTO actors (id, actor_type, created_at)
			 SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM actors WHERE actor_type = ?)`,
			domain.NewID(), string(t), formatTime(time.Now()), string(t),
		); err != nil {
			return fmt.Errorf("ensure reserved actor %s: %w", t, mapWriteError(err))
		}
	}
	return nil
}

// Create inserts a new actor. The actors table's UNIQUE(actor_type)
// constraint is what makes this the safe, sole path for creating the
// Owner actor: a second concurrent attempt collides on that constraint
// and mapWriteError turns it into domain.ErrConflict.
func (r *actorRepository) Create(ctx context.Context, a domain.Actor) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at, display_name) VALUES (?, ?, ?, ?)`,
		a.ID, string(a.Type), formatTime(a.CreatedAt), nullableString(a.DisplayName),
	)
	return mapWriteError(err)
}

func (r *actorRepository) Get(ctx context.Context, id string) (domain.Actor, error) {
	return scanActor(r.q.QueryRowContext(ctx,
		`SELECT id, actor_type, created_at, display_name FROM actors WHERE id = ?`, id))
}

func (r *actorRepository) GetByType(ctx context.Context, actorType domain.ActorType) (domain.Actor, error) {
	return scanActor(r.q.QueryRowContext(ctx,
		`SELECT id, actor_type, created_at, display_name FROM actors WHERE actor_type = ?`, string(actorType)))
}

// SetDisplayName always writes displayName as given (including ""),
// never SQL NULL: NULL on this column means "never explicitly set"
// (e.g. the reserved assistant/system actors, or an owner row from
// before Issue #23 PR1's migration), distinct from an owner who
// explicitly cleared their display name back to empty.
func (r *actorRepository) SetDisplayName(ctx context.Context, actorID string, displayName string) error {
	res, err := r.q.ExecContext(ctx, `UPDATE actors SET display_name = ? WHERE id = ?`, displayName, actorID)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanActor(row rowScanner) (domain.Actor, error) {
	var a domain.Actor
	var actorType, createdAt string
	var displayName sql.NullString
	if err := row.Scan(&a.ID, &actorType, &createdAt, &displayName); err != nil {
		return domain.Actor{}, mapReadError(err)
	}
	a.Type = domain.ActorType(actorType)
	a.DisplayName = stringPtr(displayName)
	t, err := parseTime(createdAt)
	if err != nil {
		return domain.Actor{}, err
	}
	a.CreatedAt = t
	return a, nil
}
