package main

import (
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// setupWebLoginCLI mirrors setupConfigCLI, additionally seeding one
// registered WebAdminCredential for the owner it binds, so
// list-credentials/revoke-credential tests have a real row to act on.
func setupWebLoginCLI(t *testing.T) domain.WebAdminCredential {
	t.Helper()
	_, db, _ := setupCLI(t)
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatalf("create owner actor: %v", err)
	}
	cred := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: owner.ID, CredentialID: "cli-test-credential",
		CredentialJSON: `{"id":"cli-test-credential"}`, CreatedAt: time.Now(),
	}
	if err := db.WebAdminCredentials.Create(t.Context(), cred); err != nil {
		t.Fatalf("create web admin credential: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestRunWebLogin_Issue_PrintsSetupURLWithToken(t *testing.T) {
	setupConfigCLI(t)
	out := runOK(t, []string{"web-login", "issue"})
	if !strings.Contains(out, "https://portal.example/admin/setup?token=") {
		t.Fatalf("output = %q, want it to contain the setup URL", out)
	}
}

func TestRunWebLogin_Issue_RejectsWhenNoOwnerBound(t *testing.T) {
	_, db, _ := setupCLI(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	code, _, err := runExitCode(t, []string{"web-login", "issue"})
	if err == nil {
		t.Fatal("issue with no owner actor bound succeeded, want an error")
	}
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (exitUsage)", code, exitUsage)
	}
}

func TestRunWebLogin_UnknownSubcommand_ExitCode(t *testing.T) {
	setupConfigCLI(t)
	for _, args := range [][]string{
		{"web-login"},
		{"web-login", "bogus"},
	} {
		code, _, err := runExitCode(t, args)
		if err == nil {
			t.Fatalf("run(%v) succeeded, want a usage error", args)
		}
		if code != exitUsage {
			t.Errorf("run(%v) exit code = %d, want %d (exitUsage)", args, code, exitUsage)
		}
	}
}

func TestRunWebLogin_ListCredentials_PrintsRegisteredCredentials(t *testing.T) {
	cred := setupWebLoginCLI(t)
	out := runOK(t, []string{"web-login", "list-credentials"})
	if !strings.Contains(out, cred.ID) {
		t.Fatalf("output = %q, want it to contain credential id %q", out, cred.ID)
	}
	if strings.Contains(out, cred.CredentialJSON) {
		t.Fatalf("output = %q, must never dump CredentialJSON", out)
	}
}

func TestRunWebLogin_RevokeCredential_HappyPath_ReportsSessionCount(t *testing.T) {
	cred := setupWebLoginCLI(t)
	out := runOK(t, []string{"web-login", "revoke-credential", cred.ID})
	if !strings.Contains(out, "Revoked credential "+cred.ID) || !strings.Contains(out, "0 active session(s)") {
		t.Fatalf("output = %q", out)
	}

	// A second run against the now-deleted credential must fail, not
	// silently succeed a second time.
	code, _, err := runExitCode(t, []string{"web-login", "revoke-credential", cred.ID})
	if err == nil {
		t.Fatal("second revoke-credential succeeded, want an error")
	}
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (exitNotFound)", code, exitNotFound)
	}
}

func TestRunWebLogin_RevokeCredential_UnknownID_ExitCode(t *testing.T) {
	setupConfigCLI(t)
	code, _, err := runExitCode(t, []string{"web-login", "revoke-credential", "no-such-id"})
	if err == nil {
		t.Fatal("revoke-credential with an unknown id succeeded, want an error")
	}
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (exitNotFound)", code, exitNotFound)
	}
}

func TestRunWebLogin_RevokeCredential_RequiresExactlyOneArg(t *testing.T) {
	setupConfigCLI(t)
	for _, args := range [][]string{
		{"web-login", "revoke-credential"},
		{"web-login", "revoke-credential", "a", "b"},
	} {
		code, _, err := runExitCode(t, args)
		if err == nil {
			t.Fatalf("run(%v) succeeded, want a usage error", args)
		}
		if code != exitUsage {
			t.Errorf("run(%v) exit code = %d, want %d (exitUsage)", args, code, exitUsage)
		}
	}
}
