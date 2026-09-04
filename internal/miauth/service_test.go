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
