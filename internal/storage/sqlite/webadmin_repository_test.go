package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func createBootstrapToken(t *testing.T, db *DB, ownerActorID string, createdAt time.Time) domain.WebAdminBootstrapToken {
	t.Helper()
	tok := domain.WebAdminBootstrapToken{
		ID: domain.NewID(), TokenHash: domain.NewID(), OwnerActorID: ownerActorID,
		Status: domain.WebAdminBootstrapIssued, CreatedAt: createdAt, ExpiresAt: createdAt.Add(10 * time.Minute),
	}
	if err := db.WebAdminBootstrapTokens.Create(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestWebAdminBootstrapTokenRepository_CreateAndGetByTokenHash(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)
	tok := createBootstrapToken(t, db, owner.ID, now)

	got, err := db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), tok.TokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != tok.ID || got.Status != domain.WebAdminBootstrapIssued || got.WebAuthnSessionData != nil {
		t.Fatalf("GetByTokenHash = %+v", got)
	}
	if _, err := db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), "unknown-hash"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown hash error = %v, want ErrNotFound", err)
	}
}

func TestWebAdminBootstrapTokenRepository_SetSessionData_ConflictsIfExpiredOrConsumed(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)

	expired := createBootstrapToken(t, db, owner.ID, now)
	if err := db.WebAdminBootstrapTokens.SetSessionData(t.Context(), expired.ID, "{}", now.Add(11*time.Minute)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired SetSessionData error = %v, want ErrConflict", err)
	}

	consumed := createBootstrapToken(t, db, owner.ID, now)
	if _, err := db.WebAdminBootstrapTokens.Consume(t.Context(), consumed.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := db.WebAdminBootstrapTokens.SetSessionData(t.Context(), consumed.ID, "{}", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("consumed SetSessionData error = %v, want ErrConflict", err)
	}

	live := createBootstrapToken(t, db, owner.ID, now)
	if err := db.WebAdminBootstrapTokens.SetSessionData(t.Context(), live.ID, `{"challenge":"abc"}`, now); err != nil {
		t.Fatal(err)
	}
	got, err := db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), live.TokenHash)
	if err != nil || got.WebAuthnSessionData == nil || *got.WebAuthnSessionData != `{"challenge":"abc"}` {
		t.Fatalf("GetByTokenHash after SetSessionData = %+v, err = %v", got, err)
	}
}

func TestWebAdminBootstrapTokenRepository_Consume_ConflictsIfAlreadyConsumedOrExpired(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)

	tok := createBootstrapToken(t, db, owner.ID, now)
	consumed, err := db.WebAdminBootstrapTokens.Consume(t.Context(), tok.ID, now)
	if err != nil || consumed.Status != domain.WebAdminBootstrapConsumed || consumed.ConsumedAt == nil {
		t.Fatalf("Consume = %+v, err = %v", consumed, err)
	}
	if _, err := db.WebAdminBootstrapTokens.Consume(t.Context(), tok.ID, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Consume error = %v, want ErrConflict", err)
	}

	expired := createBootstrapToken(t, db, owner.ID, now)
	if _, err := db.WebAdminBootstrapTokens.Consume(t.Context(), expired.ID, now.Add(11*time.Minute)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired Consume error = %v, want ErrConflict", err)
	}
}

func TestWebAdminCredentialRepository_CreateAndListByOwner(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)
	// other is Assistant, not a second Owner: actors has a partial unique
	// index over the singleton types (see domain.Actor's own doc
	// comment), so a second Owner row would conflict. ListByOwner only
	// needs a distinct actor id to isolate against, not a real Owner row.
	other := domain.Actor{ID: domain.NewID(), Type: domain.ActorAssistant, CreatedAt: now}
	if err := db.Actors.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}

	first := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: owner.ID, CredentialID: "cred-1",
		CredentialJSON: `{"id":"cred-1"}`, CreatedAt: now,
	}
	second := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: owner.ID, CredentialID: "cred-2",
		CredentialJSON: `{"id":"cred-2"}`, CreatedAt: now.Add(time.Minute),
	}
	unrelated := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: other.ID, CredentialID: "cred-3",
		CredentialJSON: `{"id":"cred-3"}`, CreatedAt: now,
	}
	for _, c := range []domain.WebAdminCredential{first, second, unrelated} {
		if err := db.WebAdminCredentials.Create(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.WebAdminCredentials.ListByOwner(t.Context(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].CredentialID != "cred-1" || got[1].CredentialID != "cred-2" {
		t.Fatalf("ListByOwner = %+v", got)
	}

	if err := db.WebAdminCredentials.Create(t.Context(), first); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate credential_id error = %v, want ErrConflict", err)
	}
}

func createWebAdminCredential(t *testing.T, db *DB, ownerActorID, credentialID string, createdAt time.Time) domain.WebAdminCredential {
	t.Helper()
	c := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: ownerActorID, CredentialID: credentialID,
		CredentialJSON: `{"id":"` + credentialID + `"}`, CreatedAt: createdAt,
	}
	if err := db.WebAdminCredentials.Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWebAdminCredentialRepository_UpdateAfterLogin_UpdatesJSONAndLastUsedAt(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)
	cred := createWebAdminCredential(t, db, owner.ID, "cred-update", now)

	updatedJSON := `{"id":"cred-update","signCount":7}`
	loginAt := now.Add(time.Hour)
	if err := db.WebAdminCredentials.UpdateAfterLogin(t.Context(), cred.CredentialID, updatedJSON, loginAt); err != nil {
		t.Fatal(err)
	}

	got, err := db.WebAdminCredentials.Get(t.Context(), cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CredentialJSON != updatedJSON {
		t.Fatalf("CredentialJSON = %q, want %q", got.CredentialJSON, updatedJSON)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(loginAt) {
		t.Fatalf("LastUsedAt = %v, want %v", got.LastUsedAt, loginAt)
	}

	if err := db.WebAdminCredentials.UpdateAfterLogin(t.Context(), "no-such-credential-id", "{}", loginAt); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown credential_id error = %v, want ErrNotFound", err)
	}
}

func TestWebAdminCredentialRepository_Delete_RemovesRow_NotFoundOnUnknownID(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)
	cred := createWebAdminCredential(t, db, owner.ID, "cred-delete", now)

	if _, err := db.WebAdminCredentials.Get(t.Context(), cred.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.WebAdminCredentials.Delete(t.Context(), cred.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WebAdminCredentials.Get(t.Context(), cred.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := db.WebAdminCredentials.Delete(t.Context(), "no-such-id"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Delete unknown id error = %v, want ErrNotFound", err)
	}
}

func createPendingSession(t *testing.T, db *DB, ownerActorID string, createdAt, expiresAt time.Time) domain.WebAdminSession {
	t.Helper()
	sessionData := `{"challenge":"abc"}`
	s := domain.WebAdminSession{
		ID: domain.NewID(), OwnerActorID: ownerActorID, Status: domain.WebAdminSessionPending,
		WebAuthnSessionData: &sessionData, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
	if err := db.WebAdminSessions.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebAdminSessionRepository_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)
	s := createPendingSession(t, db, owner.ID, now, now.Add(5*time.Minute))

	got, err := db.WebAdminSessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.WebAdminSessionPending || got.CredentialID != nil || got.SessionTokenHash != nil ||
		got.CSRFToken != nil || got.WebAuthnSessionData == nil || got.RevokedAt != nil {
		t.Fatalf("Get = %+v", got)
	}
	if _, err := db.WebAdminSessions.Get(t.Context(), "no-such-id"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown id error = %v, want ErrNotFound", err)
	}
}

func TestWebAdminSessionRepository_Activate_ConflictsIfNotPendingOrExpired(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)

	expired := createPendingSession(t, db, owner.ID, now, now)
	if _, err := db.WebAdminSessions.Activate(t.Context(), expired.ID, "cred-1", "hash-1", "csrf-1", now.Add(time.Hour), now.Add(time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired Activate error = %v, want ErrConflict", err)
	}

	live := createPendingSession(t, db, owner.ID, now, now.Add(5*time.Minute))
	activated, err := db.WebAdminSessions.Activate(t.Context(), live.ID, "cred-1", "hash-1", "csrf-1", now.Add(12*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if activated.Status != domain.WebAdminSessionActive || activated.CredentialID == nil || *activated.CredentialID != "cred-1" ||
		activated.SessionTokenHash == nil || *activated.SessionTokenHash != "hash-1" ||
		activated.CSRFToken == nil || *activated.CSRFToken != "csrf-1" ||
		activated.WebAuthnSessionData != nil || !activated.ExpiresAt.Equal(now.Add(12*time.Hour)) {
		t.Fatalf("Activate = %+v", activated)
	}

	// A second Activate on the now-active row is a conflict (status is
	// no longer 'pending').
	if _, err := db.WebAdminSessions.Activate(t.Context(), live.ID, "cred-2", "hash-2", "csrf-2", now.Add(24*time.Hour), now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("re-Activate error = %v, want ErrConflict", err)
	}
}

func TestWebAdminSessionRepository_GetActiveBySessionTokenHash_ExcludesExpiredRevokedPending(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)

	activate := func(hash string, expiresAt time.Time) domain.WebAdminSession {
		pending := createPendingSession(t, db, owner.ID, now, now.Add(5*time.Minute))
		activated, err := db.WebAdminSessions.Activate(t.Context(), pending.ID, "cred-1", hash, "csrf", expiresAt, now)
		if err != nil {
			t.Fatal(err)
		}
		return activated
	}

	active := activate("hash-active", now.Add(time.Hour))
	if got, err := db.WebAdminSessions.GetActiveBySessionTokenHash(t.Context(), "hash-active", now); err != nil || got.ID != active.ID {
		t.Fatalf("GetActiveBySessionTokenHash(active) = %+v, err = %v", got, err)
	}

	// A never-activated pending session has no session_token_hash to
	// look up at all — its exclusion is structural (the query's own
	// status='active' filter), unlike the expired/revoked cases below,
	// which do have a real hash that must still be excluded.
	createPendingSession(t, db, owner.ID, now, now.Add(5*time.Minute))
	activate("hash-expired", now.Add(-time.Second))

	revokedSession := activate("hash-revoked", now.Add(time.Hour))
	if err := db.WebAdminSessions.Revoke(t.Context(), revokedSession.ID, now); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		hash string
		at   time.Time
		want error
	}{
		{"expired", "hash-expired", now, domain.ErrNotFound},
		{"revoked", "hash-revoked", now, domain.ErrNotFound},
		{"unknown hash", "hash-does-not-exist", now, domain.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.WebAdminSessions.GetActiveBySessionTokenHash(t.Context(), tc.hash, tc.at); !errors.Is(err, tc.want) {
				t.Fatalf("GetActiveBySessionTokenHash(%q) error = %v, want %v", tc.hash, err, tc.want)
			}
		})
	}
}

func TestWebAdminSessionRepository_Revoke_IdempotentOnAlreadyRevoked(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)
	pending := createPendingSession(t, db, owner.ID, now, now.Add(5*time.Minute))
	active, err := db.WebAdminSessions.Activate(t.Context(), pending.ID, "cred-1", "hash-revoke-idem", "csrf", now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.WebAdminSessions.Revoke(t.Context(), active.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := db.WebAdminSessions.Get(t.Context(), active.ID)
	if err != nil || got.RevokedAt == nil {
		t.Fatalf("Get after Revoke = %+v, err = %v, want RevokedAt set", got, err)
	}
	firstRevokedAt := *got.RevokedAt

	// Revoking again (already revoked) must succeed and must not move
	// RevokedAt.
	if err := db.WebAdminSessions.Revoke(t.Context(), active.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("second Revoke = %v, want nil (idempotent)", err)
	}
	got2, err := db.WebAdminSessions.Get(t.Context(), active.ID)
	if err != nil || got2.RevokedAt == nil || !got2.RevokedAt.Equal(firstRevokedAt) {
		t.Fatalf("Get after second Revoke = %+v, err = %v, want RevokedAt unchanged at %v", got2, err, firstRevokedAt)
	}

	// Revoking a nonexistent session id is also a no-op success.
	if err := db.WebAdminSessions.Revoke(t.Context(), "no-such-id", now); err != nil {
		t.Fatalf("Revoke unknown id = %v, want nil (no-op)", err)
	}
}

func TestWebAdminSessionRepository_RevokeAllByCredential_OnlyAffectsMatchingActiveRows(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := createOwnerActor(t, db, now)

	activate := func(credentialID, hash string) domain.WebAdminSession {
		pending := createPendingSession(t, db, owner.ID, now, now.Add(5*time.Minute))
		activated, err := db.WebAdminSessions.Activate(t.Context(), pending.ID, credentialID, hash, "csrf", now.Add(time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		return activated
	}

	matchA := activate("cred-target", "hash-a")
	matchB := activate("cred-target", "hash-b")
	other := activate("cred-other", "hash-c")
	// Already-revoked rows for the target credential must not be
	// double-counted in RowsAffected.
	alreadyRevoked := activate("cred-target", "hash-d")
	if err := db.WebAdminSessions.Revoke(t.Context(), alreadyRevoked.ID, now); err != nil {
		t.Fatal(err)
	}

	n, err := db.WebAdminSessions.RevokeAllByCredential(t.Context(), "cred-target", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("RevokeAllByCredential returned %d, want 2", n)
	}

	for _, id := range []string{matchA.ID, matchB.ID} {
		got, err := db.WebAdminSessions.Get(t.Context(), id)
		if err != nil || got.RevokedAt == nil {
			t.Fatalf("session %s = %+v, err = %v, want revoked", id, got, err)
		}
	}
	gotOther, err := db.WebAdminSessions.Get(t.Context(), other.ID)
	if err != nil || gotOther.RevokedAt != nil {
		t.Fatalf("other credential's session = %+v, err = %v, want unaffected", gotOther, err)
	}
}
