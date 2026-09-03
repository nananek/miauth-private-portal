package sqlite

import (
	"context"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// WithinTx implements domain.UnitOfWork: fn runs against a domain.Repos
// backed by one *sql.Tx, so writes made through several repositories (for
// example, an Entry and its durable Job intent) commit or roll back
// together. It rolls back on both a returned error and a panic from fn,
// re-panicking afterward so a caller's own recover still observes it.
func (d *DB) WithinTx(ctx context.Context, fn func(ctx context.Context, repos domain.Repos) error) (err error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(ctx, newRepos(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
