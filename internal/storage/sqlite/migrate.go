package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
}

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

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
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
