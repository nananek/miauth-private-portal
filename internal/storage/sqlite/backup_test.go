package sqlite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestBackup_SnapshotVerifiesAndMatchesSourceRowCounts(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	mustCreateThreadAndRoot(t, db, actorID, time.Now())
	mustEnqueueJob(t, db, nil, time.Now())
	mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")

	wantCounts, err := db.TableCounts(t.Context())
	if err != nil {
		t.Fatalf("source TableCounts: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(t.Context(), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	backup, err := OpenReadOnly(t.Context(), dest)
	if err != nil {
		t.Fatalf("OpenReadOnly(backup): %v", err)
	}
	defer backup.Close()

	if err := backup.VerifyMigrations(t.Context()); err != nil {
		t.Errorf("VerifyMigrations(backup) = %v, want nil", err)
	}
	if err := backup.IntegrityCheck(t.Context()); err != nil {
		t.Errorf("IntegrityCheck(backup) = %v, want nil", err)
	}
	gotCounts, err := backup.TableCounts(t.Context())
	if err != nil {
		t.Fatalf("backup TableCounts: %v", err)
	}
	for table, want := range wantCounts {
		if gotCounts[table] != want {
			t.Errorf("backup table %s count = %d, want %d (source)", table, gotCounts[table], want)
		}
	}
}

func TestBackup_RefusesToOverwriteExistingFile(t *testing.T) {
	db := newTestDB(t)
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(dest, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := db.Backup(t.Context(), dest); err == nil {
		t.Fatal("Backup() into an existing file = nil error, want an error")
	}
}

func TestOpenReadOnly_MissingFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.db")
	if _, err := OpenReadOnly(t.Context(), path); err == nil {
		t.Fatal("OpenReadOnly() on a missing file = nil error, want an error")
	}
}

func TestOpenReadOnly_RejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(t.Context(), Config{Path: path, BusyTimeout: 5 * time.Second, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ro, err := OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	if _, err := ro.sqlDB.ExecContext(t.Context(), `INSERT INTO actors (id, actor_type, created_at) VALUES ('x', 'system', '2024-01-01T00:00:00Z')`); err == nil {
		t.Fatal("write through OpenReadOnly connection succeeded, want an error")
	}
}

func TestVerifyMigrations_SucceedsForFullyMigratedDatabase(t *testing.T) {
	db := newTestDB(t)
	if err := db.VerifyMigrations(t.Context()); err != nil {
		t.Errorf("VerifyMigrations() = %v, want nil", err)
	}
}

func TestVerifyMigrations_DetectsChecksumMismatch(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)},
	}
	sqlDB := openMemoryDB(t)
	if _, err := sqlDB.ExecContext(t.Context(), `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (1, 'tampered', '2024-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}

	err := verifyMigrations(t.Context(), sqlDB, files, "migrations")
	if err == nil {
		t.Fatal("expected an error for a tampered checksum")
	}
	if !strings.Contains(err.Error(), "0001_a") {
		t.Errorf("error %q does not name the mismatched migration", err.Error())
	}
}

func TestVerifyMigrations_DetectsMigrationNotYetApplied(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)},
	}
	sqlDB := openMemoryDB(t)
	if _, err := sqlDB.ExecContext(t.Context(), `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}

	err := verifyMigrations(t.Context(), sqlDB, files, "migrations")
	if err == nil {
		t.Fatal("expected an error for a migration not recorded as applied")
	}
	if !strings.Contains(err.Error(), "0001_a") || !strings.Contains(err.Error(), "not recorded as applied") {
		t.Errorf("error %q does not name the unapplied migration", err.Error())
	}
}

func TestTableCounts_ReflectsSeededRows(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	mustCreateThreadAndRoot(t, db, actorID, time.Now())
	mustEnqueueJob(t, db, nil, time.Now())
	mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")

	counts, err := db.TableCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if counts["entries"] != 1 {
		t.Errorf("entries count = %d, want 1", counts["entries"])
	}
	if counts["jobs"] != 1 {
		t.Errorf("jobs count = %d, want 1", counts["jobs"])
	}
	if counts["external_sources"] != 1 {
		t.Errorf("external_sources count = %d, want 1", counts["external_sources"])
	}
	if counts["actors"] < 1 {
		t.Errorf("actors count = %d, want at least 1", counts["actors"])
	}
}

func TestIntegrityCheck_SucceedsForFreshDatabase(t *testing.T) {
	db := newTestDB(t)
	if err := db.IntegrityCheck(t.Context()); err != nil {
		t.Errorf("IntegrityCheck() = %v, want nil", err)
	}
}
