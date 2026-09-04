package sqlite

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestBootstrapGateRepository_CreateAndConsume(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	g := domain.BootstrapGate{
		ID: domain.NewID(), Status: domain.BootstrapGateIssued, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := db.BootstrapGates.Create(t.Context(), g); err != nil {
		t.Fatal(err)
	}

	if err := db.BootstrapGates.Consume(t.Context(), g.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("consume: %v", err)
	}

	got, err := db.BootstrapGates.Get(t.Context(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.BootstrapGateConsumed {
		t.Errorf("Status = %q, want consumed", got.Status)
	}
}

func TestBootstrapGateRepository_Consume_RejectsSecondAttempt(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	g := domain.BootstrapGate{
		ID: domain.NewID(), Status: domain.BootstrapGateIssued, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := db.BootstrapGates.Create(t.Context(), g); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapGates.Consume(t.Context(), g.ID, now); err != nil {
		t.Fatal(err)
	}

	err := db.BootstrapGates.Consume(t.Context(), g.ID, now)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second Consume() error = %v, want ErrConflict", err)
	}
}

func TestBootstrapGateRepository_Consume_RejectsExpired(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	g := domain.BootstrapGate{
		ID: domain.NewID(), Status: domain.BootstrapGateIssued, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := db.BootstrapGates.Create(t.Context(), g); err != nil {
		t.Fatal(err)
	}

	err := db.BootstrapGates.Consume(t.Context(), g.ID, now.Add(time.Hour))
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Consume() on expired gate error = %v, want ErrConflict", err)
	}
}

func TestBootstrapGateRepository_Fail(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	g := domain.BootstrapGate{
		ID: domain.NewID(), Status: domain.BootstrapGateIssued, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := db.BootstrapGates.Create(t.Context(), g); err != nil {
		t.Fatal(err)
	}

	if err := db.BootstrapGates.Fail(t.Context(), g.ID, now); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	got, err := db.BootstrapGates.Get(t.Context(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.BootstrapGateFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}

	// A gate already failed must not be consumable afterward.
	if err := db.BootstrapGates.Consume(t.Context(), g.ID, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Consume() after Fail error = %v, want ErrConflict", err)
	}
}

func TestBootstrapGateRepository_Fail_RejectsAlreadyTerminal(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	g := domain.BootstrapGate{
		ID: domain.NewID(), Status: domain.BootstrapGateIssued, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := db.BootstrapGates.Create(t.Context(), g); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapGates.Consume(t.Context(), g.ID, now); err != nil {
		t.Fatal(err)
	}

	if err := db.BootstrapGates.Fail(t.Context(), g.ID, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Fail() after Consume error = %v, want ErrConflict", err)
	}
}

func TestLocalMiAuthSessionRepository_Deny(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	s := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}

	if err := db.LocalMiAuth.Deny(t.Context(), s.RouteSessionID, now); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	got, err := db.LocalMiAuth.Get(t.Context(), s.RouteSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.MiAuthDenied {
		t.Errorf("Status = %q, want denied", got.Status)
	}

	// A denied session is terminal: it must not become authorizable
	// afterward (for example on a late duplicate callback).
	actorID := mustCreateActor(t, db)
	if err := db.LocalMiAuth.Authorize(t.Context(), s.RouteSessionID, actorID, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Authorize() after Deny error = %v, want ErrConflict", err)
	}
}

func TestLocalMiAuthSessionRepository_Deny_RejectsAlreadyAuthorized(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	s := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if err := db.LocalMiAuth.Authorize(t.Context(), s.RouteSessionID, actorID, now); err != nil {
		t.Fatal(err)
	}

	if err := db.LocalMiAuth.Deny(t.Context(), s.RouteSessionID, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Deny() after Authorize error = %v, want ErrConflict", err)
	}
}

// TestLocalMiAuthSessionRepository_Deny_RejectsExpired backs the same
// CAS-guard convention every sibling method (Authorize, Consume,
// BootstrapGates.Fail) already has: Deny must not affect a row whose TTL
// has already passed, even though it was still in the created state.
func TestLocalMiAuthSessionRepository_Deny_RejectsExpired(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	s := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}

	err := db.LocalMiAuth.Deny(t.Context(), s.RouteSessionID, now.Add(time.Hour))
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Deny() on expired session error = %v, want ErrConflict", err)
	}
}

func TestUpstreamMiAuthSessionRepository_GetByLocalSessionID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	local := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	upstream := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), LocalSessionID: &local.RouteSessionID, IdentityOrigin: "https://misskey.example",
		State: domain.NewID(), Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.UpstreamMiAuth.Create(t.Context(), upstream); err != nil {
		t.Fatal(err)
	}

	got, err := db.UpstreamMiAuth.GetByLocalSessionID(t.Context(), local.RouteSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != upstream.ID {
		t.Errorf("ID = %q, want %q", got.ID, upstream.ID)
	}

	if _, err := db.UpstreamMiAuth.GetByLocalSessionID(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByLocalSessionID() for unknown local session error = %v, want ErrNotFound", err)
	}
}

func TestUpstreamMiAuthSessionRepository_GetByBootstrapGateID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	gate := domain.BootstrapGate{
		ID: domain.NewID(), Status: domain.BootstrapGateIssued, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := db.BootstrapGates.Create(t.Context(), gate); err != nil {
		t.Fatal(err)
	}
	upstream := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), BootstrapGateID: &gate.ID, IdentityOrigin: "https://misskey.example",
		State: domain.NewID(), Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.UpstreamMiAuth.Create(t.Context(), upstream); err != nil {
		t.Fatal(err)
	}

	got, err := db.UpstreamMiAuth.GetByBootstrapGateID(t.Context(), gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != upstream.ID {
		t.Errorf("ID = %q, want %q", got.ID, upstream.ID)
	}

	if _, err := db.UpstreamMiAuth.GetByBootstrapGateID(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByBootstrapGateID() for unknown gate error = %v, want ErrNotFound", err)
	}
}

func TestUpstreamMiAuthSessionRepository_Deny(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	local := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	upstream := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), LocalSessionID: &local.RouteSessionID, IdentityOrigin: "https://misskey.example",
		State: domain.NewID(), Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.UpstreamMiAuth.Create(t.Context(), upstream); err != nil {
		t.Fatal(err)
	}

	if err := db.UpstreamMiAuth.Deny(t.Context(), upstream.ID, now); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	got, err := db.UpstreamMiAuth.Get(t.Context(), upstream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.MiAuthDenied {
		t.Errorf("Status = %q, want denied", got.Status)
	}
	if err := db.UpstreamMiAuth.Authorize(t.Context(), upstream.ID, "wrong-user", now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Authorize() after Deny error = %v, want ErrConflict", err)
	}
}

// TestUpstreamMiAuthSessionRepository_Deny_RejectsExpired mirrors
// TestLocalMiAuthSessionRepository_Deny_RejectsExpired: Deny must not
// affect a row whose TTL has already passed.
func TestUpstreamMiAuthSessionRepository_Deny_RejectsExpired(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	local := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	upstream := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), LocalSessionID: &local.RouteSessionID, IdentityOrigin: "https://misskey.example",
		State: domain.NewID(), Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := db.UpstreamMiAuth.Create(t.Context(), upstream); err != nil {
		t.Fatal(err)
	}

	err := db.UpstreamMiAuth.Deny(t.Context(), upstream.ID, now.Add(time.Hour))
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Deny() on expired session error = %v, want ErrConflict", err)
	}
}

func TestLocalMiAuthSessionRepository_AuthorizeThenConsume(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	s := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}

	if err := db.LocalMiAuth.Authorize(t.Context(), s.RouteSessionID, actorID, now); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	consumed, err := db.LocalMiAuth.Consume(t.Context(), s.RouteSessionID, now)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if consumed.Status != domain.MiAuthConsumed || consumed.LocalActorID == nil || *consumed.LocalActorID != actorID {
		t.Errorf("Consume() returned %+v, want consumed session for actor %q", consumed, actorID)
	}

	got, err := db.LocalMiAuth.Get(t.Context(), s.RouteSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.MiAuthConsumed {
		t.Errorf("Status = %q, want consumed", got.Status)
	}
	if got.LocalActorID == nil || *got.LocalActorID != actorID {
		t.Errorf("LocalActorID = %v, want %q", got.LocalActorID, actorID)
	}
}

// TestLocalMiAuthSessionRepository_Consume_OnlyOneWinnerUnderRace backs
// ADR-0001's "a check that races another check can have only one winner"
// requirement: two concurrent Consume calls against the same authorized
// session must not both succeed.
func TestLocalMiAuthSessionRepository_Consume_OnlyOneWinnerUnderRace(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	s := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if err := db.LocalMiAuth.Authorize(t.Context(), s.RouteSessionID, actorID, now); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	successes := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := db.LocalMiAuth.Consume(t.Context(), s.RouteSessionID, now)
			successes[i] = err == nil
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range successes {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
}

func TestUpstreamMiAuthSessionRepository_AuthorizeThenConsume(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	local := domain.LocalMiAuthSession{
		RouteSessionID: domain.NewID(), State: domain.NewID(), Status: domain.MiAuthCreated,
		RequestedPermissions: "read:account", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), local); err != nil {
		t.Fatal(err)
	}

	upstream := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), LocalSessionID: &local.RouteSessionID, IdentityOrigin: "https://misskey.example",
		State: domain.NewID(), Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.UpstreamMiAuth.Create(t.Context(), upstream); err != nil {
		t.Fatal(err)
	}

	if err := db.UpstreamMiAuth.Authorize(t.Context(), upstream.ID, "upstream-user-1", now); err != nil {
		t.Fatal(err)
	}
	if err := db.UpstreamMiAuth.Consume(t.Context(), upstream.ID, now); err != nil {
		t.Fatal(err)
	}

	got, err := db.UpstreamMiAuth.Get(t.Context(), upstream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.MiAuthConsumed {
		t.Errorf("Status = %q, want consumed", got.Status)
	}
	if got.UpstreamUserID == nil || *got.UpstreamUserID != "upstream-user-1" {
		t.Errorf("UpstreamUserID = %v, want upstream-user-1", got.UpstreamUserID)
	}
}

// TestUpstreamMiAuthSessionRepository_RequiresExactlyOneBinding backs the
// schema CHECK that an upstream session is bound to exactly one of a
// local session or a bootstrap gate.
func TestUpstreamMiAuthSessionRepository_RequiresExactlyOneBinding(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	neither := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), IdentityOrigin: "https://misskey.example", State: domain.NewID(),
		Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.UpstreamMiAuth.Create(t.Context(), neither); err == nil {
		t.Error("expected an error creating an upstream session bound to neither a local session nor a bootstrap gate")
	}
}

// TestUpstreamMiAuthSessionRepository_Create_RejectsDuplicateBootstrapGateID
// backs the single-use bootstrap gate design: a bootstrap gate must bind
// to at most one upstream session, so a second upstream session created
// against the same gate must fail rather than silently sharing it.
func TestUpstreamMiAuthSessionRepository_Create_RejectsDuplicateBootstrapGateID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	gate := domain.BootstrapGate{
		ID: domain.NewID(), Status: domain.BootstrapGateIssued, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := db.BootstrapGates.Create(t.Context(), gate); err != nil {
		t.Fatal(err)
	}

	first := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), BootstrapGateID: &gate.ID, IdentityOrigin: "https://misskey.example",
		State: domain.NewID(), Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.UpstreamMiAuth.Create(t.Context(), first); err != nil {
		t.Fatal(err)
	}

	dup := domain.UpstreamMiAuthSession{
		ID: domain.NewID(), BootstrapGateID: &gate.ID, IdentityOrigin: "https://misskey.example",
		State: domain.NewID(), Status: domain.MiAuthCreated, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	err := db.UpstreamMiAuth.Create(t.Context(), dup)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create() error = %v, want ErrConflict", err)
	}
}

func TestAPITokenRepository_CreateGetRevoke(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	tok := domain.APIToken{
		ID: domain.NewID(), TokenHash: "hash-1", LocalActorID: actorID, Scopes: "read:account write:notes",
		CreatedAt: now,
	}
	if err := db.APITokens.Create(t.Context(), tok); err != nil {
		t.Fatal(err)
	}

	got, err := db.APITokens.GetByTokenHash(t.Context(), "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != tok.ID {
		t.Errorf("ID = %q, want %q", got.ID, tok.ID)
	}
	if got.RevokedAt != nil {
		t.Error("RevokedAt should be nil before revocation")
	}

	if err := db.APITokens.TouchLastUsed(t.Context(), tok.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := db.APITokens.Revoke(t.Context(), tok.ID, now); err != nil {
		t.Fatal(err)
	}

	got, err = db.APITokens.GetByTokenHash(t.Context(), "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt == nil {
		t.Error("RevokedAt should be set after revocation")
	}
	if got.LastUsedAt == nil {
		t.Error("LastUsedAt should be set")
	}
}

func TestAPITokenRepository_Create_RejectsDuplicateHash(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	tok := domain.APIToken{ID: domain.NewID(), TokenHash: "dup", LocalActorID: actorID, Scopes: "read:account", CreatedAt: now}
	if err := db.APITokens.Create(t.Context(), tok); err != nil {
		t.Fatal(err)
	}

	dup := domain.APIToken{ID: domain.NewID(), TokenHash: "dup", LocalActorID: actorID, Scopes: "read:account", CreatedAt: now}
	err := db.APITokens.Create(t.Context(), dup)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create() error = %v, want ErrConflict", err)
	}
}

// TestOwnerBindingRepository_Bind_OnlyOneWinsConcurrentAttempt backs
// ADR-0001's "concurrent bootstrap race has exactly one winner" via the
// owner_bindings singleton row's primary key.
func TestOwnerBindingRepository_Bind_OnlyOneWinsConcurrentAttempt(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()

	const n = 8
	var wg sync.WaitGroup
	successes := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := db.OwnerBindings.Bind(t.Context(), domain.OwnerBinding{
				LocalActorID: actorID, IdentityOrigin: "https://misskey.example",
				UpstreamUserID: "user", BoundAt: now,
			})
			successes[i] = err == nil
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range successes {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}

	got, err := db.OwnerBindings.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamUserID != "user" {
		t.Errorf("UpstreamUserID = %q, want user", got.UpstreamUserID)
	}
}

func TestOwnerBindingRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.OwnerBindings.Get(t.Context())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestUpstreamTokenRepository_PutGetDelete(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	if err := db.OwnerBindings.Bind(t.Context(), domain.OwnerBinding{
		LocalActorID: actorID, IdentityOrigin: "https://misskey.example", UpstreamUserID: "user", BoundAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	tok := domain.UpstreamToken{Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: "v1", CreatedAt: now}
	if err := db.UpstreamTokens.Put(t.Context(), tok); err != nil {
		t.Fatal(err)
	}

	got, err := db.UpstreamTokens.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Ciphertext) != "cipher" {
		t.Errorf("Ciphertext = %q, want cipher", got.Ciphertext)
	}

	rotated := domain.UpstreamToken{Ciphertext: []byte("cipher2"), Nonce: []byte("nonce2"), KeyVersion: "v2", CreatedAt: now}
	if err := db.UpstreamTokens.Put(t.Context(), rotated); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	got, err = db.UpstreamTokens.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Ciphertext) != "cipher2" {
		t.Errorf("Ciphertext after rotation = %q, want cipher2", got.Ciphertext)
	}

	if err := db.UpstreamTokens.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpstreamTokens.Get(t.Context()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() after Delete error = %v, want ErrNotFound", err)
	}
}
