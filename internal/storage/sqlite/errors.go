package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	sqlitedriver "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// mapWriteError translates a modernc.org/sqlite UNIQUE/PRIMARY KEY
// constraint violation into domain.ErrConflict, so every repository's
// "this is already taken" case (a duplicate idempotency key, token hash,
// or the owner_bindings singleton row) exposes one storage-independent
// sentinel instead of a SQLite-specific error type. Any other error,
// including a foreign-key or CHECK violation, is returned wrapped but
// otherwise unchanged: those indicate invalid caller input, not a
// conflict a caller would retry past.
func mapWriteError(err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *sqlitedriver.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqlitelib.SQLITE_CONSTRAINT_UNIQUE, sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY:
			return domain.ErrConflict
		}
	}
	return fmt.Errorf("sqlite write: %w", err)
}

// mapReadError translates sql.ErrNoRows into domain.ErrNotFound.
func mapReadError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}
