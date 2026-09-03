package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

var expectedTables = []string{
	"actors", "owner_bindings", "upstream_tokens", "bootstrap_gates", "miauth_local_sessions",
	"miauth_upstream_sessions", "api_tokens", "threads", "entries", "user_tags", "llm_classifications",
	"llm_classification_tags", "llm_classification_related_entries", "jobs", "llm_generations",
	"external_sources", "external_items",
}

func TestMigrate_FreshDatabase(t *testing.T) {
	db := newTestDB(t)

	rows, err := db.sqlDB.QueryContext(t.Context(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if len(versions) != 8 {
		t.Fatalf("applied migration count = %d, want 8 (versions: %v)", len(versions), versions)
	}
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("applied migrations = %v, want 1..8 in order", versions)
		}
	}

	for _, table := range expectedTables {
		var name string
		err := db.sqlDB.QueryRowContext(t.Context(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s was not created: %v", table, err)
		}
	}
}

func TestMigrate_UpgradeAppliesRemainingMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=1")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	ctx := t.Context()
	if _, err := sqlDB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}

	migrations, err := loadMigrations(migrationsFS, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 8 {
		t.Fatalf("embedded migration count = %d, want 8", len(migrations))
	}

	// Simulate a database already migrated up to version 7, one release
	// behind the embedded schema.
	for _, m := range migrations {
		if m.version > 7 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	var name string
	err = sqlDB.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'external_sources'`).Scan(&name)
	if err == nil {
		t.Fatal("external_sources should not exist before migration 8 is applied")
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := sqlDB.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'external_sources'`).Scan(&name); err != nil {
		t.Fatalf("external_sources should exist after migration 8 is applied: %v", err)
	}

	var count int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Errorf("schema_migrations count = %d, want 8", count)
	}
}

// TestApplyAll_RejectsAppliedVersionMissingFromEmbeddedFiles backs the
// reverse direction of the checksum check above: applyAll must also
// notice when schema_migrations records a version as already applied but
// the filesystem it is handed no longer has a migration file for that
// version (for example, one was deleted by mistake after being deployed).
// Silently starting up in that state would contradict
// docs/operations/configuration.md's "enforced mechanically" claim.
func TestApplyAll_RejectsAppliedVersionMissingFromEmbeddedFiles(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)},
	}
	db := openMemoryDB(t)

	if _, err := db.ExecContext(t.Context(), `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (2, 'deadbeef', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}

	err := applyAll(t.Context(), db, files, "migrations")
	if err == nil {
		t.Fatal("expected an error for an applied version with no corresponding embedded migration file")
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Errorf("error %q does not name the missing version", err.Error())
	}
}

func TestMigrate_RejectsEditedAppliedMigration(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()

	if _, err := db.sqlDB.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}

	err := db.Migrate(ctx)
	if err == nil {
		t.Fatal("expected an error for a tampered applied-migration checksum")
	}
	if !strings.Contains(err.Error(), "0001_actors") {
		t.Errorf("error %q does not name the tampered migration", err.Error())
	}
}
