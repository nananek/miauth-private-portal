package miauth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func defaultTestConfig() Config {
	return Config{
		IdentityOrigin:       testIdentityOrigin,
		AllowedMisskeyUserID: testAllowedUserID,
		ClientCallbacks:      []string{"aria://aria/miauth"},
		OwnerUsername:        "owner",
	}
}

func TestStartLocalSession_CreatesLinkedUpstreamSession(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())

	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account,write:notes", nil)
	if err != nil {
		t.Fatalf("StartLocalSession: %v", err)
	}
	if started.UpstreamSessionID == "" {
		t.Fatal("StartLocalSession returned an empty upstream session ID")
	}

	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if upstream.LocalSessionID == nil || *upstream.LocalSessionID != "route-1" {
		t.Errorf("upstream.LocalSessionID = %v, want route-1", upstream.LocalSessionID)
	}
	if upstream.IdentityOrigin != testIdentityOrigin {
		t.Errorf("upstream.IdentityOrigin = %q, want %q", upstream.IdentityOrigin, testIdentityOrigin)
	}
}

func TestStartLocalSession_IsIdempotentForMatchingRetry(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())

	first, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatalf("second StartLocalSession: %v", err)
	}
	if first != second {
		t.Errorf("retry resumed a different upstream session: %q != %q", first, second)
	}
}

func TestStartLocalSession_RejectsMismatchedRetryWithoutMutating(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())

	first, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = ts.StartLocalSession(t.Context(), "route-1", "write:notes", nil)
	if !errors.Is(err, ErrSessionUnavailable) {
		t.Errorf("StartLocalSession() with a differing permission error = %v, want ErrSessionUnavailable", err)
	}

	// The original pending attempt must be untouched.
	local, err := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}
	if local.Status != domain.MiAuthCreated || local.RequestedPermissions != "read:account" {
		t.Errorf("original session was mutated by the rejected retry: %+v", local)
	}

	again, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("the original attempt should still be resumable with its original request")
	}
}

func TestStartLocalSession_RejectsDisallowedClientCallback(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	cb := "https://evil.example/callback"
	_, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", &cb)
	if !errors.Is(err, ErrClientCallbackNotAllowed) {
		t.Errorf("StartLocalSession() with disallowed callback error = %v, want ErrClientCallbackNotAllowed", err)
	}
}

func TestStartLocalSession_AllowsAllowlistedClientCallback(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	cb := "aria://aria/miauth"
	_, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", &cb)
	if err != nil {
		t.Fatalf("StartLocalSession() with allowlisted callback: %v", err)
	}
}

// beginAndAuthorize drives StartLocalSession through a successful
// HandleUpstreamCallback for testAllowedUserID, returning the route
// session ID a Check call can then complete.
func beginAndAuthorize(t *testing.T, ts *testService, routeSessionID string) {
	t.Helper()
	started, err := ts.StartLocalSession(t.Context(), routeSessionID, "read:account,write:notes", nil)
	if err != nil {
		t.Fatalf("StartLocalSession: %v", err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return testAllowedUserID, true, nil
	}

	if _, err := ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State); err != nil {
		t.Fatalf("HandleUpstreamCallback: %v", err)
	}
}

func TestHandleUpstreamCallback_SuccessAuthorizesLocalSession(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	beginAndAuthorize(t, ts, "route-1")

	local, err := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}
	if local.Status != domain.MiAuthAuthorized {
		t.Errorf("local.Status = %q, want authorized", local.Status)
	}
	if local.LocalActorID == nil {
		t.Fatal("local.LocalActorID was not set")
	}

	owner, err := ts.db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatal(err)
	}
	if owner.ID != *local.LocalActorID {
		t.Errorf("owner actor ID = %q, local session bound to %q", owner.ID, *local.LocalActorID)
	}

	binding, err := ts.db.OwnerBindings.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if binding.UpstreamUserID != testAllowedUserID || binding.IdentityOrigin != testIdentityOrigin {
		t.Errorf("binding = %+v, want user %q at %q", binding, testAllowedUserID, testIdentityOrigin)
	}
}

func TestHandleUpstreamCallback_SecondOwnerReusesExistingOwnerActor(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	beginAndAuthorize(t, ts, "route-1")

	owner1, err := ts.db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatal(err)
	}

	// A later, already-bound-deployment login by the same owner must
	// reuse the existing Owner actor, never create a second one.
	beginAndAuthorize(t, ts, "route-2")

	owner2, err := ts.db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatal(err)
	}
	if owner1.ID != owner2.ID {
		t.Errorf("a second successful login created a second owner actor: %q != %q", owner1.ID, owner2.ID)
	}
}

func TestHandleUpstreamCallback_WrongUserDeniesBothSessions(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return "someone-else", true, nil
	}

	_, err = ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State)
	if !errors.Is(err, ErrOwnerBindingDenied) {
		t.Fatalf("HandleUpstreamCallback() with wrong user error = %v, want ErrOwnerBindingDenied", err)
	}

	gotUpstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUpstream.Status != domain.MiAuthDenied {
		t.Errorf("upstream.Status = %q, want denied", gotUpstream.Status)
	}
	local, err := ts.db.LocalMiAuth.Get(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}
	if local.Status != domain.MiAuthDenied {
		t.Errorf("local.Status = %q, want denied", local.Status)
	}

	if _, err := ts.db.OwnerBindings.Get(t.Context()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("OwnerBindings.Get() after a denied attempt error = %v, want ErrNotFound", err)
	}
}

func TestHandleUpstreamCallback_UpstreamNotOKDeniesSession(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return "", false, nil
	}

	_, err = ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State)
	if !errors.Is(err, ErrUpstreamVerification) {
		t.Fatalf("HandleUpstreamCallback() with ok=false error = %v, want ErrUpstreamVerification", err)
	}

	gotUpstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUpstream.Status != domain.MiAuthDenied {
		t.Errorf("upstream.Status = %q, want denied", gotUpstream.Status)
	}
}

func TestHandleUpstreamCallback_UpstreamTransportErrorDoesNotDenyAndAllowsRetry(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return "", false, errors.New("upstream timed out")
	}

	_, err = ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State)
	if !errors.Is(err, ErrUpstreamVerification) {
		t.Fatalf("HandleUpstreamCallback() with a transport error, error = %v, want ErrUpstreamVerification", err)
	}

	gotUpstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUpstream.Status != domain.MiAuthCreated {
		t.Errorf("a transport error denied the session: Status = %q, want created (retryable)", gotUpstream.Status)
	}

	// A retry after upstream recovers must still be able to succeed.
	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return testAllowedUserID, true, nil
	}
	if _, err := ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State); err != nil {
		t.Fatalf("retry after upstream recovered: %v", err)
	}
}

func TestHandleUpstreamCallback_WrongStateDoesNotMutateSessionAndAllowsRetry(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return testAllowedUserID, true, nil
	}

	// A wrong state guess must not consume upstream.Check at all, and
	// must not deny the session — see HandleUpstreamCallback's doc
	// comment for why.
	_, err = ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, "wrong-state")
	if !errors.Is(err, ErrCallbackInvalid) {
		t.Fatalf("HandleUpstreamCallback() with wrong state error = %v, want ErrCallbackInvalid", err)
	}
	if ts.provider.calls != 0 {
		t.Errorf("upstream.Check was called %d times for a callback that never passed state validation", ts.provider.calls)
	}

	gotUpstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUpstream.Status != domain.MiAuthCreated {
		t.Errorf("a wrong state guess mutated the session: Status = %q, want created", gotUpstream.Status)
	}

	// The legitimate holder can still complete the flow with the
	// correct state afterward.
	if _, err := ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State); err != nil {
		t.Fatalf("HandleUpstreamCallback with the correct state after a wrong guess: %v", err)
	}
}

func TestHandleUpstreamCallback_UnknownIDIsInvalid(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	_, err := ts.HandleUpstreamCallback(t.Context(), "does-not-exist", "any-state")
	if !errors.Is(err, ErrCallbackInvalid) {
		t.Errorf("HandleUpstreamCallback() for an unknown id error = %v, want ErrCallbackInvalid", err)
	}
}

func TestHandleUpstreamCallback_StorageFailureIsNotCallbackInvalid(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	if err := ts.db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := ts.HandleUpstreamCallback(t.Context(), "session", "state")
	if err == nil {
		t.Fatal("HandleUpstreamCallback() error = nil, want storage failure")
	}
	if errors.Is(err, ErrCallbackInvalid) {
		t.Errorf("storage failure was misclassified as ErrCallbackInvalid: %v", err)
	}
}

func TestHandleUpstreamCallback_ExpiredSessionIsInvalid(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.clock.Advance(upstreamSessionTTL + 1)

	_, err = ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State)
	if !errors.Is(err, ErrCallbackInvalid) {
		t.Errorf("HandleUpstreamCallback() for an expired session error = %v, want ErrCallbackInvalid", err)
	}
}

func TestHandleUpstreamCallback_ExpiryDuringUpstreamCheckIsCallbackInvalid(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		ts.clock.Advance(upstreamSessionTTL)
		return testAllowedUserID, true, nil
	}

	_, err = ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State)
	if !errors.Is(err, ErrCallbackInvalid) {
		t.Errorf("HandleUpstreamCallback() after expiring in upstream check = %v, want ErrCallbackInvalid", err)
	}

	got, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.MiAuthCreated {
		t.Errorf("expired callback mutated upstream status to %q, want created", got.Status)
	}
	if _, err := ts.db.OwnerBindings.Get(t.Context()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expired callback created an owner binding: %v", err)
	}
}

func TestCheck_SuccessIssuesTokenAndConsumesSession(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	beginAndAuthorize(t, ts, "route-1")

	result, err := ts.Check(t.Context(), "route-1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Token == "" {
		t.Error("Check() returned an empty token")
	}
	if result.OwnerUsername != "owner" {
		t.Errorf("OwnerUsername = %q, want owner", result.OwnerUsername)
	}

	tok, err := ts.db.APITokens.GetByTokenHash(t.Context(), hashAPIToken(result.Token))
	if err != nil {
		t.Fatal(err)
	}
	if tok.LocalActorID != result.OwnerActorID {
		t.Errorf("stored token actor = %q, want %q", tok.LocalActorID, result.OwnerActorID)
	}
	wantScopes := scopesString([]string{ScopeReadNotes, ScopeReadAccount, ScopeWriteNotes})
	if tok.Scopes != wantScopes {
		t.Errorf("stored token scopes = %q, want %q", tok.Scopes, wantScopes)
	}
}

func TestCheck_ReplayFails(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	beginAndAuthorize(t, ts, "route-1")

	if _, err := ts.Check(t.Context(), "route-1"); err != nil {
		t.Fatalf("first Check: %v", err)
	}
	if _, err := ts.Check(t.Context(), "route-1"); !errors.Is(err, ErrCheckNotReady) {
		t.Errorf("replayed Check() error = %v, want ErrCheckNotReady", err)
	}
}

func TestCheck_PendingSessionFails(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	if _, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Check(t.Context(), "route-1"); !errors.Is(err, ErrCheckNotReady) {
		t.Errorf("Check() on a still-pending session error = %v, want ErrCheckNotReady", err)
	}
}

func TestCheck_UnknownSessionFails(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	if _, err := ts.Check(t.Context(), "does-not-exist"); !errors.Is(err, ErrCheckNotReady) {
		t.Errorf("Check() for an unknown session error = %v, want ErrCheckNotReady", err)
	}
}

func TestCheck_DeniedSessionFails(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	started, err := ts.StartLocalSession(t.Context(), "route-1", "read:account", nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	ts.provider.check = func(context.Context, string) (string, bool, error) { return "someone-else", true, nil }
	if _, err := ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State); !errors.Is(err, ErrOwnerBindingDenied) {
		t.Fatal(err)
	}

	if _, err := ts.Check(t.Context(), "route-1"); !errors.Is(err, ErrCheckNotReady) {
		t.Errorf("Check() on a denied session error = %v, want ErrCheckNotReady", err)
	}
}

// TestCheck_ConcurrentCallsHaveExactlyOneWinner backs ADR-0001's
// requirement that a racing check() call can have only one winner.
func TestCheck_ConcurrentCallsHaveExactlyOneWinner(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	beginAndAuthorize(t, ts, "route-1")

	const n = 8
	var wg sync.WaitGroup
	successes := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ts.Check(t.Context(), "route-1")
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

func TestDescribeOwner(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.OwnerDisplayName = "Test Owner"
	ts := newTestService(t, cfg)
	beginAndAuthorize(t, ts, "route-1")
	result, err := ts.Check(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}

	profile, err := ts.DescribeOwner(t.Context(), result.OwnerActorID)
	if err != nil {
		t.Fatalf("DescribeOwner: %v", err)
	}
	if profile.ActorID != result.OwnerActorID {
		t.Errorf("ActorID = %q, want %q", profile.ActorID, result.OwnerActorID)
	}
	if profile.Username != cfg.OwnerUsername {
		t.Errorf("Username = %q, want %q", profile.Username, cfg.OwnerUsername)
	}
	if profile.DisplayName != cfg.OwnerDisplayName {
		t.Errorf("DisplayName = %q, want %q", profile.DisplayName, cfg.OwnerDisplayName)
	}
	if !profile.CreatedAt.Equal(result.OwnerCreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", profile.CreatedAt, result.OwnerCreatedAt)
	}

	if _, err := ts.DescribeOwner(t.Context(), "missing-actor"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DescribeOwner(missing) error = %v, want ErrNotFound", err)
	}
}

func TestVerifyToken(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	beginAndAuthorize(t, ts, "route-1")
	result, err := ts.Check(t.Context(), "route-1")
	if err != nil {
		t.Fatal(err)
	}

	actorID, err := ts.VerifyToken(t.Context(), result.Token, ScopeReadAccount)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if actorID != result.OwnerActorID {
		t.Errorf("VerifyToken() actor = %q, want %q", actorID, result.OwnerActorID)
	}

	if _, err := ts.VerifyToken(t.Context(), "not-a-real-token", ScopeReadAccount); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("VerifyToken() with an unknown token error = %v, want ErrTokenInvalid", err)
	}

	// A token never grants a scope it was not issued with, even one
	// that sounds related.
	if _, err := ts.VerifyToken(t.Context(), result.Token, "write:account"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("VerifyToken() with an unrequested scope error = %v, want ErrTokenInvalid", err)
	}

	tok, err := ts.db.APITokens.GetByTokenHash(t.Context(), hashAPIToken(result.Token))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.db.APITokens.Revoke(t.Context(), tok.ID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.VerifyToken(t.Context(), result.Token, ScopeReadAccount); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("VerifyToken() with a revoked token error = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyToken_StorageFailureIsNotTokenInvalid(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	if err := ts.db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := ts.VerifyToken(t.Context(), "token", ScopeReadAccount)
	if err == nil {
		t.Fatal("VerifyToken() error = nil, want storage failure")
	}
	if errors.Is(err, ErrTokenInvalid) {
		t.Errorf("storage failure was misclassified as ErrTokenInvalid: %v", err)
	}
}

func TestBootstrapFlow_SucceedsWithoutAnAllowlist(t *testing.T) {
	ts := newTestService(t, Config{IdentityOrigin: testIdentityOrigin, OwnerUsername: "owner"})

	gateID, err := ts.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatalf("IssueBootstrapGate: %v", err)
	}

	started, err := ts.StartBootstrapSession(t.Context(), gateID)
	if err != nil {
		t.Fatalf("StartBootstrapSession: %v", err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}

	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return "any-upstream-user", true, nil
	}

	result, err := ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State)
	if err != nil {
		t.Fatalf("HandleUpstreamCallback: %v", err)
	}
	if result.ClientCallback != nil || result.RouteSessionID != "" {
		t.Errorf("bootstrap callback result = %+v, want zero value (no Aria route session)", result)
	}

	binding, err := ts.db.OwnerBindings.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if binding.UpstreamUserID != "any-upstream-user" {
		t.Errorf("binding.UpstreamUserID = %q, want any-upstream-user", binding.UpstreamUserID)
	}

	gate, err := ts.db.BootstrapGates.Get(t.Context(), gateID)
	if err != nil {
		t.Fatal(err)
	}
	if gate.Status != domain.BootstrapGateConsumed {
		t.Errorf("gate.Status = %q, want consumed", gate.Status)
	}
}

func TestIssueBootstrapGate_FailsOnceBound(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	beginAndAuthorize(t, ts, "route-1")

	if _, err := ts.IssueBootstrapGate(t.Context()); !errors.Is(err, ErrAlreadyBound) {
		t.Errorf("IssueBootstrapGate() once bound, error = %v, want ErrAlreadyBound", err)
	}
}

func TestStartBootstrapSession_RejectsUnknownGate(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	if _, err := ts.StartBootstrapSession(t.Context(), "does-not-exist"); !errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("StartBootstrapSession() for an unknown gate, error = %v, want ErrBootstrapUnavailable", err)
	}
}

func TestStartBootstrapSession_RejectsExpiredGate(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	gateID, err := ts.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(bootstrapGateTTL + 1)

	if _, err := ts.StartBootstrapSession(t.Context(), gateID); !errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("StartBootstrapSession() for an expired gate, error = %v, want ErrBootstrapUnavailable", err)
	}
}

func TestStartBootstrapSession_RejectsOnceAlreadyBound(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	gateID, err := ts.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	beginAndAuthorize(t, ts, "route-1")

	if _, err := ts.StartBootstrapSession(t.Context(), gateID); !errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("StartBootstrapSession() once bound, error = %v, want ErrBootstrapUnavailable", err)
	}
}

// TestStartBootstrapSession_UpstreamSessionExpiryCappedByGate backs the
// fix for a bootstrap gate expiring before its linked upstream session's
// own TTL would: without the cap, a callback landing in that window
// would pass the upstream session's own not-expired check, run the
// whole HandleUpstreamCallback transaction (creating the Owner actor and
// OwnerBinding), and only then fail at BootstrapGates.Consume's CAS,
// rolling back the just-created binding. With the cap, the upstream
// session itself is already expired by then, so the callback is
// rejected up front (ErrCallbackInvalid) before any write happens.
func TestStartBootstrapSession_UpstreamSessionExpiryCappedByGate(t *testing.T) {
	ts := newTestService(t, Config{IdentityOrigin: testIdentityOrigin, OwnerUsername: "owner"})

	gateID, err := ts.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Wait long enough into the gate's 15-minute TTL that the upstream
	// session's own 10-minute TTL, if left uncapped, would outlive the
	// gate.
	ts.clock.Advance(6 * time.Minute)
	started, err := ts.StartBootstrapSession(t.Context(), gateID)
	if err != nil {
		t.Fatalf("StartBootstrapSession: %v", err)
	}
	upstream, err := ts.db.UpstreamMiAuth.Get(t.Context(), started.UpstreamSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !upstream.ExpiresAt.Equal(ts.clock.Now().Add(9 * time.Minute)) {
		t.Errorf("upstream.ExpiresAt = %v, want capped to the gate's expiry (%v)",
			upstream.ExpiresAt, ts.clock.Now().Add(9*time.Minute))
	}

	// Advance past the gate's (and now the capped session's) expiry, but
	// short of what the session's own uncapped 10-minute TTL would have
	// allowed.
	ts.clock.Advance(9*time.Minute + 30*time.Second)
	ts.provider.check = func(context.Context, string) (string, bool, error) {
		return "any-upstream-user", true, nil
	}

	if _, err := ts.HandleUpstreamCallback(t.Context(), started.UpstreamSessionID, upstream.State); !errors.Is(err, ErrCallbackInvalid) {
		t.Errorf("HandleUpstreamCallback() after gate expiry, error = %v, want ErrCallbackInvalid", err)
	}

	if _, err := ts.db.OwnerBindings.Get(t.Context()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("OwnerBindings.Get() = %v, want ErrNotFound (no partial owner bind left behind)", err)
	}
}

func TestStartBootstrapSession_IsIdempotent(t *testing.T) {
	ts := newTestService(t, defaultTestConfig())
	gateID, err := ts.IssueBootstrapGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	first, err := ts.StartBootstrapSession(t.Context(), gateID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.StartBootstrapSession(t.Context(), gateID)
	if err != nil {
		t.Fatalf("second StartBootstrapSession: %v", err)
	}
	if first != second {
		t.Errorf("retry resumed a different upstream session: %q != %q", first, second)
	}
}
