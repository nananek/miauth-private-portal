package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func setupCLI(t *testing.T) (string, *sqlite.DB, *miauth.Service) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("LOCAL_ORIGIN", "https://portal.example")
	t.Setenv("DB_PATH", dbPath)
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "missing.env"))
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	service := miauth.NewService(db, db.Repos, miauth.Config{})
	return dbPath, db, service
}

func TestRun_ListAndApproveWithConfirmation(t *testing.T) {
	_, db, service := setupCLI(t)
	if err := service.StartLocalSession(t.Context(), "route-1", "read:account\x1b[31m", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := run([]string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "route-1") || strings.Contains(output.String(), "\x1b") {
		t.Fatalf("unsafe or incomplete list output: %q", output.String())
	}
	output.Reset()
	if err := run([]string{"approve", "route-1"}, strings.NewReader("yes\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Permissions:") || !strings.Contains(output.String(), "Approved") {
		t.Fatalf("approve output = %q", output.String())
	}

	checkDB, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Clean(os.Getenv("DB_PATH")), BusyTimeout: 5 * time.Second, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()
	session, err := checkDB.LocalMiAuth.Get(t.Context(), "route-1")
	if err != nil || session.Status != domain.MiAuthAuthorized {
		t.Fatalf("session = %+v, err = %v", session, err)
	}
}

func TestRun_RejectTokensAndRevoke(t *testing.T) {
	_, db, service := setupCLI(t)
	if err := service.StartLocalSession(t.Context(), "reject-me", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := service.StartLocalSession(t.Context(), "mint", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := service.ApproveSession(t.Context(), "mint"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context(), "mint"); err != nil {
		t.Fatal(err)
	}
	tokens, err := service.ListAPITokens(t.Context())
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %+v, err = %v", tokens, err)
	}
	tokenID := tokens[0].ID
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"reject", "reject-me"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"tokens"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), tokenID) || strings.Contains(output.String(), tokens[0].TokenHash) {
		t.Fatalf("token output = %q", output.String())
	}
	if err := run([]string{"revoke", tokenID}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_ApproveRequiresExplicitConfirmation(t *testing.T) {
	_, db, service := setupCLI(t)
	if err := service.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"approve", "route-1"}, strings.NewReader("no\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v, want cancellation", err)
	}
}

func TestRun_ApproveYesAndArgumentErrors(t *testing.T) {
	_, db, service := setupCLI(t)
	if err := service.StartLocalSession(t.Context(), "route-1", "read:account", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"approve", "--yes", "route-1"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"approve", "--yes", "missing"}, {"reject", "missing"}, {"revoke", "missing"}} {
		if err := run(args, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Errorf("run(%v) succeeded for a missing target", args)
		}
	}
	for _, args := range [][]string{{}, {"unknown"}, {"reject"}, {"revoke"}, {"list", "extra"}} {
		if err := run(args, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Errorf("run(%v) succeeded, want usage error", args)
		}
	}
}

func TestSafeCell(t *testing.T) {
	if got := safeCell("a\tb\nc\x1b[31m"); strings.ContainsAny(got, "\t\n\x1b") {
		t.Fatalf("safeCell returned controls: %q", got)
	}
}
