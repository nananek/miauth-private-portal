package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// NormalizeStoredTimestamps rewrites every stored timestamp that does not
// already match timeLayout's fixed nine-digit fraction back into that
// canonical form, and reports how many rows it changed and how many it
// found too malformed to parse even with parseTime's now-permissive
// timeParseLayout (Issue #148) — a caller should treat any skipped count
// above zero as worth investigating, though not worth failing startup
// over.
//
// parseTime already tolerates any fractional width, so a row like this is
// no longer a crash risk by itself. But it is still a correctness gap:
// every ORDER BY and row-value pagination cursor on a timestamp column
// compares stored strings byte-wise (see timeLayout's own doc comment),
// and a non-canonical width breaks that comparison's agreement with
// chronological order for that one row. This walks every table's every
// "_at"-suffixed TEXT column — this schema's own timestamp-column
// convention, unbroken across every migration to date — parses each
// stored value and rewrites it only when the canonical re-encoding
// differs from what is already stored, so a fully-normalized database
// costs one read pass per column and zero writes.
//
// Safe to call on every startup: idempotent, and a single row that fails
// even the tolerant parse is counted and left untouched rather than
// aborting the pass — the same "one bad row must not block everything
// else" principle Issue #148 applies to the RSS actor read path this was
// written for.
func (d *DB) NormalizeStoredTimestamps(ctx context.Context) (normalized int, skipped int, err error) {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds

	tables, err := timestampOwningTables(ctx, tx)
	if err != nil {
		return 0, 0, fmt.Errorf("list tables: %w", err)
	}

	for _, table := range tables {
		columns, err := timestampColumns(ctx, tx, table)
		if err != nil {
			return 0, 0, fmt.Errorf("list columns of %s: %w", table, err)
		}
		for _, column := range columns {
			n, s, err := normalizeColumn(ctx, tx, table, column)
			if err != nil {
				return 0, 0, fmt.Errorf("normalize %s.%s: %w", table, column, err)
			}
			normalized += n
			skipped += s
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	return normalized, skipped, nil
}

// timestampOwningTables lists every real, user-defined table (never a
// view, and never SQLite's own sqlite_% bookkeeping tables) that might
// carry a timestamp column.
func timestampOwningTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// timestampColumns lists table's columns that follow this schema's own
// timestamp convention: a TEXT column whose name ends in "_at" (created_at,
// updated_at, expires_at, ...), true without exception across every
// migration this service has ever shipped.
func timestampColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		if strings.EqualFold(colType, "TEXT") && strings.HasSuffix(name, "_at") {
			columns = append(columns, name)
		}
	}
	return columns, rows.Err()
}

// normalizeColumn re-encodes every non-NULL value in table.column through
// parseTime/formatTime, writing back only the rows whose canonical
// re-encoding differs from what is stored. It fully drains and closes its
// read before issuing any UPDATE, since both share tx's single
// connection.
func normalizeColumn(ctx context.Context, tx *sql.Tx, table, column string) (normalized int, skipped int, err error) {
	ident := quoteIdent(column)
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid, %s FROM %s WHERE %s IS NOT NULL`, ident, quoteIdent(table), ident))
	if err != nil {
		return 0, 0, err
	}

	type pendingUpdate struct {
		rowID     int64
		canonical string
	}
	var updates []pendingUpdate
	for rows.Next() {
		var rowID int64
		var value string
		if err := rows.Scan(&rowID, &value); err != nil {
			rows.Close()
			return 0, 0, err
		}
		t, err := parseTime(value)
		if err != nil {
			skipped++
			continue
		}
		if canonical := formatTime(t); canonical != value {
			updates = append(updates, pendingUpdate{rowID: rowID, canonical: canonical})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	updateSQL := fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, quoteIdent(table), ident)
	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, updateSQL, u.canonical, u.rowID); err != nil {
			return normalized, skipped, err
		}
		normalized++
	}
	return normalized, skipped, nil
}

// quoteIdent double-quote-quotes a SQLite identifier sourced only from
// this database's own sqlite_master/PRAGMA table_info — never from
// external input — escaping an embedded quote per SQLite's identifier
// syntax so a table or column name that happens to collide with a
// reserved word still works.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
