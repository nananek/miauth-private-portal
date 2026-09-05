package integration

import (
	"testing"
	"time"
)

// TestServerE2E_MigrateReadyRestartShutdown is Issue #13 AC1's automated
// evidence: a clean-environment deploy of the real cmd/server binary
// (fresh SQLite database, no pre-existing .env) must migrate, become
// ready, and shut down gracefully on SIGTERM — and the same binary must
// come back up cleanly against the database it already migrated, the
// restart path an actual deployment relies on just as much as first
// boot. internal/httpserver's own run_test.go already covers Run's
// shutdown behavior in detail in-process; what was missing was proof
// that cmd/server's own config/migration/actor-seeding/job-manager
// wiring around Run actually works end to end as a real process.
func TestServerE2E_MigrateReadyRestartShutdown(t *testing.T) {
	serverBin := buildBinary(t, "./cmd/server", "server")

	first := startServer(t, serverBin, nil)
	first.waitForReady(t, 10*time.Second)
	if err := first.terminateAndWait(t, 5*time.Second); err != nil {
		t.Errorf("first boot: server exited with error after SIGTERM: %v\n%s", err, first.output.String())
	}

	second := startServer(t, serverBin, map[string]string{"DB_PATH": first.dbPath})
	second.waitForReady(t, 10*time.Second)
	if err := second.terminateAndWait(t, 5*time.Second); err != nil {
		t.Errorf("restart: server exited with error after SIGTERM: %v\n%s", err, second.output.String())
	}
}
