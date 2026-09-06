package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

// migration is one parsed, checksummed embedded migration file.
type migration struct {
	version  int
	name     string
	sql      string
	checksum string
	// rebuild marks a migration that carries the rebuildDirective and
	// therefore needs applyRebuild's foreign-keys-off connection rather
	// than the ordinary in-transaction path. See rebuildDirective.
	rebuild bool
}

// rebuildDirective marks a migration that performs SQLite's documented
// twelve-step table rebuild (CREATE new / INSERT SELECT / DROP old /
// ALTER TABLE RENAME), the only way to change a CHECK or table-level
// UNIQUE constraint on an existing table.
//
// Such a migration cannot run on the ordinary path: DROP TABLE with
// foreign keys enabled behaves like a DELETE of every row for
// constraint purposes, so dropping a parent table (actors, say) fails
// against its children, and "PRAGMA foreign_keys" is a documented no-op
// inside a transaction, so the migration file cannot turn them off
// itself. applyRebuild instead takes a dedicated connection, disables
// foreign keys outside the transaction, and re-verifies the result with
// PRAGMA foreign_key_check before committing.
//
// The directive must appear on its own line within the migration file's
// leading comment block. It is part of the file contents, so it is
// covered by the migration checksum like everything else.
const rebuildDirective = "-- migrate:rebuild"

// Migrate applies every embedded, not-yet-applied migration to the
// database in ascending version order, each inside its own transaction so
// a failing statement rolls back cleanly without partially applying.
//
// If an already-applied migration's embedded contents no longer match the
// checksum recorded when it was applied, Migrate fails instead of
// re-applying or ignoring it: this is the mechanical enforcement behind
// "an applied migration is never edited, add a new forward migration
// instead" (see docs/operations/configuration.md and AGENTS.md).
func (d *DB) Migrate(ctx context.Context) error {
	return applyAll(ctx, d.sqlDB, migrationsFS, migrationsDir)
}

// applyAll implements Migrate against an explicit fs.FS and directory so
// tests can exercise the ordering/checksum logic against fstest.MapFS
// fixtures independent of the real embedded schema.
func applyAll(ctx context.Context, db *sql.DB, files fs.FS, dir string) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	migrations, err := loadMigrations(files, dir)
	if err != nil {
		return err
	}

	applied, err := appliedChecksums(ctx, db)
	if err != nil {
		return err
	}

	// A version recorded as applied must still have a corresponding
	// embedded migration file: if the file was deleted, this is the only
	// mechanical check that notices, since the version simply would not
	// appear in migrations at all and the loop below would otherwise
	// start up successfully without ever having reapplied or verified it.
	loadedVersions := make(map[int]bool, len(migrations))
	for _, m := range migrations {
		loadedVersions[m.version] = true
	}
	for version := range applied {
		if !loadedVersions[version] {
			return fmt.Errorf(
				"schema_migrations records version %d as applied, but no embedded migration file provides that version; an applied migration must never be removed",
				version,
			)
		}
	}

	for _, m := range migrations {
		if existing, ok := applied[m.version]; ok {
			if existing != m.checksum {
				return fmt.Errorf(
					"migration %04d_%s: embedded contents no longer match the checksum recorded when it was applied (%s); an applied migration must never be edited, add a new forward migration instead",
					m.version, m.name, existing,
				)
			}
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func loadMigrations(files fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationFilename(e.Name())
		if err != nil {
			return nil, err
		}
		contents, err := fs.ReadFile(files, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(contents)
		migrations = append(migrations, migration{
			version:  version,
			name:     name,
			sql:      string(contents),
			checksum: hex.EncodeToString(sum[:]),
			rebuild:  hasRebuildDirective(string(contents)),
		})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })

	for i := 1; i < len(migrations); i++ {
		if migrations[i].version == migrations[i-1].version {
			return nil, fmt.Errorf(
				"duplicate migration version %d (%s and %s)",
				migrations[i].version, migrations[i-1].name, migrations[i].name,
			)
		}
	}
	return migrations, nil
}

// parseMigrationFilename requires the "NNNN_name.sql" convention (for
// example "0001_actors.sql") so lexical file listing order and numeric
// version order always agree.
func parseMigrationFilename(filename string) (version int, name string, err error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, rest, ok := strings.Cut(base, "_")
	if !ok || rest == "" {
		return 0, "", fmt.Errorf("migration filename %q must be in the form NNNN_name.sql", filename)
	}
	version, convErr := strconv.Atoi(prefix)
	if convErr != nil || version < 1 {
		return 0, "", fmt.Errorf("migration filename %q must start with a positive numeric version", filename)
	}
	return version, rest, nil
}

// hasRebuildDirective reports whether contents opens with the
// rebuildDirective somewhere in its leading comment block. Only that
// block is scanned so the marker cannot be smuggled in by a comment
// buried among the statements, where a reviewer would not look for it.
func hasRebuildDirective(contents string) bool {
	for line := range strings.Lines(contents) {
		trimmed := strings.TrimSpace(line)
		if trimmed == rebuildDirective {
			return true
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			return false
		}
	}
	return false
}

func appliedChecksums(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]string{}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = checksum
	}
	return applied, rows.Err()
}

// applyOne applies one not-yet-applied migration and records it in
// schema_migrations, in the same transaction so the two can never
// disagree. A migration carrying the rebuildDirective takes applyRebuild
// instead; everything else runs on the ordinary pooled-connection path.
func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	if m.rebuild {
		return applyRebuild(ctx, db, m)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("execute: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, ?)`,
		m.version, m.checksum, formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit()
}

// applyRebuild applies a rebuildDirective migration following SQLite's
// documented procedure for altering a table's constraints: disable
// foreign keys, do the CREATE/INSERT/DROP/RENAME inside a transaction,
// re-check every remaining foreign key, then commit and re-enable.
//
// The PRAGMAs must run outside the transaction (inside one they are
// silently ignored) and on a connection nothing else is using, which is
// why this checks out one *sql.Conn for the whole rebuild instead of
// issuing statements against the pool: a concurrent caller must never be
// handed a connection with foreign keys switched off. Nothing else runs
// against the database during startup migration, so holding one
// connection here costs nothing.
//
// PRAGMA foreign_key_check is what keeps "foreign keys are off" from
// meaning "foreign keys are unenforced": if the rebuilt table lost a row
// some child still references, the check reports it and the whole
// migration rolls back, leaving schema_migrations untouched so the next
// startup retries rather than proceeding on a corrupt schema.
func applyRebuild(ctx context.Context, db *sql.DB, m migration) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire dedicated connection: %w", err)
	}
	defer func() {
		// context.WithoutCancel: the restore must still run when the
		// migration failed because ctx was cancelled, otherwise the
		// connection would return to the pool with foreign keys off.
		restoreCtx := context.WithoutCancel(ctx)
		if _, restoreErr := conn.ExecContext(restoreCtx, `PRAGMA foreign_keys = ON`); restoreErr != nil {
			// The connection's foreign-key state is now unknown, so it
			// must not be reused: returning driver.ErrBadConn from Raw
			// is database/sql's documented way to discard it instead of
			// putting it back in the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("re-enable foreign keys: %w", restoreErr)
			}
			return
		}
		_ = conn.Close()
	}()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("execute: %w", err)
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, ?)`,
		m.version, m.checksum, formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit()
}

// foreignKeyCheck fails if any row in the database violates a foreign key
// constraint, naming which child-to-parent relationships broke so a
// failed rebuild is diagnosable from the startup error alone.
//
// The pragma reports one row per violating row, which on a badly broken
// rebuild could be the whole table; only the distinct table pairs are
// collected (bounded by the schema, not by the data) while every row is
// counted. No row values are read, so nothing user-authored can reach
// the error message or the logs.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()

	total := 0
	seen := map[string]bool{}
	var relations []string
	for rows.Next() {
		// The pragma's columns are (table, rowid, parent, fkid); rowid is
		// NULL for WITHOUT ROWID tables, so it is scanned as nullable.
		var table, parent string
		var rowID sql.NullInt64
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return fmt.Errorf("scan foreign key violation: %w", err)
		}
		total++
		relation := table + " -> " + parent
		if !seen[relation] {
			seen[relation] = true
			relations = append(relations, relation)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	if total > 0 {
		return fmt.Errorf(
			"rebuild left %d foreign key violation(s) in %s; rolling back",
			total, strings.Join(relations, ", "),
		)
	}
	return nil
}
