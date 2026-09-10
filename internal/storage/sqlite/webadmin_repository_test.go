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
