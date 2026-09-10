package webadmin

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestRecordAction_WritesEntryWithSessionAttribution(t *testing.T) {
	ts := newTestService(t)
	credentialID := "cred-abc"
	session := domain.WebAdminSession{
		ID: domain.NewID(), OwnerActorID: ts.ownerID, CredentialID: &credentialID,
		Status: domain.WebAdminSessionActive,
	}
	before, after := "created", "authorized"

	if err := ts.RecordAction(t.Context(), session, domain.WebAdminActionApproveSession, "route-session-1", &before, &after); err != nil {
		t.Fatal(err)
	}

	// internal/webadmin.Service exposes no read path for
	// web_admin_action_audit (record-only per plan-136-phase3 §3) — a
	// second raw connection to the same test DB file, the same pattern
	// internal/miauth/service_test.go's dangling-session test already
	// uses, is how this package's own tests inspect a table its
	// repository interface doesn't read back.
	rawDB, err := sql.Open("sqlite", "file:"+ts.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()

	var (
		gotOwnerActorID, gotCredentialID, gotAction, gotTarget, gotBefore, gotAfter, gotChangedAt string
	)
	row := rawDB.QueryRowContext(t.Context(),
		`SELECT owner_actor_id, credential_id, action, target, before_value, after_value, changed_at FROM web_admin_action_audit`)
	if err := row.Scan(&gotOwnerActorID, &gotCredentialID, &gotAction, &gotTarget, &gotBefore, &gotAfter, &gotChangedAt); err != nil {
		t.Fatal(err)
	}
	if gotOwnerActorID != ts.ownerID {
		t.Errorf("OwnerActorID = %q, want %q", gotOwnerActorID, ts.ownerID)
	}
	if gotCredentialID != credentialID {
		t.Errorf("CredentialID = %q, want %q", gotCredentialID, credentialID)
	}
	if gotAction != string(domain.WebAdminActionApproveSession) {
		t.Errorf("Action = %q, want %q", gotAction, domain.WebAdminActionApproveSession)
	}
	if gotTarget != "route-session-1" {
		t.Errorf("Target = %q, want route-session-1", gotTarget)
	}
	if gotBefore != before || gotAfter != after {
		t.Errorf("BeforeValue/AfterValue = %q/%q, want %q/%q", gotBefore, gotAfter, before, after)
	}
	// Duplicates internal/storage/sqlite's unexported timeLayout constant
	// (RFC 3339, fixed-width nanosecond fraction, always UTC) — same
	// cross-package test-duplication precedent as sha256Hex in
	// internal/httpserver/webadmin_handlers_test.go.
	const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"
	wantChangedAt := ts.clock.Now().UTC().Format(timeLayout)
	if gotChangedAt != wantChangedAt {
		t.Errorf("ChangedAt = %q, want %q", gotChangedAt, wantChangedAt)
	}
}
