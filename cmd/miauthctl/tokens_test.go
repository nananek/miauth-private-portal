package main

import (
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// setupTokensCLI mirrors setupConfigCLI: binds an owner actor (tokens
// reflect-scopes attributes every write to it) and closes the setup
// connection before returning, since run() opens its own connection
// against the same DB_PATH. Returns the DB path (for seeding tokens
// directly between run() calls) and the owner's actor id.
func setupTokensCLI(t *testing.T) (dbPath, ownerID string) {
	t.Helper()
	path, db, _ := setupCLI(t)
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatalf("create owner actor: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, owner.ID
}

// seedReflectScopesToken opens its own short-lived connection to dbPath
// to create a MiAuth session plus an API token referencing it, with
// scopes stored verbatim (not through Check's own effectiveScopes
// computation) so tests can simulate a token issued before
// grantableScopes grew to include everything the session's
// requestedPermissions already asks for. Closes its connection before
// returning, since run() (invoked separately) opens its own.
func seedReflectScopesToken(t *testing.T, dbPath, ownerID, tokenID, routeSessionID, requestedPermissions, scopes string) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	session := domain.LocalMiAuthSession{
		RouteSessionID: routeSessionID, Status: domain.MiAuthConsumed, RequestedPermissions: requestedPermissions,
		LocalActorID: &ownerID, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := db.LocalMiAuth.Create(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	token := domain.APIToken{
		ID: tokenID, TokenHash: "hash-" + tokenID, LocalActorID: ownerID,
		MiAuthLocalSessionID: &routeSessionID, Scopes: scopes, CreatedAt: now,
	}
	if err := db.APITokens.Create(t.Context(), token); err != nil {
		t.Fatal(err)
	}
}

func TestRunTokens_ReflectScopes_SingleToken_HappyPath(t *testing.T) {
	dbPath, ownerID := setupTokensCLI(t)
	seedReflectScopesToken(t, dbPath, ownerID, "tok-1", "route-1", "read:account,write:account", "read:notes read:account")

	out := runOK(t, []string{"tokens", "reflect-scopes", "--token-id", "tok-1"})
	if !strings.Contains(out, "scopes updated") || !strings.Contains(out, "write:account") {
		t.Errorf("reflect-scopes output = %q", out)
	}

	list := runOK(t, []string{"tokens"})
	if !strings.Contains(list, "write:account") {
		t.Errorf("tokens list after reflect-scopes = %q, want write:account present", list)
	}
}

func TestRunTokens_ReflectScopes_All_HappyPath(t *testing.T) {
	dbPath, ownerID := setupTokensCLI(t)
	seedReflectScopesToken(t, dbPath, ownerID, "tok-1", "route-1", "read:account,write:account", "read:notes read:account")
	seedReflectScopesToken(t, dbPath, ownerID, "tok-2", "route-2", "read:account", "read:notes read:account")

	out := runOK(t, []string{"tokens", "reflect-scopes", "--all"})
	if !strings.Contains(out, "Reflected 2 token(s): 1 updated, 1 unchanged, 0 failed.") {
		t.Errorf("reflect-scopes --all output = %q", out)
	}
}

func TestRunTokens_ReflectScopes_DryRunDoesNotWrite(t *testing.T) {
	dbPath, ownerID := setupTokensCLI(t)
	seedReflectScopesToken(t, dbPath, ownerID, "tok-1", "route-1", "read:account,write:account", "read:notes read:account")

	out := runOK(t, []string{"tokens", "reflect-scopes", "--token-id", "tok-1", "--dry-run"})
	if !strings.Contains(out, "would be updated") {
		t.Errorf("dry-run output = %q, want a would-be-updated description", out)
	}

	list := runOK(t, []string{"tokens"})
	if strings.Contains(list, "write:account") {
		t.Errorf("dry-run reflect-scopes actually wrote a scope change: %q", list)
	}
}

func TestRunTokens_ReflectScopes_RequiresExactlyOneSelector(t *testing.T) {
	setupTokensCLI(t)

	code, _, err := runExitCode(t, []string{"tokens", "reflect-scopes"})
	if err == nil {
		t.Fatal("reflect-scopes with neither --token-id nor --all succeeded, want a usage error")
	}
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (exitUsage)", code, exitUsage)
	}

	code, _, err = runExitCode(t, []string{"tokens", "reflect-scopes", "--token-id", "tok-1", "--all"})
	if err == nil {
		t.Fatal("reflect-scopes with both --token-id and --all succeeded, want a usage error")
	}
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (exitUsage)", code, exitUsage)
	}
}

func TestRunTokens_ReflectScopes_UnknownTokenID_ExitCode(t *testing.T) {
	setupTokensCLI(t)
	code, _, err := runExitCode(t, []string{"tokens", "reflect-scopes", "--token-id", "does-not-exist"})
	if err == nil {
		t.Fatal("reflect-scopes on an unknown token id succeeded, want an error")
	}
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (exitNotFound)", code, exitNotFound)
	}
}

func TestRunTokens_ReflectScopes_RevokedToken_ExitCode(t *testing.T) {
	dbPath, ownerID := setupTokensCLI(t)
	seedReflectScopesToken(t, dbPath, ownerID, "tok-1", "route-1", "read:account,write:account", "read:notes read:account")
	runOK(t, []string{"revoke", "tok-1"})

	code, _, err := runExitCode(t, []string{"tokens", "reflect-scopes", "--token-id", "tok-1"})
	if err == nil {
		t.Fatal("reflect-scopes on a revoked token succeeded, want an error")
	}
	if code != exitValidation {
		t.Errorf("exit code = %d, want %d (exitValidation)", code, exitValidation)
	}
}

// TestRunTokens_ReflectScopes_MissingSession_ExitCode is a regression
// guard for the issue's core complaint: when reflect-scopes genuinely
// cannot help (no recoverable originating session), its own output must
// name the "re-issue via fresh login" remediation rather than leaving the
// operator to guess.
func TestRunTokens_ReflectScopes_MissingSession_ExitCode(t *testing.T) {
	dbPath, ownerID := setupTokensCLI(t)
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	token := domain.APIToken{
		ID: "tok-1", TokenHash: "hash-tok-1", LocalActorID: ownerID,
		MiAuthLocalSessionID: nil, Scopes: "read:notes read:account", CreatedAt: time.Now(),
	}
	if err := db.APITokens.Create(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, err := runExitCode(t, []string{"tokens", "reflect-scopes", "--token-id", "tok-1"})
	if err == nil {
		t.Fatal("reflect-scopes on a token with no originating session succeeded, want an error")
	}
	if code != exitValidation {
		t.Errorf("exit code = %d, want %d (exitValidation)", code, exitValidation)
	}
	if !strings.Contains(err.Error(), "fresh Aria login") {
		t.Errorf("error = %q, want it to name the re-login remediation", err.Error())
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty on a hard failure", out)
	}
}

func TestRunTokens_ReflectScopes_NoOwnerBound_ExitCode(t *testing.T) {
	_, db, _ := setupCLI(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	code, _, err := runExitCode(t, []string{"tokens", "reflect-scopes", "--token-id", "tok-1"})
	if err == nil {
		t.Fatal("reflect-scopes with no owner actor bound succeeded, want an error")
	}
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (exitUsage)", code, exitUsage)
	}
}
