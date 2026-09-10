package main

import (
	"strings"
	"testing"
)

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
