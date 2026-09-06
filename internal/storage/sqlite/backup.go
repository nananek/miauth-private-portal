package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
)

// backupTables are the tables backupctl verify summarizes by row count.
// This is deliberately a small, fixed set (not every table in the
// schema): enough for an operator to sanity-check that a backup or
// restored database has the data they expect, not a full schema dump.
var backupTables = []string{"actors", "entries", "jobs", "external_sources"}

// Backup writes a consistent, point-in-time snapshot of this database to
// destPath using SQLite's VACUUM INTO. Unlike copying the underlying
// file, VACUUM INTO is safe to run against a live database in WAL mode
// without stopping the server or coordinating with concurrent
// readers/writers: it reads through SQLite's own MVCC snapshot rather
// than the raw file (which, under WAL, only reflects the last
// checkpoint and does not include the separate "-wal"/"-shm" side
// files a plain file copy would miss). VACUUM INTO refuses to
// overwrite an existing file, so destPath must not already exist.
//
// destPath is passed as a bound parameter, not concatenated into the SQL
// text: verified empirically against this repository's pinned
// modernc.org/sqlite driver to support this (including paths containing
// a single quote), so no manual escaping is needed.
func (d *DB) Backup(ctx context.Context, destPath string) error {
	if _, err := d.sqlDB.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("vacuum into %s: %w", destPath, err)
	}
	return nil
}

// OpenReadOnly opens the SQLite database file at path without ever
// writing to it (SQLite's "mode=ro" URI parameter) and without creating
// it if missing, for tools such as backupctl verify that must inspect a
// database file — often itself a backup, which must never be mutated by
// the act of checking it — rather than operate the live server's
// database.
func OpenReadOnly(ctx context.Context, path string) (*DB, error) {
	// See Open's doc comment: cfg.Path must be percent-encoded before
	// being embedded in the "file:" DSN, since modernc.org/sqlite passes
	// it through to SQLite's own URI parser.
	u := url.URL{Path: path}
	dsn := fmt.Sprintf("file:%s?mode=ro", u.EscapedPath())
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}
	return &DB{sqlDB: sqlDB, Repos: newRepos(sqlDB)}, nil
}

// VerifyMigrations checks, without writing anything to the database,
// that schema_migrations already records every embedded migration with
// its expected checksum. Unlike Migrate, it never applies a missing
// migration: a database this is meant for (a backup, or a database
// freshly restored from one) is expected to already be fully migrated,
// since the server always migrates before serving traffic, so a missing
// or mismatched version here is itself the failure to report.
//
// It reuses Migrate's own loadMigrations/appliedChecksums helpers rather
// than re-deriving the comparison, so the two can never drift apart.
func (d *DB) VerifyMigrations(ctx context.Context) error {
	return verifyMigrations(ctx, d.sqlDB, migrationsFS, migrationsDir)
}

func verifyMigrations(ctx context.Context, db *sql.DB, files fs.FS, dir string) error {
	migrations, err := loadMigrations(files, dir)
	if err != nil {
		return err
	}
	applied, err := appliedChecksums(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for _, m := range migrations {
		existing, ok := applied[m.version]
		if !ok {
			return fmt.Errorf(
				"migration %04d_%s is not recorded as applied in schema_migrations; this database is not fully migrated",
				m.version, m.name,
			)
		}
		if existing != m.checksum {
			return fmt.Errorf(
				"migration %04d_%s: embedded contents no longer match the checksum recorded when it was applied (%s)",
				m.version, m.name, existing,
			)
		}
	}
	return nil
}

// TableCounts returns the current row count of each table in
// backupTables, for backupctl verify's operator-facing summary.
func (d *DB) TableCounts(ctx context.Context) (map[string]int, error) {
	counts := make(map[string]int, len(backupTables))
	for _, table := range backupTables {
		var n int
		// table always comes from the backupTables constant above, never
		// from caller input, so building the query string this way carries
		// no injection risk despite not using a bound parameter (SQLite
		// does not support binding identifiers, only values).
		if err := d.sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = n
	}
	return counts, nil
}

// IntegrityCheck runs SQLite's PRAGMA integrity_check, a full scan of
// every table and index. It is deliberately not part of
// VerifyMigrations or the sqlite.Checker readiness probe (too expensive
// to run on every deploy or readiness poll); backupctl verify only runs
// it behind its opt-in --deep flag.
func (d *DB) IntegrityCheck(ctx context.Context) error {
	rows, err := d.sqlDB.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("run integrity check: %w", err)
	}
	defer rows.Close()

	var problems []string
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			return fmt.Errorf("scan integrity check result: %w", err)
		}
		if message != "ok" {
			problems = append(problems, message)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read integrity check results: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("integrity check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}
