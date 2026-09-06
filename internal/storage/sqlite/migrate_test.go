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
	"external_sources", "external_items", "reactions", "mentions", "notifications",
	"openwebui_workspaces", "openwebui_models", "openwebui_conversation_links", "openwebui_turn_links",
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

// TestMigrate_UpgradeAppliesExternalSourceCursorColumns backs migration
// 0011: a pre-existing external_sources row created before 0011 applied
// must survive with the new columns defaulted (cursor/last_fetched_at/
// last_error NULL, consecutive_failures 0), and a fresh database must
// expose the same columns from the start.
func TestMigrate_UpgradeAppliesExternalSourceCursorColumns(t *testing.T) {
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
	for _, m := range migrations {
		if m.version > 10 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	const sourceID = "pre-existing-source"
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO external_sources (id, kind, uri, created_at) VALUES (?, 'rss', 'https://example.com/feed.xml', '2024-01-01T00:00:00Z')`,
		sourceID,
	); err != nil {
		t.Fatalf("seed external source: %v", err)
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var cursor, lastFetchedAt, lastError sql.NullString
	var consecutiveFailures int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT cursor, last_fetched_at, last_error, consecutive_failures FROM external_sources WHERE id = ?`, sourceID,
	).Scan(&cursor, &lastFetchedAt, &lastError, &consecutiveFailures); err != nil {
		t.Fatalf("query upgraded row: %v", err)
	}
	if cursor.Valid || lastFetchedAt.Valid || lastError.Valid {
		t.Errorf("upgraded row has non-NULL cursor columns: cursor=%v last_fetched_at=%v last_error=%v", cursor, lastFetchedAt, lastError)
	}
	if consecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", consecutiveFailures)
	}
}

// TestMigrate_UpgradeAppliesActorDisplayNameColumn backs migration 0012
// (Issue #23 PR1): a pre-existing actors row created before 0012 applied
// must survive with display_name defaulted to NULL (meaning "never
// explicitly set" — see actorRepository.SetDisplayName's doc comment),
// and a fresh database must expose the same column from the start
// (covered generically by TestMigrate_FreshDatabase above).
func TestMigrate_UpgradeAppliesActorDisplayNameColumn(t *testing.T) {
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
	for _, m := range migrations {
		if m.version > 11 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	const ownerID = "pre-existing-owner"
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'owner', '2024-01-01T00:00:00Z')`, ownerID,
	); err != nil {
		t.Fatalf("seed owner actor: %v", err)
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var displayName sql.NullString
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT display_name FROM actors WHERE id = ?`, ownerID,
	).Scan(&displayName); err != nil {
		t.Fatalf("query upgraded row: %v", err)
	}
	if displayName.Valid {
		t.Errorf("upgraded row has non-NULL display_name: %v", displayName)
	}
}

// TestMigrate_UpgradeAppliesReactionsTable backs migration 0013 (Issue
// #23 PR4): unlike 0012's ALTER TABLE, this is a brand-new table with no
// pre-existing rows to preserve, so the upgrade test's job is only to
// confirm the table and its UNIQUE(entry_id, reactor_actor_id) constraint
// exist and work after upgrading from a pre-0013 database, mirroring
// TestMigrate_UpgradeAppliesExternalSourceCursorColumns' structure.
func TestMigrate_UpgradeAppliesReactionsTable(t *testing.T) {
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
	for _, m := range migrations {
		if m.version > 12 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	const ownerID = "pre-existing-owner"
	const entryID = "pre-existing-entry"
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'owner', '2024-01-01T00:00:00Z')`, ownerID,
	); err != nil {
		t.Fatalf("seed owner actor: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO threads (id, created_at, updated_at) VALUES (?, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, entryID,
	); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO entries (id, thread_id, kind, author_actor_id, body, processing_status, created_at, updated_at)
		 VALUES (?, ?, 'user_post', ?, 'body', 'none', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		entryID, entryID, ownerID,
	); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO reactions (id, entry_id, reactor_actor_id, emoji, created_at) VALUES ('r1', ?, ?, '👍', '2024-01-01T00:00:00Z')`,
		entryID, ownerID,
	); err != nil {
		t.Fatalf("insert reaction after upgrade: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO reactions (id, entry_id, reactor_actor_id, emoji, created_at) VALUES ('r2', ?, ?, '❤️', '2024-01-01T00:00:01Z')`,
		entryID, ownerID,
	); err == nil {
		t.Error("second reaction from the same actor on the same entry should violate UNIQUE(entry_id, reactor_actor_id)")
	}
}

// TestMigrate_UpgradeAppliesMentionsTable backs migration 0014 (Issue #23
// PR5): another brand-new table with no pre-existing rows to preserve, so
// this upgrade test's job is only to confirm the table (and its foreign
// keys into entries/actors) exist and are usable after upgrading from a
// pre-0014 database, mirroring TestMigrate_UpgradeAppliesReactionsTable's
// structure.
func TestMigrate_UpgradeAppliesMentionsTable(t *testing.T) {
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
	for _, m := range migrations {
		if m.version > 13 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	const ownerID = "pre-existing-owner"
	const entryID = "pre-existing-entry"
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'owner', '2024-01-01T00:00:00Z')`, ownerID,
	); err != nil {
		t.Fatalf("seed owner actor: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO threads (id, created_at, updated_at) VALUES (?, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, entryID,
	); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO entries (id, thread_id, kind, author_actor_id, body, processing_status, created_at, updated_at)
		 VALUES (?, ?, 'user_post', ?, '@owner body', 'none', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		entryID, entryID, ownerID,
	); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The pre-existing entry's "@owner" body is never backfilled into
	// mentions (Issue #23 PR5's forward-only scope, owner-confirmed
	// 2026-09-06): inserting a mention row after upgrade must still work
	// against it, but the migration itself must not have created one.
	var mentionCount int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mentions`).Scan(&mentionCount); err != nil {
		t.Fatalf("count mentions after upgrade: %v", err)
	}
	if mentionCount != 0 {
		t.Errorf("mentions after upgrade = %d, want 0 (no backfill)", mentionCount)
	}

	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO mentions (id, entry_id, mentioned_actor_id, created_at) VALUES ('m1', ?, ?, '2024-01-01T00:00:00Z')`,
		entryID, ownerID,
	); err != nil {
		t.Fatalf("insert mention after upgrade: %v", err)
	}
}

// TestMigrate_UpgradeAppliesNotificationsTable backs migration 0015
// (Issue #23 PR6): another brand-new table with no pre-existing rows to
// preserve, mirroring TestMigrate_UpgradeAppliesReactionsTable/
// TestMigrate_UpgradeAppliesMentionsTable's structure.
func TestMigrate_UpgradeAppliesNotificationsTable(t *testing.T) {
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
	for _, m := range migrations {
		if m.version > 14 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	const ownerID = "pre-existing-owner"
	const entryID = "pre-existing-entry"
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'owner', '2024-01-01T00:00:00Z')`, ownerID,
	); err != nil {
		t.Fatalf("seed owner actor: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO threads (id, created_at, updated_at) VALUES (?, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, entryID,
	); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO entries (id, thread_id, kind, author_actor_id, body, processing_status, created_at, updated_at)
		 VALUES (?, ?, 'user_post', ?, 'body', 'none', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		entryID, entryID, ownerID,
	); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO notifications (id, type, related_entry_id, created_at) VALUES ('n1', 'reply', ?, '2024-01-01T00:00:00Z')`,
		entryID,
	); err != nil {
		t.Fatalf("insert notification after upgrade: %v", err)
	}
}

// TestMigrate_UpgradeRebuildsActorsForOpenWebUIModelType backs migration
// 0016 (Issue #52 PR1), the first migration in this repository that
// rebuilds an existing table rather than adding to it. Unlike the
// new-table upgrades above, this one has real rows and real children to
// preserve, so the test seeds one of every kind of row that references
// actors before upgrading and checks all of them survive with foreign
// keys still intact — the failure mode a rebuild done with foreign keys
// simply switched off would otherwise hide.
func TestMigrate_UpgradeRebuildsActorsForOpenWebUIModelType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=1&_txlock=immediate")
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
	for _, m := range migrations {
		if m.version > 15 {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}

	const (
		ownerID     = "pre-existing-owner"
		assistantID = "pre-existing-assistant"
		systemID    = "pre-existing-system"
		entryID     = "pre-existing-entry"
		sessionID   = "pre-existing-session"
		tokenID     = "pre-existing-token"
	)
	for _, seed := range []struct {
		what string
		sql  string
		args []any
	}{
		{"owner actor", `INSERT INTO actors (id, actor_type, created_at, display_name) VALUES (?, 'owner', '2024-01-01T00:00:00Z', 'Owner Name')`, []any{ownerID}},
		{"assistant actor", `INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'assistant', '2024-01-01T00:00:01Z')`, []any{assistantID}},
		{"system actor", `INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'system', '2024-01-01T00:00:02Z')`, []any{systemID}},
		{"thread", `INSERT INTO threads (id, created_at, updated_at) VALUES (?, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, []any{entryID}},
		{"entry", `INSERT INTO entries (id, thread_id, kind, author_actor_id, body, processing_status, created_at, updated_at)
		 VALUES (?, ?, 'user_post', ?, 'body', 'none', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, []any{entryID, entryID, ownerID}},
		{"reaction", `INSERT INTO reactions (id, entry_id, reactor_actor_id, emoji, created_at) VALUES ('r1', ?, ?, '👍', '2024-01-01T00:00:00Z')`, []any{entryID, ownerID}},
		{"mention", `INSERT INTO mentions (id, entry_id, mentioned_actor_id, created_at) VALUES ('m1', ?, ?, '2024-01-01T00:00:00Z')`, []any{entryID, ownerID}},
		{"local MiAuth session", `INSERT INTO miauth_local_sessions
		 (route_session_id, state, status, requested_permissions, local_actor_id, created_at, expires_at)
		 VALUES (?, 'legacy-state', 'consumed', 'read:account', ?, '2024-01-01T00:00:00Z', '2024-01-01T00:10:00Z')`, []any{sessionID, ownerID}},
		{"API token", `INSERT INTO api_tokens (id, token_hash, local_actor_id, miauth_local_session_id, scopes, created_at)
		 VALUES (?, 'existing-hash', ?, ?, 'read:account', '2024-01-01T00:01:00Z')`, []any{tokenID, ownerID, sessionID}},
	} {
		if _, err := sqlDB.ExecContext(ctx, seed.sql, seed.args...); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Every actor row survives, display_name included: a rebuild that
	// forgot to carry a column across would still migrate "successfully".
	var displayName sql.NullString
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT display_name FROM actors WHERE id = ?`, ownerID).Scan(&displayName); err != nil {
		t.Fatalf("owner actor should survive the rebuild: %v", err)
	}
	if !displayName.Valid || displayName.String != "Owner Name" {
		t.Errorf("owner display_name after rebuild = %v, want %q", displayName, "Owner Name")
	}
	for _, id := range []string{assistantID, systemID} {
		var got string
		if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM actors WHERE id = ?`, id).Scan(&got); err != nil {
			t.Errorf("actor %s should survive the rebuild: %v", id, err)
		}
	}

	// Nothing that pointed at an actor row was orphaned by the DROP.
	for _, child := range []struct {
		what  string
		query string
		arg   string
	}{
		{"entry", `SELECT author_actor_id FROM entries WHERE id = ?`, entryID},
		{"reaction", `SELECT reactor_actor_id FROM reactions WHERE id = ?`, "r1"},
		{"mention", `SELECT mentioned_actor_id FROM mentions WHERE id = ?`, "m1"},
		{"local MiAuth session", `SELECT local_actor_id FROM miauth_local_sessions WHERE route_session_id = ?`, sessionID},
		{"API token", `SELECT local_actor_id FROM api_tokens WHERE id = ?`, tokenID},
	} {
		var actorID string
		if err := sqlDB.QueryRowContext(ctx, child.query, child.arg).Scan(&actorID); err != nil {
			t.Errorf("%s should survive the rebuild: %v", child.what, err)
			continue
		}
		if actorID != ownerID {
			t.Errorf("%s actor reference = %q, want %q", child.what, actorID, ownerID)
		}
	}

	rows, err := sqlDB.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("PRAGMA foreign_key_check reported a violation after the actors rebuild")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at) VALUES ('second-owner', 'owner', '2024-01-02T00:00:00Z')`,
	); err == nil {
		t.Error("a second owner must still be rejected by idx_actors_singleton_type after the rebuild")
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO actors (id, actor_type, created_at) VALUES ('bogus', 'openwebui_workspace', '2024-01-02T00:00:00Z')`,
	); err == nil {
		t.Error("an unknown actor_type must still be rejected by the rebuilt CHECK constraint")
	}

	// The point of the rebuild: openwebui_model rows are accepted, and
	// unlike the reserved three they are not singletons.
	for _, id := range []string{"virtual-1", "virtual-2"} {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'openwebui_model', '2024-01-02T00:00:00Z')`, id,
		); err != nil {
			t.Errorf("insert openwebui_model actor %s after upgrade: %v", id, err)
		}
	}
}

// openUpgradeDB opens a fresh file-backed database with only the
// migrations up to and including throughVersion applied, so an upgrade
// test can seed pre-existing rows the way a deployed database would have
// them before the migration under test runs.
func openUpgradeDB(t *testing.T, throughVersion int) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=1&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

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
	for _, m := range migrations {
		if m.version > throughVersion {
			continue
		}
		if err := applyOne(ctx, sqlDB, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}
	return sqlDB
}

// TestMigrate_UpgradeAppliesOpenWebUIRegistryTables backs migration 0017
// (Issue #52 PR2). Like the reactions/mentions/notifications upgrades
// above these are brand-new tables with no pre-existing rows to
// preserve, so the test's job is the constraints — in particular the
// composite foreign key, which is the only thing making "a workspace's
// default model belongs to that workspace" true rather than merely
// intended.
func TestMigrate_UpgradeAppliesOpenWebUIRegistryTables(t *testing.T) {
	sqlDB := openUpgradeDB(t, 16)
	ctx := t.Context()

	const (
		actorA = "virtual-actor-a"
		actorB = "virtual-actor-b"
	)
	for _, id := range []string{actorA, actorB} {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO actors (id, actor_type, created_at) VALUES (?, 'openwebui_model', '2024-01-01T00:00:00Z')`, id,
		); err != nil {
			t.Fatalf("seed VirtualActor %s: %v", id, err)
		}
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	insertWorkspace := func(id, baseURL string) error {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO openwebui_workspaces (id, name, base_url, secret_ref, presentation_host, created_at, updated_at)
			 VALUES (?, 'Open WebUI', ?, 'OPENWEBUI_API_KEY', 'openwebui.example.net',
			 '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, id, baseURL)
		return err
	}
	if err := insertWorkspace("w1", "https://a.example.net"); err != nil {
		t.Fatalf("insert workspace after upgrade: %v", err)
	}
	if err := insertWorkspace("w2", "https://b.example.net"); err != nil {
		t.Fatalf("insert second workspace after upgrade: %v", err)
	}
	if err := insertWorkspace("w3", "https://a.example.net"); err == nil {
		t.Error("a duplicate base_url should violate UNIQUE(base_url)")
	}

	// The gates and capability statuses default to the cautious values:
	// nothing about the target is assumed before it is observed.
	var enabled, generationEnabled int
	var chatCreate, chatContinue, capabilities string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT enabled, generation_enabled, chat_create_status, chat_continue_status
		 FROM openwebui_workspaces WHERE id = 'w1'`,
	).Scan(&enabled, &generationEnabled, &chatCreate, &chatContinue); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || generationEnabled != 0 || chatCreate != "unverified" || chatContinue != "unverified" {
		t.Errorf("workspace defaults = %d/%d/%q/%q, want 0/0/unverified/unverified",
			enabled, generationEnabled, chatCreate, chatContinue)
	}

	insertModel := func(id, workspaceID, externalID, slug, actorID string) error {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO openwebui_models (id, workspace_id, external_model_id, display_name, actor_slug, actor_id,
				created_at, updated_at)
			 VALUES (?, ?, ?, 'Display', ?, ?, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
			id, workspaceID, externalID, slug, actorID)
		return err
	}
	if err := insertModel("m1", "w1", "gpt-oss:20b", "model", actorA); err != nil {
		t.Fatalf("insert model after upgrade: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT capabilities FROM openwebui_models WHERE id = 'm1'`).Scan(&capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities != "{}" {
		t.Errorf("model capabilities default = %q, want %q", capabilities, "{}")
	}
	if err := insertModel("m2", "w1", "gpt-oss:20b", "other", actorB); err == nil {
		t.Error("a duplicate (workspace_id, external_model_id) should be rejected")
	}
	if err := insertModel("m3", "w1", "other-model", "other", actorA); err == nil {
		t.Error("a second model on the same actor should violate UNIQUE(actor_id)")
	}
	if err := insertModel("m4", "w1", "other-model", "model", actorB); err == nil {
		t.Error("a duplicate (workspace_id, actor_slug) should be rejected")
	}
	// The provider's id namespace is per instance, so the same external
	// id in another workspace is a different model.
	if err := insertModel("m5", "w2", "gpt-oss:20b", "model", actorB); err != nil {
		t.Errorf("the same external model id in another workspace should be allowed: %v", err)
	}

	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE openwebui_workspaces SET default_model_id = 'm1' WHERE id = 'w1'`); err != nil {
		t.Fatalf("point a workspace at its own model: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE openwebui_workspaces SET default_model_id = 'm1' WHERE id = 'w2'`); err == nil {
		t.Error("a model from another workspace must not be usable as a default")
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE openwebui_workspaces SET default_model_id = 'nope' WHERE id = 'w1'`); err == nil {
		t.Error("a nonexistent model must not be usable as a default")
	}
}

// TestMigrate_UpgradeAppliesOpenWebUILinkTables backs migration 0018
// (Issue #52 PR2). Its constraints are the local-identity rules
// ADR-0005 D2 depends on: the branch is identified by (thread_id,
// branch_id), a remote chat id is unique per workspace only when it is
// present at all, and the logical turn key is (link, local message,
// revision).
func TestMigrate_UpgradeAppliesOpenWebUILinkTables(t *testing.T) {
	sqlDB := openUpgradeDB(t, 17)
	ctx := t.Context()

	const (
		ownerID = "pre-existing-owner"
		actorID = "virtual-actor"
		// A thread's root entry shares the thread's ID (see the entries
		// table's (parent_entry_id IS NULL) = (id = thread_id) check).
		threadID = "pre-existing-thread"
		entryID  = threadID
	)
	for _, seed := range []struct {
		what string
		sql  string
	}{
		{"owner actor", `INSERT INTO actors (id, actor_type, created_at) VALUES ('` + ownerID + `', 'owner', '2024-01-01T00:00:00Z')`},
		{"VirtualActor", `INSERT INTO actors (id, actor_type, created_at) VALUES ('` + actorID + `', 'openwebui_model', '2024-01-01T00:00:00Z')`},
		{"thread", `INSERT INTO threads (id, created_at, updated_at) VALUES ('` + threadID + `', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"entry", `INSERT INTO entries (id, thread_id, kind, author_actor_id, body, processing_status, created_at, updated_at)
		 VALUES ('` + entryID + `', '` + threadID + `', 'user_post', '` + ownerID + `', 'body', 'none', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"workspace", `INSERT INTO openwebui_workspaces (id, name, base_url, secret_ref, presentation_host, created_at, updated_at)
		 VALUES ('w1', 'Open WebUI', 'https://a.example.net', 'OPENWEBUI_API_KEY', 'openwebui.example.net', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"model", `INSERT INTO openwebui_models (id, workspace_id, external_model_id, display_name, actor_slug, actor_id, created_at, updated_at)
		 VALUES ('m1', 'w1', 'gpt-oss:20b', 'Display', 'model', '` + actorID + `', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
	} {
		if _, err := sqlDB.ExecContext(ctx, seed.sql); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	insertLink := func(id, branchID, state string) error {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO openwebui_conversation_links (id, thread_id, branch_id, workspace_id, model_id, state,
				claimed_at, last_transition_at, created_at, updated_at)
			 VALUES (?, ?, ?, 'w1', 'm1', ?, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z',
			 '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, id, threadID, branchID, state)
		return err
	}
	if err := insertLink("l1", "branch-1", "creation_pending"); err != nil {
		t.Fatalf("insert link after upgrade: %v", err)
	}
	if err := insertLink("l2", "branch-1", "creation_pending"); err == nil {
		t.Error("a second link for the same (thread_id, branch_id) should be rejected")
	}
	if err := insertLink("l3", "branch-2", "unlinked"); err == nil {
		t.Error("'unlinked' is the absence of a row, not a state: the CHECK should reject it")
	}
	if err := insertLink("l4", "branch-2", "creation_pending"); err != nil {
		t.Fatalf("insert a second branch of the same thread: %v", err)
	}

	// Many links may have no remote chat; no two in a workspace may
	// share one.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE openwebui_conversation_links SET remote_chat_id = 'chat-1' WHERE id = 'l1'`); err != nil {
		t.Fatalf("record a remote chat: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE openwebui_conversation_links SET remote_chat_id = 'chat-1' WHERE id = 'l4'`); err == nil {
		t.Error("two links in one workspace should not be able to claim the same remote chat")
	}

	insertTurn := func(id, linkID, localMessageID, requestID string, revision int, status string) error {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO openwebui_turn_links (id, link_id, branch_id, local_message_id, request_id, revision,
				provider_status, created_at, updated_at)
			 VALUES (?, ?, 'branch-1', ?, ?, ?, ?, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
			id, linkID, localMessageID, requestID, revision, status)
		return err
	}
	if err := insertTurn("t1", "l1", entryID, "request-1", 1, "pending"); err != nil {
		t.Fatalf("insert turn after upgrade: %v", err)
	}
	if err := insertTurn("t2", "l1", entryID, "request-1", 2, "pending"); err == nil {
		t.Error("a duplicate request_id should be rejected")
	}
	if err := insertTurn("t3", "l1", entryID, "request-2", 1, "pending"); err == nil {
		t.Error("a duplicate (link_id, local_message_id, revision) should be rejected")
	}
	if err := insertTurn("t4", "l1", entryID, "request-3", 2, "pending"); err != nil {
		t.Errorf("a new revision of the same message should be allowed: %v", err)
	}
	if err := insertTurn("t5", "l1", entryID, "request-4", 3, "probably_fine"); err == nil {
		t.Error("an unknown provider_status should be rejected by the CHECK constraint")
	}
	if err := insertTurn("t6", "l1", "does-not-exist", "request-5", 1, "pending"); err == nil {
		t.Error("a turn naming a nonexistent entry should fail the foreign key")
	}

	// Both tables' defaults: a turn starts at its first revision with no
	// attempt made and no tombstone.
	var revision, attempt int
	var tombstonedAt sql.NullString
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT revision, attempt, tombstoned_at FROM openwebui_turn_links WHERE id = 't1'`,
	).Scan(&revision, &attempt, &tombstonedAt); err != nil {
		t.Fatal(err)
	}
	if revision != 1 || attempt != 0 || tombstonedAt.Valid {
		t.Errorf("turn defaults = revision %d, attempt %d, tombstoned_at %v; want 1, 0, NULL", revision, attempt, tombstonedAt)
	}

	rows, err := sqlDB.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("PRAGMA foreign_key_check reported a violation after the link migrations")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestMigrate_UpgradeAppliesOpenWebUITurnOutcomeColumns backs migration
// 0019 (Issue #53 PR1): an ALTER TABLE ADD COLUMN upgrade with a
// pre-existing turn row to preserve, mirroring
// TestMigrate_UpgradeAppliesExternalSourceCursorColumns' structure, plus
// the new idx_openwebui_links_state index.
func TestMigrate_UpgradeAppliesOpenWebUITurnOutcomeColumns(t *testing.T) {
	sqlDB := openUpgradeDB(t, 18)
	ctx := t.Context()

	const (
		ownerID  = "pre-existing-owner"
		actorID  = "virtual-actor"
		threadID = "pre-existing-thread"
		entryID  = threadID
	)
	for _, seed := range []struct {
		what string
		sql  string
	}{
		{"owner actor", `INSERT INTO actors (id, actor_type, created_at) VALUES ('` + ownerID + `', 'owner', '2024-01-01T00:00:00Z')`},
		{"VirtualActor", `INSERT INTO actors (id, actor_type, created_at) VALUES ('` + actorID + `', 'openwebui_model', '2024-01-01T00:00:00Z')`},
		{"thread", `INSERT INTO threads (id, created_at, updated_at) VALUES ('` + threadID + `', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"entry", `INSERT INTO entries (id, thread_id, kind, author_actor_id, body, processing_status, created_at, updated_at)
		 VALUES ('` + entryID + `', '` + threadID + `', 'user_post', '` + ownerID + `', 'body', 'none', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"workspace", `INSERT INTO openwebui_workspaces (id, name, base_url, secret_ref, presentation_host, created_at, updated_at)
		 VALUES ('w1', 'Open WebUI', 'https://a.example.net', 'OPENWEBUI_API_KEY', 'openwebui.example.net', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"model", `INSERT INTO openwebui_models (id, workspace_id, external_model_id, display_name, actor_slug, actor_id, created_at, updated_at)
		 VALUES ('m1', 'w1', 'gpt-oss:20b', 'Display', 'model', '` + actorID + `', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"link", `INSERT INTO openwebui_conversation_links (id, thread_id, branch_id, workspace_id, model_id, state,
			claimed_at, last_transition_at, created_at, updated_at)
		 VALUES ('l1', '` + threadID + `', 'branch-1', 'w1', 'm1', 'creation_pending',
		 '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
		{"turn", `INSERT INTO openwebui_turn_links (id, link_id, branch_id, local_message_id, request_id, revision,
			provider_status, created_at, updated_at)
		 VALUES ('t1', 'l1', 'branch-1', '` + entryID + `', 'request-1', 1, 'pending', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`},
	} {
		if _, err := sqlDB.ExecContext(ctx, seed.sql); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}

	db := &DB{sqlDB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var failureCategory, finishReason, lastAttemptAt, completedAt sql.NullString
	var promptTokens, completionTokens sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT failure_category, prompt_tokens, completion_tokens, finish_reason, last_attempt_at, completed_at
		 FROM openwebui_turn_links WHERE id = 't1'`,
	).Scan(&failureCategory, &promptTokens, &completionTokens, &finishReason, &lastAttemptAt, &completedAt); err != nil {
		t.Fatalf("query upgraded row: %v", err)
	}
	if failureCategory.Valid || promptTokens.Valid || completionTokens.Valid || finishReason.Valid || lastAttemptAt.Valid || completedAt.Valid {
		t.Errorf("upgraded turn row has non-NULL new columns: failure_category=%v prompt_tokens=%v completion_tokens=%v finish_reason=%v last_attempt_at=%v completed_at=%v",
			failureCategory, promptTokens, completionTokens, finishReason, lastAttemptAt, completedAt)
	}

	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE openwebui_turn_links SET failure_category = 'auth_failed', prompt_tokens = 1, completion_tokens = 2,
			finish_reason = 'stop', last_attempt_at = '2024-01-01T00:00:00Z', completed_at = '2024-01-01T00:00:00Z'
		 WHERE id = 't1'`); err != nil {
		t.Errorf("write the new columns after upgrade: %v", err)
	}

	var indexName string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_openwebui_links_state'`,
	).Scan(&indexName); err != nil {
		t.Errorf("idx_openwebui_links_state should exist after upgrade: %v", err)
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
