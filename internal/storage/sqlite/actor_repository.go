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
