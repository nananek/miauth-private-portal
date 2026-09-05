package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// TestBackup_DestPathWithSingleQuoteIsBoundSafely pins the claim in this
// package's commit history that VACUUM INTO's destination is passed as a
// bound parameter (not concatenated into SQL text), so a path containing
// a single quote — which would break naive string concatenation into
// `VACUUM INTO '<path>'` — backs up and reads back cleanly.
func TestBackup_DestPathWithSingleQuoteIsBoundSafely(t *testing.T) {
	db := newTestDB(t)
	mustCreateActor(t, db)

	dest := filepath.Join(t.TempDir(), "it's a backup.db")
	if err := db.Backup(t.Context(), dest); err != nil {
		t.Fatalf("Backup() to a single-quote path: %v", err)
	}

	backup, err := OpenReadOnly(t.Context(), dest)
	if err != nil {
		t.Fatalf("OpenReadOnly() on a single-quote path: %v", err)
	}
	defer backup.Close()
	if err := backup.VerifyMigrations(t.Context()); err != nil {
		t.Errorf("VerifyMigrations() on single-quote-path backup = %v, want nil", err)
	}
}

// TestBackup_SucceedsAlongsideConcurrentWrites exercises the doc
// comment's claim that VACUUM INTO is safe to run against a live
// database without stopping the server: it keeps a writer busy
// inserting rows on the same *DB for the whole duration of Backup and
// requires both the backup and every write to succeed.
//
// Starting the goroutine with "go" does not guarantee it gets scheduled
// before Backup returns, so without more this could pass having raced
// nothing at all (Backup finishing before the writer ever ran). started
// blocks Backup from beginning until the writer has already completed at
// least one write, and the completed-count check after Backup returns
// fails loudly if the writer made no further progress while Backup was
// running, rather than silently accepting a non-concurrent run as a pass.
func TestBackup_SucceedsAlongsideConcurrentWrites(t *testing.T) {
	db := newTestDB(t)
	mustCreateActor(t, db)

	started := make(chan struct{})
	stop := make(chan struct{})
	writeErrs := make(chan error, 1)
	var completed int64
	go func() {
		defer close(writeErrs)
		for {
			select {
			case <-stop:
				return
			default:
			}
			now := time.Now()
			j := domain.Job{
				ID: domain.NewID(), JobType: "test", Payload: "{}", PayloadVersion: 1,
				State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now,
			}
			if err := db.Jobs.Enqueue(context.Background(), j); err != nil {
				writeErrs <- err
				return
			}
			if n := atomic.AddInt64(&completed, 1); n == 1 {
				close(started)
			}
		}
	}()
	<-started

	dest := filepath.Join(t.TempDir(), "backup-concurrent.db")
	beforeBackup := atomic.LoadInt64(&completed)
	backupErr := db.Backup(t.Context(), dest)
	duringBackup := atomic.LoadInt64(&completed) - beforeBackup
	close(stop)
	writeErr := <-writeErrs

	if backupErr != nil {
		t.Errorf("Backup() during concurrent writes = %v, want nil", backupErr)
	}
	if writeErr != nil {
		t.Errorf("concurrent write during Backup() = %v, want nil", writeErr)
	}
	if duringBackup == 0 {
		t.Fatal("no writes completed while Backup() was running; this run raced nothing, so it is not evidence that Backup is safe alongside concurrent writes")
	}
}

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
