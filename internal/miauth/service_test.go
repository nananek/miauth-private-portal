package miauth

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type testService struct {
	*Service
	db    *sqlite.DB
	clock *fakeClock
}

func newTestService(t *testing.T) *testService {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second, MaxOpenConns: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	service := NewService(db, db.Repos, Config{
		ClientCallbacks: []string{"aria://aria/miauth"}, OwnerUsername: "owner",
		OwnerDisplayName: "Test Owner", Clock: clock,
	})
	return &testService{Service: service, db: db, clock: clock}
}

func TestStartLocalSession_CreateResumeAndValidation(t *testing.T) {
	ts := newTestService(t)
	callback := "aria://aria/miauth"
	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", &callback); err != nil {
		t.Fatal(err)
	}
	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", &callback); err != nil {
		t.Fatalf("matching retry: %v", err)
	}
	if err := ts.StartLocalSession(t.Context(), "route-1", "write:notes", &callback); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("mismatched retry error = %v, want ErrSessionUnavailable", err)
	}
	disallowed := "https://evil.example/callback"
	if err := ts.StartLocalSession(t.Context(), "route-2", "read:account", &disallowed); !errors.Is(err, ErrClientCallbackNotAllowed) {
		t.Fatalf("disallowed callback error = %v, want ErrClientCallbackNotAllowed", err)
	}
	if _, err := ts.db.LocalMiAuth.Get(t.Context(), "route-2"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disallowed callback created a session: %v", err)
	}
}

func TestStartLocalSession_ExpiredCannotResume(t *testing.T) {
	ts := newTestService(t)
	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(11 * time.Minute)
	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("expired retry error = %v, want ErrSessionUnavailable", err)
	}
}

func TestApproveSession_CreatesAndReusesOwner(t *testing.T) {
	ts := newTestService(t)
	for _, id := range []string{"route-1", "route-2"} {
		if err := ts.StartLocalSession(t.Context(), id, "read:account", nil); err != nil {
			t.Fatal(err)
		}
		if err := ts.ApproveSession(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	second, _ := ts.db.LocalMiAuth.Get(t.Context(), "route-2")
	if first.LocalActorID == nil || second.LocalActorID == nil || *first.LocalActorID != *second.LocalActorID {
		t.Fatalf("approved sessions did not converge on one owner: %+v %+v", first, second)
	}
	owner, err := ts.db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil || owner.ID != *first.LocalActorID {
		t.Fatalf("owner = %+v, err = %v", owner, err)
	}
	if err := ts.ApproveSession(t.Context(), "route-1"); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("second approval error = %v, want ErrSessionUnavailable", err)
	}
}

func TestApproveSession_ConcurrentInitialApprovalsConverge(t *testing.T) {
	ts := newTestService(t)
	for _, id := range []string{"route-1", "route-2"} {
		if err := ts.StartLocalSession(t.Context(), id, "read:account", nil); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"route-1", "route-2"} {
		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			<-start
			errs <- ts.ApproveSession(t.Context(), sessionID)
		}(id)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ApproveSession: %v", err)
		}
	}
	first, _ := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	second, _ := ts.db.LocalMiAuth.Get(t.Context(), "route-2")
	if first.LocalActorID == nil || second.LocalActorID == nil || *first.LocalActorID != *second.LocalActorID {
		t.Fatalf("concurrent approvals used different owners: %+v %+v", first, second)
	}
}

func TestApproveAndRejectUnavailableSessions(t *testing.T) {
	ts := newTestService(t)
	if err := ts.ApproveSession(t.Context(), "missing"); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("missing approval error = %v", err)
	}
	if err := ts.StartLocalSession(t.Context(), "expired", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(11 * time.Minute)
	if err := ts.ApproveSession(t.Context(), "expired"); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("expired approval error = %v", err)
	}
	if err := ts.RejectSession(t.Context(), "expired"); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("expired rejection error = %v", err)
	}
}

func TestRejectAndListPendingSessions(t *testing.T) {
	ts := newTestService(t)
	for _, id := range []string{"keep", "reject", "approve"} {
		if err := ts.StartLocalSession(t.Context(), id, "read:account", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.RejectSession(t.Context(), "reject"); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveSession(t.Context(), "approve"); err != nil {
		t.Fatal(err)
	}
	pending, err := ts.ListPendingSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RouteSessionID != "keep" {
		t.Fatalf("pending = %+v, want keep only", pending)
	}
}

func TestCheckTokenListRevokeAndDescribeOwner(t *testing.T) {
	ts := newTestService(t)
	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account,write:notes", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Check(t.Context(), "route-1"); !errors.Is(err, ErrCheckNotReady) {
		t.Fatalf("pending Check error = %v", err)
	}
	if err := ts.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatal(err)
	}
	result, err := ts.Check(t.Context(), "route-1")
	if err != nil || result.Token == "" {
		t.Fatalf("Check result = %+v, err = %v", result, err)
	}
	profile, err := ts.DescribeOwner(t.Context(), result.OwnerActorID)
	if err != nil || profile.ActorID != result.OwnerActorID || profile.DisplayName != "Test Owner" {
		t.Fatalf("profile = %+v, err = %v", profile, err)
	}
	if actorID, err := ts.VerifyToken(t.Context(), result.Token, ScopeReadAccount); err != nil || actorID != result.OwnerActorID {
		t.Fatalf("VerifyToken actor = %q, err = %v", actorID, err)
	}
	if _, err := ts.Check(t.Context(), "route-1"); !errors.Is(err, ErrCheckNotReady) {
		t.Fatalf("replayed Check error = %v", err)
	}
	tokens, err := ts.ListAPITokens(t.Context())
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %+v, err = %v", tokens, err)
	}
	if err := ts.RevokeAPIToken(t.Context(), tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.VerifyToken(t.Context(), result.Token, ScopeReadAccount); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("revoked VerifyToken error = %v", err)
	}
}

// TestUpdateOwnerDisplayName_PersistsAndReplacesConfigValue is Issue #23
// PR1's core case: once the owner exists, DescribeOwner/Check must read
// display name from the actors row, not from cfg.OwnerDisplayName ("Test
// Owner" here) at all — updating it must fully replace the initial
// config-seeded value, not merely append to or coexist with it.
func TestUpdateOwnerDisplayName_PersistsAndReplacesConfigValue(t *testing.T) {
	ts := newTestService(t)
	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatal(err)
	}
	result, err := ts.Check(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := ts.UpdateOwnerDisplayName(t.Context(), result.OwnerActorID, "New Display Name")
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "New Display Name" {
		t.Fatalf("UpdateOwnerDisplayName result.DisplayName = %q, want %q", updated.DisplayName, "New Display Name")
	}
	if updated.Username != "owner" {
		t.Fatalf("UpdateOwnerDisplayName result.Username = %q, want unchanged %q", updated.Username, "owner")
	}

	profile, err := ts.DescribeOwner(t.Context(), result.OwnerActorID)
	if err != nil || profile.DisplayName != "New Display Name" {
		t.Fatalf("DescribeOwner after update = %+v, err = %v, want DisplayName %q", profile, err, "New Display Name")
	}

	// Clearing back to "" must also persist (not be confused with "never
	// set"): DescribeOwner must report "" again, not fall back to the
	// original config value or the previous display name.
	if _, err := ts.UpdateOwnerDisplayName(t.Context(), result.OwnerActorID, ""); err != nil {
		t.Fatal(err)
	}
	if profile, err := ts.DescribeOwner(t.Context(), result.OwnerActorID); err != nil || profile.DisplayName != "" {
		t.Fatalf("DescribeOwner after clearing = %+v, err = %v, want empty DisplayName", profile, err)
	}
}

// TestUpdateOwnerDisplayName_RejectsNonOwnerActor is a defense-in-depth
// check: no real code path issues a write:account-scoped token bound to
// a non-owner actor (RequireScope/VerifyToken only ever resolve to the
// owner), but UpdateOwnerDisplayName must still refuse to silently edit
// the reserved assistant/system presentation actors if it were ever
// called with one of their IDs.
func TestUpdateOwnerDisplayName_RejectsNonOwnerActor(t *testing.T) {
	ts := newTestService(t)
	if err := ts.db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatal(err)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.UpdateOwnerDisplayName(t.Context(), assistant.ID, "Should Not Apply"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("UpdateOwnerDisplayName on assistant actor error = %v, want ErrNotOwner", err)
	}
	after, err := ts.db.Actors.Get(t.Context(), assistant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DisplayName != nil {
		t.Fatalf("assistant actor DisplayName = %v, want unchanged nil", after.DisplayName)
	}
}

// TestBackfillOwnerDisplayName_SeedsNilDisplayName covers the pre-Issue-#23
// PR1 owner row case: an Owner actor created before migration 0012 added
// display_name (or by a caller whose Config left OwnerDisplayName unset)
// has display_name = NULL, and DescribeOwner must not keep reporting ""
// forever once cfg.OwnerDisplayName ("Test Owner", from newTestService)
// is available to backfill it from.
func TestBackfillOwnerDisplayName_SeedsNilDisplayName(t *testing.T) {
	ts := newTestService(t)
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: ts.clock.Now()}
	if err := ts.db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatal(err)
	}

	if err := ts.BackfillOwnerDisplayName(t.Context()); err != nil {
		t.Fatalf("BackfillOwnerDisplayName: %v", err)
	}

	profile, err := ts.DescribeOwner(t.Context(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.DisplayName != "Test Owner" {
		t.Errorf("DescribeOwner.DisplayName after backfill = %q, want %q", profile.DisplayName, "Test Owner")
	}
}

// TestBackfillOwnerDisplayName_LeavesExplicitValueAlone ensures the
// backfill never overwrites a display name that was already explicitly
// set, including an explicit "" from UpdateOwnerDisplayName clearing it
// — that must stay distinct from "never set" (see SetDisplayName's doc
// comment) and must never revert to the config value.
func TestBackfillOwnerDisplayName_LeavesExplicitValueAlone(t *testing.T) {
	ts := newTestService(t)
	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatal(err)
	}
	result, err := ts.Check(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.UpdateOwnerDisplayName(t.Context(), result.OwnerActorID, ""); err != nil {
		t.Fatal(err)
	}

	if err := ts.BackfillOwnerDisplayName(t.Context()); err != nil {
		t.Fatalf("BackfillOwnerDisplayName: %v", err)
	}

	profile, err := ts.DescribeOwner(t.Context(), result.OwnerActorID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.DisplayName != "" {
		t.Errorf("DescribeOwner.DisplayName after backfill = %q, want unchanged empty string", profile.DisplayName)
	}
}

// TestBackfillOwnerDisplayName_NoOwnerYet must be a no-op, not an error,
// when no MiAuth session has ever been approved yet.
func TestBackfillOwnerDisplayName_NoOwnerYet(t *testing.T) {
	ts := newTestService(t)
	if err := ts.BackfillOwnerDisplayName(t.Context()); err != nil {
		t.Fatalf("BackfillOwnerDisplayName with no owner yet: %v", err)
	}
}

// mustCreateVirtualActor inserts one Open WebUI VirtualActor row (Issue
// #52 PR1). Migration 0016 made such a row possible; these tests exist to
// prove that possibility never became a second way to log in.
func mustCreateVirtualActor(t *testing.T, ts *testService) domain.Actor {
	t.Helper()
	displayName := "Model Display Name"
	a := domain.Actor{
		ID:          domain.NewID(),
		Type:        domain.ActorOpenWebUIModel,
		CreatedAt:   ts.clock.Now(),
		DisplayName: &displayName,
	}
	if err := ts.db.Actors.Create(t.Context(), a); err != nil {
		t.Fatalf("create openwebui_model actor: %v", err)
	}
	return a
}

// TestApproveSession_NeverBindsToOpenWebUIModelActor is the structural
// half of Issue #52's "VirtualActors cannot log in" requirement: the
// VirtualActor row exists before any owner does, and approval must still
// create and bind the owner rather than adopting the actor that happens
// to already be there.
func TestApproveSession_NeverBindsToOpenWebUIModelActor(t *testing.T) {
	ts := newTestService(t)
	virtual := mustCreateVirtualActor(t, ts)

	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatal(err)
	}

	session, err := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}
	if session.LocalActorID == nil {
		t.Fatal("approved session has no local actor")
	}
	if *session.LocalActorID == virtual.ID {
		t.Fatal("approved session bound to the Open WebUI VirtualActor")
	}
	owner, err := ts.db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatalf("approval should have created the owner actor: %v", err)
	}
	if *session.LocalActorID != owner.ID {
		t.Fatalf("session actor = %q, want owner %q", *session.LocalActorID, owner.ID)
	}
	if bound, err := ts.db.Actors.Get(t.Context(), *session.LocalActorID); err != nil {
		t.Fatal(err)
	} else if !bound.CanMiAuth() {
		t.Fatalf("session bound to an actor that cannot MiAuth: %+v", bound)
	}
}

// TestCheckAndVerifyToken_NeverResolveToOpenWebUIModelActor covers the
// token side of the same requirement: with a VirtualActor present, the
// issued API token still resolves to the owner, and the VirtualActor
// holds no token of its own.
func TestCheckAndVerifyToken_NeverResolveToOpenWebUIModelActor(t *testing.T) {
	ts := newTestService(t)
	virtual := mustCreateVirtualActor(t, ts)

	if err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveSession(t.Context(), "route-1"); err != nil {
		t.Fatal(err)
	}
	result, err := ts.Check(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.OwnerActorID == virtual.ID {
		t.Fatal("Check issued a token for the Open WebUI VirtualActor")
	}

	actorID, err := ts.VerifyToken(t.Context(), result.Token, ScopeReadAccount)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ts.db.Actors.Get(t.Context(), actorID)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.IsLoginable() {
		t.Fatalf("VerifyToken resolved to a non-loginable actor: %+v", resolved)
	}

	tokens, err := ts.ListAPITokens(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if tok.LocalActorID == virtual.ID {
			t.Fatalf("an API token is bound to the Open WebUI VirtualActor: %+v", tok)
		}
	}
}

// TestUpdateOwnerDisplayName_RejectsOpenWebUIModelActor extends
// TestUpdateOwnerDisplayName_RejectsNonOwnerActor to the new actor type:
// a VirtualActor's display name belongs to the Open WebUI registry
// (Issue #52 PR3), and POST /api/i/update must never reach it.
func TestUpdateOwnerDisplayName_RejectsOpenWebUIModelActor(t *testing.T) {
	ts := newTestService(t)
	virtual := mustCreateVirtualActor(t, ts)

	if _, err := ts.UpdateOwnerDisplayName(t.Context(), virtual.ID, "Should Not Apply"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("UpdateOwnerDisplayName on VirtualActor error = %v, want ErrNotOwner", err)
	}
	after, err := ts.db.Actors.Get(t.Context(), virtual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DisplayName == nil || *after.DisplayName != "Model Display Name" {
		t.Fatalf("VirtualActor DisplayName = %v, want unchanged %q", after.DisplayName, "Model Display Name")
	}
}

// TestBackfillOwnerDisplayName_LeavesOpenWebUIModelActorAlone guards the
// one remaining write in this package that finds an actor by type rather
// than by ID: it must keep resolving the owner even when other actor
// rows exist, and never write into a VirtualActor whose display name is
// deliberately NULL until the registry sets it.
func TestBackfillOwnerDisplayName_LeavesOpenWebUIModelActorAlone(t *testing.T) {
	ts := newTestService(t)
	virtual := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: ts.clock.Now()}
	if err := ts.db.Actors.Create(t.Context(), virtual); err != nil {
		t.Fatal(err)
	}
	if err := ts.db.Actors.Create(t.Context(), domain.Actor{
		ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: ts.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := ts.BackfillOwnerDisplayName(t.Context()); err != nil {
		t.Fatalf("BackfillOwnerDisplayName: %v", err)
	}

	owner, err := ts.db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatal(err)
	}
	if owner.DisplayName == nil || *owner.DisplayName != "Test Owner" {
		t.Fatalf("owner DisplayName = %v, want %q", owner.DisplayName, "Test Owner")
	}
	after, err := ts.db.Actors.Get(t.Context(), virtual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DisplayName != nil {
		t.Fatalf("VirtualActor DisplayName = %v, want unchanged nil", after.DisplayName)
	}
}
