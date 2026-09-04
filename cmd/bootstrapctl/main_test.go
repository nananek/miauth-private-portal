package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func setTestEnv(t *testing.T, dbPath string) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("LOCAL_ORIGIN", "https://portal.example")
	t.Setenv("IDENTITY_ORIGIN", "https://misskey.example")
	t.Setenv("DB_PATH", dbPath)
}

func TestRun_IssuesGateAndPrintsURL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	setTestEnv(t, dbPath)

	var runErr error
	out := captureStdout(t, func() { runErr = run() })
	if runErr != nil {
		t.Fatalf("run(): %v", runErr)
	}

	if !strings.Contains(out, "https://portal.example/miauth/bootstrap/") {
		t.Errorf("output = %q, expected a bootstrap URL under LOCAL_ORIGIN", out)
	}

	// The printed gate must actually exist and be usable.
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	idx := strings.Index(out, "/miauth/bootstrap/")
	gateID := strings.TrimSpace(out[idx+len("/miauth/bootstrap/"):])
	gateID = strings.SplitN(gateID, "\n", 2)[0]

	gate, err := db.BootstrapGates.Get(t.Context(), gateID)
	if err != nil {
		t.Fatalf("issued gate not found in database: %v", err)
	}
	if gate.Status != domain.BootstrapGateIssued {
		t.Errorf("gate.Status = %q, want issued", gate.Status)
	}
}

func TestRun_RefusesOnceAlreadyBound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	setTestEnv(t, dbPath)

	// Pre-provision a bound deployment directly through the database,
	// the same way a completed MiAuth flow would leave it.
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	if err := db.OwnerBindings.Bind(t.Context(), domain.OwnerBinding{
		LocalActorID: owner.ID, IdentityOrigin: "https://misskey.example", UpstreamUserID: "someone", BoundAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var runErr error
	out := captureStdout(t, func() { runErr = run() })
	if runErr == nil {
		t.Fatal("run() succeeded, want a refusal once a binding already exists")
	}
	if !errors.Is(runErr, miauth.ErrAlreadyBound) {
		t.Errorf("run() error = %v, want to wrap miauth.ErrAlreadyBound", runErr)
	}
	if strings.Contains(out, "/miauth/bootstrap/") {
		t.Errorf("output printed a gate URL despite the refusal: %q", out)
	}
}
