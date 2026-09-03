package sqlite

import (
	"context"
	"database/sql"
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
