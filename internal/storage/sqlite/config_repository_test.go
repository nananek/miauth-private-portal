package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// newConfigTestDB is a plain migrated database: app_config/
// app_config_audit carry no foreign key to actors (updated_by/changed_by
// are opaque owner-actor-id strings, never joined against), so these
// tests need no actor fixture at all.
func newConfigTestDB(t *testing.T) *DB {
	t.Helper()
	return newTestDB(t)
}

func TestConfigRepository_Set_FirstTimeCreatesVersion1(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()

	if err := db.Config.Set(t.Context(), "JOBS_POLL_INTERVAL", "2s", 0, "owner-1", now); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := db.Config.Get(t.Context(), "JOBS_POLL_INTERVAL")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "2s" || got.Version != 1 || got.UpdatedBy != "owner-1" {
		t.Errorf("Get = %+v, want value=2s version=1 updatedBy=owner-1", got)
	}
}

func TestConfigRepository_Set_ExpectedVersionZeroConflictsWhenRowExists(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()

	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "5", 0, "owner-1", now); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	// A second "must not already exist" Set (expectedVersion 0) is
	// exactly the concurrent-first-writer race two operators racing
	// miauthctl config set would produce.
	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "6", 0, "owner-2", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Set(expectedVersion=0) = %v, want ErrConflict", err)
	}
}

func TestConfigRepository_Set_CASUpdateSucceedsAndIncrementsVersion(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()

	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "5", 0, "owner-1", now); err != nil {
		t.Fatalf("Set(0): %v", err)
	}
	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "6", 1, "owner-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Set(1): %v", err)
	}

	got, err := db.Config.Get(t.Context(), "JOBS_MAX_ATTEMPTS")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "6" || got.Version != 2 {
		t.Errorf("Get = %+v, want value=6 version=2", got)
	}
}

// TestConfigRepository_Set_ConflictOnStaleExpectedVersion backs Issue
// #76 AC7's "同時更新" test: an operator reading version 1, then a
// second operator's write moving the row to version 2, must make the
// first operator's own version-1 write fail rather than silently
// clobbering the second write.
func TestConfigRepository_Set_ConflictOnStaleExpectedVersion(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()

	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "5", 0, "owner-1", now); err != nil {
		t.Fatalf("Set(0): %v", err)
	}
	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "6", 1, "owner-2", now); err != nil {
		t.Fatalf("Set(1) by owner-2: %v", err)
	}

	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "7", 1, "owner-1", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale Set(1) by owner-1 = %v, want ErrConflict", err)
	}

	// The conflicting write must not have applied.
	got, err := db.Config.Get(t.Context(), "JOBS_MAX_ATTEMPTS")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "6" || got.Version != 2 {
		t.Errorf("Get after conflict = %+v, want value=6 version=2 (owner-2's write untouched)", got)
	}
}

func TestConfigRepository_Set_PositiveExpectedVersionConflictsWhenRowDeleted(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()

	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "5", 0, "owner-1", now); err != nil {
		t.Fatalf("Set(0): %v", err)
	}
	if err := db.Config.Unset(t.Context(), "JOBS_MAX_ATTEMPTS"); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "6", 1, "owner-1", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Set(1) after Unset = %v, want ErrConflict", err)
	}
}

func TestConfigRepository_Get_NotFound(t *testing.T) {
	db := newConfigTestDB(t)
	if _, err := db.Config.Get(t.Context(), "JOBS_POLL_INTERVAL"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get(unset key) = %v, want ErrNotFound", err)
	}
}

func TestConfigRepository_Unset_NotFoundWhenMissing(t *testing.T) {
	db := newConfigTestDB(t)
	if err := db.Config.Unset(t.Context(), "JOBS_POLL_INTERVAL"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Unset(never-set key) = %v, want ErrNotFound", err)
	}
}

func TestConfigRepository_List_OrderedByKey(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()

	for _, key := range []string{"RSS_POLL_INTERVAL", "JOBS_POLL_INTERVAL", "LLM_MODEL"} {
		if err := db.Config.Set(t.Context(), key, "v", 0, "owner-1", now); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
	}

	got, err := db.Config.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"JOBS_POLL_INTERVAL", "LLM_MODEL", "RSS_POLL_INTERVAL"}
	if len(got) != len(want) {
		t.Fatalf("List returned %d entries, want %d", len(got), len(want))
	}
	for i, k := range want {
		if got[i].Key != k {
			t.Errorf("List()[%d].Key = %q, want %q (want alphabetical order)", i, got[i].Key, k)
		}
	}
}

func TestConfigAuditRepository_Record_ListByKeyOrderedByVersion(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()
	old1 := "5"
	new1 := "6"
	new2 := "7"

	if err := db.ConfigAudit.Record(t.Context(), domain.AppConfigAuditEntry{
		ID: domain.NewID(), Key: "JOBS_MAX_ATTEMPTS", OldValue: nil, NewValue: &old1,
		Version: 1, ChangedAt: now, ChangedBy: "owner-1",
	}); err != nil {
		t.Fatalf("Record v1: %v", err)
	}
	if err := db.ConfigAudit.Record(t.Context(), domain.AppConfigAuditEntry{
		ID: domain.NewID(), Key: "JOBS_MAX_ATTEMPTS", OldValue: &old1, NewValue: &new1,
		Version: 2, ChangedAt: now.Add(time.Minute), ChangedBy: "owner-1",
	}); err != nil {
		t.Fatalf("Record v2: %v", err)
	}
	if err := db.ConfigAudit.Record(t.Context(), domain.AppConfigAuditEntry{
		ID: domain.NewID(), Key: "JOBS_MAX_ATTEMPTS", OldValue: &new1, NewValue: &new2,
		Version: 3, ChangedAt: now.Add(2 * time.Minute), ChangedBy: "owner-2",
	}); err != nil {
		t.Fatalf("Record v3: %v", err)
	}
	// A different key's audit trail must not leak into ListByKey.
	if err := db.ConfigAudit.Record(t.Context(), domain.AppConfigAuditEntry{
		ID: domain.NewID(), Key: "LLM_MODEL", OldValue: nil, NewValue: &new1,
		Version: 1, ChangedAt: now, ChangedBy: "owner-1",
	}); err != nil {
		t.Fatalf("Record other key: %v", err)
	}

	history, err := db.ConfigAudit.ListByKey(t.Context(), "JOBS_MAX_ATTEMPTS")
	if err != nil {
		t.Fatalf("ListByKey: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("ListByKey returned %d entries, want 3", len(history))
	}
	for i, wantVersion := range []int{1, 2, 3} {
		if history[i].Version != wantVersion {
			t.Errorf("history[%d].Version = %d, want %d", i, history[i].Version, wantVersion)
		}
	}
	if history[0].OldValue != nil {
		t.Errorf("history[0].OldValue = %v, want nil (first-ever row)", *history[0].OldValue)
	}
	if history[0].NewValue == nil || *history[0].NewValue != "5" {
		t.Errorf("history[0].NewValue = %v, want \"5\"", history[0].NewValue)
	}
}

// TestConfigRepository_Set_AndAudit_WithinTxRollsBackTogether backs Issue
// #76 AC4: a Set and its Record call are meant to be composed inside one
// domain.UnitOfWork.WithinTx transaction, so a failure partway through
// (here, a duplicate audit id violating app_config_audit's primary key)
// must roll back the app_config write too, not leave it applied with no
// audit trail.
func TestConfigRepository_Set_AndAudit_WithinTxRollsBackTogether(t *testing.T) {
	db := newConfigTestDB(t)
	now := time.Now()
	dupID := domain.NewID()
	if err := db.ConfigAudit.Record(t.Context(), domain.AppConfigAuditEntry{
		ID: dupID, Key: "LLM_MODEL", OldValue: nil, NewValue: strPtr("seed"),
		Version: 1, ChangedAt: now, ChangedBy: "owner-1",
	}); err != nil {
		t.Fatalf("seed Record: %v", err)
	}

	err := db.WithinTx(t.Context(), func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Config.Set(ctx, "JOBS_POLL_INTERVAL", "2s", 0, "owner-1", now); err != nil {
			return err
		}
		// Reusing dupID collides with the primary key inserted above,
		// forcing the transaction to fail after the Set above already
		// ran against the same *sql.Tx.
		return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
			ID: dupID, Key: "JOBS_POLL_INTERVAL", OldValue: nil, NewValue: strPtr("2s"),
			Version: 1, ChangedAt: now, ChangedBy: "owner-1",
		})
	})
	if err == nil {
		t.Fatal("WithinTx succeeded, want the duplicate audit id to fail it")
	}

	if _, err := db.Config.Get(t.Context(), "JOBS_POLL_INTERVAL"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after rolled-back transaction = %v, want ErrNotFound (Set must not have persisted)", err)
	}
}

func strPtr(s string) *string { return &s }
