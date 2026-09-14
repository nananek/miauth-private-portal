package sqlite

import "testing"

// TestNormalizeStoredTimestamps_FixesLegacyRowsAcrossTables seeds
// malformed rows in two unrelated tables directly (bypassing formatTime,
// the way the historical write path behind Issue #148 must have), then
// checks NormalizeStoredTimestamps rewrites every one of them to
// timeLayout's canonical fixed-width form without changing the moment in
// time each one represents, and leaves a genuinely unparseable row
// untouched rather than failing the whole pass over it.
func TestNormalizeStoredTimestamps_FixesLegacyRowsAcrossTables(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)

	const (
		sixDigitFraction = "2026-09-08T15:13:42.619632Z" // Issue #148's exact repro value
		noFraction       = "2024-01-01T00:00:00Z"
		garbage          = "not-a-timestamp"
	)

	if _, err := db.sqlDB.ExecContext(t.Context(),
		`UPDATE actors SET created_at = ? WHERE id = ?`, sixDigitFraction, actorID); err != nil {
		t.Fatalf("seed malformed actors row: %v", err)
	}
	if _, err := db.sqlDB.ExecContext(t.Context(),
		`UPDATE schema_migrations SET applied_at = ? WHERE version = 1`, noFraction); err != nil {
		t.Fatalf("seed malformed schema_migrations row: %v", err)
	}
	if _, err := db.sqlDB.ExecContext(t.Context(),
		`UPDATE schema_migrations SET applied_at = ? WHERE version = 2`, garbage); err != nil {
		t.Fatalf("seed unparseable schema_migrations row: %v", err)
	}

	normalized, skipped, err := db.NormalizeStoredTimestamps(t.Context())
	if err != nil {
		t.Fatalf("NormalizeStoredTimestamps: %v", err)
	}
	if normalized != 2 {
		t.Errorf("normalized = %d, want 2 (the actors row and the version-1 schema_migrations row)", normalized)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the unparseable version-2 row)", skipped)
	}

	var gotActorCreatedAt string
	if err := db.sqlDB.QueryRowContext(t.Context(),
		`SELECT created_at FROM actors WHERE id = ?`, actorID).Scan(&gotActorCreatedAt); err != nil {
		t.Fatalf("read back actors.created_at: %v", err)
	}
	wantTime, err := parseTime(sixDigitFraction)
	if err != nil {
		t.Fatalf("parseTime(%q): %v", sixDigitFraction, err)
	}
	if want := formatTime(wantTime); gotActorCreatedAt != want {
		t.Errorf("actors.created_at = %q, want canonical %q", gotActorCreatedAt, want)
	}
	if got, err := parseTime(gotActorCreatedAt); err != nil || !got.Equal(wantTime) {
		t.Errorf("normalized actors.created_at parses to %v (err %v), want %v", got, err, wantTime)
	}

	var gotMigrationAppliedAt string
	if err := db.sqlDB.QueryRowContext(t.Context(),
		`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&gotMigrationAppliedAt); err != nil {
		t.Fatalf("read back schema_migrations.applied_at (version 1): %v", err)
	}
	if len(gotMigrationAppliedAt) != len(formatTime(wantTime)) {
		t.Errorf("schema_migrations.applied_at (version 1) = %q, want fixed-width canonical form", gotMigrationAppliedAt)
	}

	var gotGarbage string
	if err := db.sqlDB.QueryRowContext(t.Context(),
		`SELECT applied_at FROM schema_migrations WHERE version = 2`).Scan(&gotGarbage); err != nil {
		t.Fatalf("read back schema_migrations.applied_at (version 2): %v", err)
	}
	if gotGarbage != garbage {
		t.Errorf("schema_migrations.applied_at (version 2) = %q, want untouched %q", gotGarbage, garbage)
	}

	normalizedAgain, skippedAgain, err := db.NormalizeStoredTimestamps(t.Context())
	if err != nil {
		t.Fatalf("second NormalizeStoredTimestamps: %v", err)
	}
	if normalizedAgain != 0 {
		t.Errorf("second pass normalized = %d, want 0 (idempotent)", normalizedAgain)
	}
	if skippedAgain != 1 {
		t.Errorf("second pass skipped = %d, want 1 (the same unparseable row)", skippedAgain)
	}
}
