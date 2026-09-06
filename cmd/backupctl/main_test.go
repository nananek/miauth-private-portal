package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// wantTableRow reports whether tabwriter-formatted output has a row for
// table with exactly count, tolerating the column padding tabwriter
// inserts (so it can't be matched with a literal "table\tcount"
// substring check).
func wantTableRow(output, table string, count int) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(table) + `\s+` + regexp.QuoteMeta(strconv.Itoa(count)) + `$`)
	return re.MatchString(output)
}

func setBackupctlTestEnv(t *testing.T, dbPath string) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("LOCAL_ORIGIN", "https://portal.example")
	t.Setenv("DB_PATH", dbPath)
}

func TestRunValidatesArguments(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(nil) error = %v, want usage", err)
	}
	if err := run([]string{"unknown"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(unknown) error = %v", err)
	}
	if err := run([]string{"backup"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(backup, no args) error = %v, want usage", err)
	}
	if err := run([]string{"verify"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(verify, no args) error = %v, want usage", err)
	}
}

func TestRunBackupAndVerify_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "portal.db")
	setBackupctlTestEnv(t, dbPath)

	seedDB(t, dbPath)

	dest := filepath.Join(dir, "backup.db")
	var backupOut bytes.Buffer
	if err := run([]string{"backup", dest}, &backupOut); err != nil {
		t.Fatalf("run(backup): %v", err)
	}
	if !strings.Contains(backupOut.String(), dest) {
		t.Errorf("backup output = %q, want it to name the destination path", backupOut.String())
	}

	var verifyOut bytes.Buffer
	if err := run([]string{"verify", dest}, &verifyOut); err != nil {
		t.Fatalf("run(verify): %v", err)
	}
	got := verifyOut.String()
	if !strings.Contains(got, "Schema migrations: OK") {
		t.Errorf("verify output = %q, want schema migrations OK", got)
	}
	if strings.Contains(got, "Integrity check") {
		t.Errorf("verify output = %q, want no integrity check line without --deep", got)
	}
	if !wantTableRow(got, "entries", 1) {
		t.Errorf("verify output = %q, want entries row count of 1", got)
	}
	if !wantTableRow(got, "jobs", 1) {
		t.Errorf("verify output = %q, want jobs row count of 1", got)
	}
	if !wantTableRow(got, "external_sources", 1) {
		t.Errorf("verify output = %q, want external_sources row count of 1", got)
	}
}

func TestRunVerify_DeepFlagRunsIntegrityCheck(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "portal.db")
	setBackupctlTestEnv(t, dbPath)
	seedDB(t, dbPath)

	dest := filepath.Join(dir, "backup.db")
	if err := run([]string{"backup", dest}, &bytes.Buffer{}); err != nil {
		t.Fatalf("run(backup): %v", err)
	}

	var out bytes.Buffer
	if err := run([]string{"verify", "--deep", dest}, &out); err != nil {
		t.Fatalf("run(verify --deep): %v", err)
	}
	if !strings.Contains(out.String(), "Integrity check: OK") {
		t.Errorf("verify --deep output = %q, want integrity check OK", out.String())
	}
}

func TestRunVerify_RejectsMissingFile(t *testing.T) {
	err := run([]string{"verify", filepath.Join(t.TempDir(), "missing.db")}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run(verify) on a missing file = nil error, want an error")
	}
}

// TestRestoreDrill_BackupSurvivesSourceDestructionAndRestoresRelationships
// is Issue #13 AC6's restore drill: seed a thread with a parent/child
// reply, a completed job, and an external source with an advanced fetch
// cursor into a live database (kept open throughout the backup, since
// VACUUM INTO's whole point is not requiring the server to stop);
// back it up; destroy the original database entirely (including any WAL
// side files); place the backup at DB_PATH as a restore would; and
// confirm every relationship and the row counts survive intact.
func TestRestoreDrill_BackupSurvivesSourceDestructionAndRestoresRelationships(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "portal.db")
	setBackupctlTestEnv(t, dbPath)

	source, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := source.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatal(err)
	}
	owner, err := source.Actors.GetByType(t.Context(), "system")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	rootID := domain.NewID()
	replyID := domain.NewID()
	if err := source.Threads.Create(t.Context(), domain.Thread{ID: rootID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := source.Entries.Create(t.Context(), domain.Entry{
		ID: rootID, ThreadID: rootID, Kind: domain.EntryUserPost, AuthorActorID: owner.ID,
		Body: "root post", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := source.Entries.Create(t.Context(), domain.Entry{
		ID: replyID, ThreadID: rootID, ParentEntryID: &rootID, Kind: domain.EntryUserPost, AuthorActorID: owner.ID,
		Body: "reply post", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	jobID := domain.NewID()
	if err := source.Jobs.Enqueue(t.Context(), domain.Job{
		ID: jobID, JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
		NextRunAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := source.Jobs.Claim(t.Context(), "drill-test", 1, now, now.Add(time.Minute))
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim job: %v %+v", err, claimed)
	}
	if err := source.Jobs.Succeed(t.Context(), jobID, "drill-test", now); err != nil {
		t.Fatal(err)
	}

	sourceID := domain.NewID()
	if err := source.ExternalSources.Create(t.Context(), domain.ExternalSource{
		ID: sourceID, Kind: "rss", URI: "https://example.com/feed.xml", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	cursor := "cursor-at-backup-time"
	if err := source.ExternalSources.RecordFetchSuccess(t.Context(), sourceID, &cursor, now); err != nil {
		t.Fatal(err)
	}

	wantCounts, err := source.TableCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Back up while source is still open, proving the live connection
	// does not need to be closed first.
	dest := filepath.Join(dir, "backup.db")
	if err := run([]string{"backup", dest}, &bytes.Buffer{}); err != nil {
		t.Fatalf("run(backup): %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	// Destroy the original database entirely, including WAL side files,
	// to simulate data loss the restore must recover from.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("dbPath still exists after destruction: %v", err)
	}

	// Restore: place the backup at DB_PATH, as documented.
	if err := os.Rename(dest, dbPath); err != nil {
		t.Fatal(err)
	}

	restored, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	defer restored.Close()
	if err := restored.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate restored database: %v", err)
	}

	gotCounts, err := restored.TableCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range wantCounts {
		if gotCounts[table] != want {
			t.Errorf("restored table %s count = %d, want %d", table, gotCounts[table], want)
		}
	}

	reply, err := restored.Entries.Get(t.Context(), replyID)
	if err != nil {
		t.Fatalf("get restored reply entry: %v", err)
	}
	if reply.ParentEntryID == nil || *reply.ParentEntryID != rootID || reply.ThreadID != rootID {
		t.Errorf("restored reply entry = %+v, want parent/thread = %s", reply, rootID)
	}

	job, err := restored.Jobs.Get(t.Context(), jobID)
	if err != nil {
		t.Fatalf("get restored job: %v", err)
	}
	if job.State != domain.JobSucceeded {
		t.Errorf("restored job state = %s, want %s", job.State, domain.JobSucceeded)
	}

	restoredSource, err := restored.ExternalSources.Get(t.Context(), sourceID)
	if err != nil {
		t.Fatalf("get restored external source: %v", err)
	}
	if restoredSource.Cursor == nil || *restoredSource.Cursor != cursor {
		t.Errorf("restored external source cursor = %v, want %q", restoredSource.Cursor, cursor)
	}
}

// seedDB opens, migrates, and seeds a minimal database at dbPath (one
// entry, one succeeded job, one external source) for tests that only
// need backup/verify's row-count output, not the fuller restore drill's
// relationship assertions.
func seedDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatal(err)
	}
	owner, err := db.Actors.GetByType(t.Context(), "system")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	rootID := domain.NewID()
	if err := db.Threads.Create(t.Context(), domain.Thread{ID: rootID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Entries.Create(t.Context(), domain.Entry{
		ID: rootID, ThreadID: rootID, Kind: domain.EntryUserPost, AuthorActorID: owner.ID,
		Body: "root post", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	jobID := domain.NewID()
	if err := db.Jobs.Enqueue(t.Context(), domain.Job{
		ID: jobID, JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
		NextRunAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.Jobs.Claim(t.Context(), "seed-test", 1, now, now.Add(time.Minute))
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim job: %v %+v", err, claimed)
	}
	if err := db.Jobs.Succeed(t.Context(), jobID, "seed-test", now); err != nil {
		t.Fatal(err)
	}

	if err := db.ExternalSources.Create(t.Context(), domain.ExternalSource{
		ID: domain.NewID(), Kind: "rss", URI: "https://example.com/feed.xml", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}
