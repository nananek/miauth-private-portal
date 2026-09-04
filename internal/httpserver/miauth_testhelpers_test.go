package httpserver

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

const testLocalOrigin = "https://portal.example"

type miauthTestServer struct {
	*Server
	db     *sqlite.DB
	logBuf *bytes.Buffer
}

func newMiAuthTestServer(t *testing.T, cfg miauth.Config) *miauthTestServer {
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
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc := miauth.NewService(db, db.Repos, cfg)
	logBuf := &bytes.Buffer{}
	server := NewServer(logging.New(logBuf, logging.Config{Format: "json", Level: "info"}), health.NewRegistry(), Options{MiAuthService: svc})
	return &miauthTestServer{Server: server, db: db, logBuf: logBuf}
}

func defaultMiAuthTestConfig() miauth.Config {
	return miauth.Config{
		ClientCallbacks: []string{"aria://aria/miauth"}, OwnerUsername: "owner",
	}
}
