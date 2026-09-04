package httpserver

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// fakeProvider is a func-backed miauth.UpstreamProvider fake so HTTP
// handler tests can script upstream verification without a real
// network call (AGENTS.md requires normal tests not depend on real
// credentials or network access).
type fakeProvider struct {
	check func(ctx context.Context, upstreamSessionID string) (string, bool, error)
}

func (f *fakeProvider) Check(ctx context.Context, upstreamSessionID string) (string, bool, error) {
	return f.check(ctx, upstreamSessionID)
}

func fixedProvider(userID string, ok bool, err error) *fakeProvider {
	return &fakeProvider{check: func(context.Context, string) (string, bool, error) {
		return userID, ok, err
	}}
}

const (
	testLocalOrigin    = "https://portal.example"
	testIdentityOrigin = "https://misskey.example"
	testAllowedUserID  = "allowed-upstream-user"
)

// miauthTestServer bundles a real Server (with its MiAuth routes
// registered) backed by a temp SQLite database and a scriptable
// provider fake, plus the captured log buffer for redaction assertions.
type miauthTestServer struct {
	*Server
	db       *sqlite.DB
	provider *fakeProvider
	logBuf   *bytes.Buffer
}

func newMiAuthTestServer(t *testing.T, cfg miauth.Config) *miauthTestServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}

	if cfg.IdentityOrigin == "" {
		cfg.IdentityOrigin = testIdentityOrigin
	}

	provider := fixedProvider("", false, nil)
	svc := miauth.NewService(db, db.Repos, provider, cfg)

	logBuf := &bytes.Buffer{}
	logger := logging.New(logBuf, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	srv := NewServer(logger, reg, Options{
		MiAuthService:  svc,
		LocalOrigin:    testLocalOrigin,
		IdentityOrigin: testIdentityOrigin,
	})

	return &miauthTestServer{Server: srv, db: db, provider: provider, logBuf: logBuf}
}

func defaultMiAuthTestConfig() miauth.Config {
	return miauth.Config{
		IdentityOrigin:       testIdentityOrigin,
		AllowedMisskeyUserID: testAllowedUserID,
		ClientCallbacks:      []string{"aria://aria/miauth"},
		OwnerUsername:        "owner",
	}
}
