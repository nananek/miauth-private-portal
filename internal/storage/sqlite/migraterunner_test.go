package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

func openMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestApplyAll_OrdersByNumericVersionNotLexicalFilename(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0002_second.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE second (id INTEGER PRIMARY KEY);`)},
		"migrations/0010_tenth.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE tenth (id INTEGER PRIMARY KEY);`)},
		"migrations/0001_first.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE first (id INTEGER PRIMARY KEY);`)},
	}
	db := openMemoryDB(t)
	if err := applyAll(context.Background(), db, files, "migrations"); err != nil {
		t.Fatalf("applyAll: %v", err)
	}

	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
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
	want := []int{1, 2, 10}
	if len(versions) != len(want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Fatalf("versions = %v, want %v", versions, want)
		}
	}
}

func TestApplyAll_RejectsMalformedFilename(t *testing.T) {
	files := fstest.MapFS{
		"migrations/not-numbered.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE x (id INTEGER PRIMARY KEY);`)},
	}
	db := openMemoryDB(t)
	if err := applyAll(context.Background(), db, files, "migrations"); err == nil {
		t.Fatal("expected an error for a malformed migration filename")
	}
}

func TestApplyAll_RejectsDuplicateVersion(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)},
		"migrations/0001_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY);`)},
	}
	db := openMemoryDB(t)
	if err := applyAll(context.Background(), db, files, "migrations"); err == nil {
		t.Fatal("expected an error for a duplicate migration version")
	}
}

func TestApplyAll_BadSQLRollsBackWholeMigration(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_bad.sql": &fstest.MapFile{
			Data: []byte(`CREATE TABLE ok (id INTEGER PRIMARY KEY); THIS IS NOT VALID SQL;`),
		},
	}
	db := openMemoryDB(t)
	if err := applyAll(context.Background(), db, files, "migrations"); err == nil {
		t.Fatal("expected an error for invalid SQL")
	}

	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'ok'`).Scan(&name)
	if err == nil {
		t.Fatal("table created by the failed migration's earlier statement should have been rolled back")
	}
}

// openFileDB opens a fresh file-backed database with the same pragmas the
// real DSN applies. Rebuild migrations must not use openMemoryDB:
// "file::memory:" gives each pooled connection its own empty database,
// and applyRebuild deliberately asks the pool for a dedicated connection.
func openFileDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=1&_txlock=immediate")
	if err != nil {
		t.Fatalf("open file-backed database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// rebuildBaseMigration is a parent/child pair with one row each, the
// smallest schema on which a parent-table rebuild can go wrong.
const rebuildBaseMigration = `
CREATE TABLE parent (id INTEGER PRIMARY KEY, label TEXT NOT NULL);
CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent (id));
INSERT INTO parent (id, label) VALUES (1, 'kept');
INSERT INTO child (id, parent_id) VALUES (10, 1);
`

func TestLoadMigrations_DetectsRebuildDirectiveOnlyInLeadingComments(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_plain.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);"),
		},
		"migrations/0002_directive.sql": &fstest.MapFile{
			Data: []byte("-- a leading note\n" + rebuildDirective + "\n\nCREATE TABLE b (id INTEGER PRIMARY KEY);"),
		},
		"migrations/0003_buried.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE c (id INTEGER PRIMARY KEY);\n" + rebuildDirective + "\n"),
		},
	}
	migrations, err := loadMigrations(files, "migrations")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	want := map[int]bool{1: false, 2: true, 3: false}
	for _, m := range migrations {
		if m.rebuild != want[m.version] {
			t.Errorf("migration %d rebuild = %v, want %v", m.version, m.rebuild, want[m.version])
		}
	}
}

// TestApplyAll_RebuildDirectiveDropsParentTableWithoutBreakingChildren is
// the property migration 0016 depends on: with foreign keys enabled the
// DROP TABLE step of a table rebuild fails outright, so applyRebuild has
// to switch them off, and the child rows must still be intact and still
// enforced afterwards.
func TestApplyAll_RebuildDirectiveDropsParentTableWithoutBreakingChildren(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_base.sql": &fstest.MapFile{Data: []byte(rebuildBaseMigration)},
		"migrations/0002_rebuild.sql": &fstest.MapFile{Data: []byte(rebuildDirective + `
CREATE TABLE parent_new (id INTEGER PRIMARY KEY, label TEXT NOT NULL CHECK (label <> ''));
INSERT INTO parent_new (id, label) SELECT id, label FROM parent;
DROP TABLE parent;
ALTER TABLE parent_new RENAME TO parent;
`)},
	}
	db := openFileDB(t)
	ctx := t.Context()
	if err := applyAll(ctx, db, files, "migrations"); err != nil {
		t.Fatalf("applyAll: %v", err)
	}

	var label string
	if err := db.QueryRowContext(ctx, `SELECT label FROM parent WHERE id = 1`).Scan(&label); err != nil {
		t.Fatalf("rebuilt parent row: %v", err)
	}
	if label != "kept" {
		t.Errorf("label = %q, want %q", label, "kept")
	}
	var childCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child`).Scan(&childCount); err != nil {
		t.Fatal(err)
	}
	if childCount != 1 {
		t.Errorf("child rows after rebuild = %d, want 1", childCount)
	}

	// The rebuilt table's new CHECK is in force...
	if _, err := db.ExecContext(ctx, `INSERT INTO parent (id, label) VALUES (2, '')`); err == nil {
		t.Error("empty label should violate the rebuilt table's CHECK constraint")
	}
	// ...and so are foreign keys, on connections handed out after the
	// rebuild released its dedicated one.
	if _, err := db.ExecContext(ctx, `INSERT INTO child (id, parent_id) VALUES (11, 999)`); err == nil {
		t.Error("foreign keys should be enforced again after a rebuild migration")
	}
}

// TestApplyAll_RebuildLeavingDanglingChildRowsFails backs the
// foreign_key_check step: turning foreign keys off means SQLite will
// happily let a rebuild orphan a child row, so the migration must detect
// that itself and roll back rather than commit a corrupt schema.
func TestApplyAll_RebuildLeavingDanglingChildRowsFails(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_base.sql": &fstest.MapFile{Data: []byte(rebuildBaseMigration)},
		"migrations/0002_rebuild.sql": &fstest.MapFile{Data: []byte(rebuildDirective + `
CREATE TABLE parent_new (id INTEGER PRIMARY KEY, label TEXT NOT NULL);
INSERT INTO parent_new (id, label) SELECT id, label FROM parent WHERE id > 100;
DROP TABLE parent;
ALTER TABLE parent_new RENAME TO parent;
`)},
	}
	db := openFileDB(t)
	ctx := t.Context()
	err := applyAll(ctx, db, files, "migrations")
	if err == nil {
		t.Fatal("expected a rebuild that orphans a child row to fail")
	}
	if !strings.Contains(err.Error(), "foreign key violation") {
		t.Errorf("error %q does not name the foreign key violation", err.Error())
	}

	var version int
	if scanErr := db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations WHERE version = 2`).Scan(&version); scanErr == nil {
		t.Error("a rolled-back rebuild must not be recorded in schema_migrations")
	}
	// The original parent row is still there: the whole rebuild rolled
	// back, so the next startup retries it from a consistent state.
	var label string
	if scanErr := db.QueryRowContext(ctx, `SELECT label FROM parent WHERE id = 1`).Scan(&label); scanErr != nil {
		t.Errorf("original parent row should survive the rolled-back rebuild: %v", scanErr)
	}
}

// TestApplyAll_RebuildRestoresForeignKeysPragma checks the pool is left
// clean: applyRebuild turns foreign_keys off on a real connection, and
// every connection the pool hands out afterwards must report it back on.
func TestApplyAll_RebuildRestoresForeignKeysPragma(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_base.sql": &fstest.MapFile{Data: []byte(rebuildBaseMigration)},
		"migrations/0002_rebuild.sql": &fstest.MapFile{Data: []byte(rebuildDirective + `
CREATE TABLE parent_new (id INTEGER PRIMARY KEY, label TEXT NOT NULL);
INSERT INTO parent_new (id, label) SELECT id, label FROM parent;
DROP TABLE parent;
ALTER TABLE parent_new RENAME TO parent;
`)},
	}
	db := openFileDB(t)
	db.SetMaxOpenConns(1)
	ctx := t.Context()
	if err := applyAll(ctx, db, files, "migrations"); err != nil {
		t.Fatalf("applyAll: %v", err)
	}

	// MaxOpenConns(1) forces this query onto the very connection
	// applyRebuild borrowed and returned, which is the one that would
	// still have foreign keys off if the restore were skipped.
	var foreignKeys string
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys pragma: %v", err)
	}
	if foreignKeys != "1" {
		t.Errorf("foreign_keys after a rebuild migration = %q, want 1", foreignKeys)
	}
}

// TestApplyAll_RebuildDirectiveIsRequiredForTableRebuilds records why the
// directive exists at all: the identical migration on the ordinary
// in-transaction path fails at DROP TABLE, because dropping a parent with
// foreign keys enabled behaves like deleting every one of its rows.
func TestApplyAll_RebuildDirectiveIsRequiredForTableRebuilds(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_base.sql": &fstest.MapFile{Data: []byte(rebuildBaseMigration)},
		"migrations/0002_no_directive.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE parent_new (id INTEGER PRIMARY KEY, label TEXT NOT NULL);
INSERT INTO parent_new (id, label) SELECT id, label FROM parent;
DROP TABLE parent;
ALTER TABLE parent_new RENAME TO parent;
`)},
	}
	db := openFileDB(t)
	if err := applyAll(t.Context(), db, files, "migrations"); err == nil {
		t.Fatal("expected a table rebuild without the directive to fail on DROP TABLE")
	}
}
