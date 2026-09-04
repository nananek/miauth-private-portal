package sqlite

import (
	"database/sql"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so a single scan
// helper can back both a Get and a List method.
type rowScanner interface {
	Scan(dest ...any) error
}

// requireRowAffected returns domain.ErrNotFound when res reports zero
// rows affected, so an UPDATE targeting a nonexistent ID surfaces the same
// sentinel a Get would.
func requireRowAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// requireRowAffectedConflict returns domain.ErrConflict when res reports
// zero rows affected. It backs atomic state-machine transitions (Consume,
// Authorize, ...) whose UPDATE ... WHERE clause encodes the expected
// prior state: zero rows affected there means either the record does not
// exist or it is not currently in that state, and both cases are the
// same "only one winner" conflict a caller must not treat as retryable
// the same way as ErrNotFound.
func requireRowAffectedConflict(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrConflict
	}
	return nil
}
