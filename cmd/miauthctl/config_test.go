package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// setupConfigCLI mirrors setupCLI, additionally binding an owner actor
// (config subcommands attribute every write to it — see ownerActorID)
// and closing the setup connection before returning, since run() opens
// its own connection against the same DB_PATH.
func setupConfigCLI(t *testing.T) {
	t.Helper()
	_, db, _ := setupCLI(t)
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}
	if err := db.Actors.Create(t.Context(), domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create owner actor: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func runOK(t *testing.T, args []string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(args, strings.NewReader(""), &out); err != nil {
		t.Fatalf("run(%v) = %v, output=%q", args, err, out.String())
	}
	return out.String()
}

func runExitCode(t *testing.T, args []string) (int, string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, strings.NewReader(""), &out)
	if err == nil {
		return 0, out.String(), nil
	}
	return exitCodeFor(err), out.String(), err
}

func TestRunConfig_SetGetUnset_HappyPath(t *testing.T) {
	setupConfigCLI(t)

	out := runOK(t, []string{"config", "set", "JOBS_MAX_ATTEMPTS", "5"})
	if !strings.Contains(out, "Set JOBS_MAX_ATTEMPTS") {
		t.Errorf("set output = %q", out)
	}

	out = runOK(t, []string{"config", "get", "JOBS_MAX_ATTEMPTS"})
	if !strings.Contains(out, "5") || !strings.Contains(out, "db") {
		t.Errorf("get output = %q, want value 5 and source db", out)
	}

	out = runOK(t, []string{"config", "unset", "JOBS_MAX_ATTEMPTS"})
	if !strings.Contains(out, "Unset JOBS_MAX_ATTEMPTS") {
		t.Errorf("unset output = %q", out)
	}

	out = runOK(t, []string{"config", "get", "JOBS_MAX_ATTEMPTS"})
	if strings.Contains(out, "\tdb\n") {
		t.Errorf("get output after unset still shows db source: %q", out)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("get output after unset = %q, want source=default (JOBS_MAX_ATTEMPTS has no env/file override in this test)", out)
	}
}

func TestRunConfig_Set_RejectsSecretKey(t *testing.T) {
	setupConfigCLI(t)
	code, _, err := runExitCode(t, []string{"config", "set", "LLM_API_KEY", "sk-example"})
	if err == nil {
		t.Fatal("set on a secret key succeeded, want an error")
	}
	if code != exitValidation {
		t.Errorf("exit code = %d, want %d (exitValidation)", code, exitValidation)
	}
}

func TestRunConfig_Set_RejectsBootstrapOnlyKey(t *testing.T) {
	setupConfigCLI(t)
	code, _, err := runExitCode(t, []string{"config", "set", "LOCAL_ORIGIN", "https://evil.example"})
	if err == nil {
		t.Fatal("set on a bootstrap-only key succeeded, want an error")
	}
	if code != exitValidation {
		t.Errorf("exit code = %d, want %d (exitValidation)", code, exitValidation)
	}
}

func TestRunConfig_Set_RejectsUnknownKey(t *testing.T) {
	setupConfigCLI(t)
	code, _, err := runExitCode(t, []string{"config", "set", "NOT_A_REAL_KEY", "1"})
	if err == nil {
		t.Fatal("set on an unknown key succeeded, want an error")
	}
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (exitNotFound)", code, exitNotFound)
	}
}

func TestRunConfig_Set_RejectsInvalidValue(t *testing.T) {
	setupConfigCLI(t)
	code, _, err := runExitCode(t, []string{"config", "set", "JOBS_MAX_ATTEMPTS", "not-an-int"})
	if err == nil {
		t.Fatal("set with an invalid value succeeded, want an error")
	}
	if code != exitValidation {
		t.Errorf("exit code = %d, want %d (exitValidation)", code, exitValidation)
	}
}

func TestRunConfig_Set_DryRunDoesNotWrite(t *testing.T) {
	setupConfigCLI(t)
	out := runOK(t, []string{"config", "set", "--dry-run", "JOBS_MAX_ATTEMPTS", "5"})
	if !strings.Contains(out, "dry run") {
		t.Errorf("dry-run output = %q", out)
	}
	out = runOK(t, []string{"config", "get", "JOBS_MAX_ATTEMPTS"})
	if strings.Contains(out, "\tdb\n") {
		t.Errorf("dry-run set actually wrote a row: %q", out)
	}
}

func TestRunConfig_Get_UnknownKey(t *testing.T) {
	setupConfigCLI(t)
	code, _, err := runExitCode(t, []string{"config", "get", "NOT_A_REAL_KEY"})
	if err == nil {
		t.Fatal("get on an unknown key succeeded, want an error")
	}
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (exitNotFound)", code, exitNotFound)
	}
}

func TestRunConfig_Unset_NotFound(t *testing.T) {
	setupConfigCLI(t)
	code, _, err := runExitCode(t, []string{"config", "unset", "JOBS_MAX_ATTEMPTS"})
	if err == nil {
		t.Fatal("unset on a never-set key succeeded, want an error")
	}
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (exitNotFound)", code, exitNotFound)
	}
}

// TestRunConfig_List_NeverShowsSecretValue backs Issue #76 AC3: a secret
// present in the environment must never appear verbatim in "config
// list" output, only cfg.Redacted()'s <set> marker.
func TestRunConfig_List_NeverShowsSecretValue(t *testing.T) {
	setupConfigCLI(t)
	t.Setenv("LLM_API_KEY", "sk-super-secret-value")

	out := runOK(t, []string{"config", "list"})
	if strings.Contains(out, "sk-super-secret-value") {
		t.Fatalf("config list leaked the secret value: %q", out)
	}
	if !strings.Contains(out, "LLM_API_KEY") || !strings.Contains(out, "<set>") {
		t.Errorf("config list output = %q, want LLM_API_KEY marked <set>", out)
	}
}

func TestRunConfig_HistoryAndRollback(t *testing.T) {
	setupConfigCLI(t)
	runOK(t, []string{"config", "set", "JOBS_MAX_ATTEMPTS", "5"})
	runOK(t, []string{"config", "set", "JOBS_MAX_ATTEMPTS", "6"})

	history := runOK(t, []string{"config", "history", "JOBS_MAX_ATTEMPTS"})
	if !strings.Contains(history, "5") || !strings.Contains(history, "6") {
		t.Fatalf("history output = %q, want both values", history)
	}

	out := runOK(t, []string{"config", "rollback", "--to-version", "1", "JOBS_MAX_ATTEMPTS"})
	if !strings.Contains(out, "Rolled back") {
		t.Errorf("rollback output = %q", out)
	}

	out = runOK(t, []string{"config", "get", "JOBS_MAX_ATTEMPTS"})
	if !strings.Contains(out, "5") {
		t.Errorf("get after rollback = %q, want the version-1 value (5)", out)
	}
}

func TestRunConfig_Rollback_UnknownVersion(t *testing.T) {
	setupConfigCLI(t)
	runOK(t, []string{"config", "set", "JOBS_MAX_ATTEMPTS", "5"})
	code, _, err := runExitCode(t, []string{"config", "rollback", "--to-version", "99", "JOBS_MAX_ATTEMPTS"})
	if err == nil {
		t.Fatal("rollback to a nonexistent version succeeded, want an error")
	}
	if code != exitNotFound {
		t.Errorf("exit code = %d, want %d (exitNotFound)", code, exitNotFound)
	}
}

func TestRunConfig_Import_FromEnvThenIdempotent(t *testing.T) {
	setupConfigCLI(t)
	t.Setenv("JOBS_MAX_ATTEMPTS", "7")

	out := runOK(t, []string{"config", "import", "--from-env"})
	if !strings.Contains(out, "Imported") {
		t.Fatalf("import output = %q", out)
	}

	out = runOK(t, []string{"config", "get", "JOBS_MAX_ATTEMPTS"})
	if !strings.Contains(out, "7") || !strings.Contains(out, "db") {
		t.Errorf("get after import = %q, want value 7 sourced from db", out)
	}

	// Re-running import must not overwrite the now-existing row (§2-4:
	// "既存行は上書きしない").
	runOK(t, []string{"config", "set", "JOBS_MAX_ATTEMPTS", "9"})
	runOK(t, []string{"config", "import", "--from-env"})
	out = runOK(t, []string{"config", "get", "JOBS_MAX_ATTEMPTS"})
	if !strings.Contains(out, "9") {
		t.Errorf("get after re-import = %q, want the operator's own value (9) preserved, not re-seeded from env (7)", out)
	}
}

func TestRunConfig_Export(t *testing.T) {
	setupConfigCLI(t)
	runOK(t, []string{"config", "set", "JOBS_MAX_ATTEMPTS", "5"})
	out := runOK(t, []string{"config", "export"})
	if out != "JOBS_MAX_ATTEMPTS=5\n" {
		t.Errorf("export output = %q", out)
	}
}

func TestRunConfig_ValidateKeyValueAndFile(t *testing.T) {
	setupConfigCLI(t)
	out := runOK(t, []string{"config", "validate", "JOBS_MAX_ATTEMPTS", "5"})
	if !strings.Contains(out, "OK") {
		t.Errorf("validate output = %q", out)
	}
	code, _, err := runExitCode(t, []string{"config", "validate", "JOBS_MAX_ATTEMPTS", "not-an-int"})
	if err == nil {
		t.Fatal("validate of an invalid value succeeded, want an error")
	}
	if code != exitValidation {
		t.Errorf("exit code = %d, want %d (exitValidation)", code, exitValidation)
	}
}

func TestExitCodeFor_NonCLIErrorDefaultsToUsage(t *testing.T) {
	if got := exitCodeFor(errors.New("plain error")); got != exitUsage {
		t.Errorf("exitCodeFor(plain error) = %d, want %d", got, exitUsage)
	}
	if got := exitCodeFor(&cliExitError{code: exitConflict, err: domain.ErrConflict}); got != exitConflict {
		t.Errorf("exitCodeFor(cliExitError conflict) = %d, want %d", got, exitConflict)
	}
}
