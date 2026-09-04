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
	"actors", "miauth_local_sessions", "api_tokens", "threads", "entries", "user_tags", "llm_classifications",
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
	migrations, err := loadMigrations(migrationsFS, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	wantVersions := make([]int, len(migrations))
	for i, migration := range migrations {
		wantVersions[i] = migration.version
	}
	if len(versions) != len(wantVersions) {
		t.Fatalf("applied migrations = %v, want %v", versions, wantVersions)
	}
	for i, v := range versions {
		if v != wantVersions[i] {
			t.Fatalf("applied migrations = %v, want %v", versions, wantVersions)
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
	for _, dropped := range []string{"upstream_tokens", "owner_bindings", "miauth_upstream_sessions", "bootstrap_gates"} {
		var name string
		err := db.sqlDB.QueryRowContext(t.Context(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, dropped).Scan(&name)
		if err == nil {
			t.Errorf("legacy table %s exists in fresh schema", dropped)
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
	// Simulate a database on the old upstream-verification schema.
	for _, m := range migrations {
		if m.version > 8 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	var name string
	const ownerID = "existing-owner"
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'owner', '2024-01-01T00:00:00Z')`, ownerID); err != nil {
		t.Fatalf("seed owner actor: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO owner_bindings (id, local_actor_id, identity_origin, upstream_user_id, bound_at)
		 VALUES (1, ?, 'https://misskey.example', 'owner-upstream', '2024-01-01T00:00:00Z')`, ownerID); err != nil {
		t.Fatalf("seed owner binding: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO miauth_local_sessions
		 (route_session_id, state, status, requested_permissions, local_actor_id, created_at, expires_at, consumed_at)
		 VALUES ('existing-session', 'legacy-state', 'consumed', 'read:account', ?,
		 '2024-01-01T00:00:00Z', '2024-01-01T00:10:00Z', '2024-01-01T00:01:00Z')`, ownerID); err != nil {
		t.Fatalf("seed local session: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO api_tokens
		 (id, token_hash, local_actor_id, miauth_local_session_id, scopes, created_at)
		 VALUES ('existing-token', 'existing-hash', ?, 'existing-session', 'read:account', '2024-01-01T00:01:00Z')`, ownerID); err != nil {
		t.Fatalf("seed API token: %v", err)
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM actors WHERE actor_type = 'owner'`).Scan(&name); err != nil {
		t.Fatalf("owner actor should survive migration 10: %v", err)
	}
	if name != ownerID {
		t.Fatalf("owner actor id = %q, want %q", name, ownerID)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM api_tokens WHERE id = 'existing-token'`).Scan(&name); err != nil {
		t.Fatalf("local API token should survive migration 10: %v", err)
	}
	for _, dropped := range []string{"upstream_tokens", "owner_bindings", "miauth_upstream_sessions", "bootstrap_gates"} {
		err := sqlDB.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, dropped).Scan(&name)
		if err == nil {
			t.Errorf("legacy table %s still exists", dropped)
		}
	}

	var count int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) {
		t.Errorf("schema_migrations count = %d, want %d", count, len(migrations))
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
