package sqlite

import (
	"context"
	"database/sql"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// querier is satisfied by both *sql.DB and *sql.Tx, so every repository
// in this package works identically whether called standalone (against
// *DB.Repos) or inside a UnitOfWork.WithinTx transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// newRepos builds a domain.Repos backed by q: the top-level *sql.DB for
// standalone calls, or a *sql.Tx for one UnitOfWork transaction.
func newRepos(q querier) domain.Repos {
	return domain.Repos{
		Actors:          &actorRepository{q: q},
		Threads:         &threadRepository{q: q},
		Entries:         &entryRepository{q: q},
		LocalMiAuth:     &localMiAuthSessionRepository{q: q},
		APITokens:       &apiTokenRepository{q: q},
		UserTags:        &userTagRepository{q: q},
		Classifications: &llmClassificationRepository{q: q},
		Generations:     &llmGenerationRepository{q: q},
		Jobs:            &jobRepository{q: q},
		ExternalSources: &externalSourceRepository{q: q},
		ExternalItems:   &externalItemRepository{q: q},
	}
}
