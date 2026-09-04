package sqlite

import (
	"context"
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
		`INSERT INTO actors (id, actor_type, created_at) VALUES (?, ?, ?)`,
		a.ID, string(a.Type), formatTime(a.CreatedAt),
	)
	return mapWriteError(err)
}

func (r *actorRepository) Get(ctx context.Context, id string) (domain.Actor, error) {
	return scanActor(r.q.QueryRowContext(ctx,
		`SELECT id, actor_type, created_at FROM actors WHERE id = ?`, id))
}

func (r *actorRepository) GetByType(ctx context.Context, actorType domain.ActorType) (domain.Actor, error) {
	return scanActor(r.q.QueryRowContext(ctx,
		`SELECT id, actor_type, created_at FROM actors WHERE actor_type = ?`, string(actorType)))
}

func scanActor(row rowScanner) (domain.Actor, error) {
	var a domain.Actor
	var actorType, createdAt string
	if err := row.Scan(&a.ID, &actorType, &createdAt); err != nil {
		return domain.Actor{}, mapReadError(err)
	}
	a.Type = domain.ActorType(actorType)
	t, err := parseTime(createdAt)
	if err != nil {
		return domain.Actor{}, err
	}
	a.CreatedAt = t
	return a, nil
}
