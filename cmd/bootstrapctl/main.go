// Command bootstrapctl issues a one-time operator bootstrap gate for
// binding this deployment's single owner when ALLOWED_MISSKEY_USER_ID
// is unset (ADR-0001 §2). It is a separate binary from cmd/server,
// deliberately: it exposes no HTTP endpoint of its own, so the gate
// value it prints is reachable only by whoever can already run this
// command (an operator's shell session), never by anyone who can only
// reach the service over HTTP the way Aria's ordinary MiAuth flow does.
// The operator relays the printed URL out-of-band (for example, reading
// it from the SSH session that ran this command) — that is what
// satisfies ADR-0001's "shown only through the operator channel"
// requirement.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.LoadOptions{ConfigFilePath: configFilePath()})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx := context.Background()

	db, err := sqlite.Open(ctx, sqlite.Config{
		Path:         cfg.DB.Path,
		BusyTimeout:  cfg.DB.BusyTimeout,
		MaxOpenConns: cfg.DB.MaxOpenConns,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// Migrations are idempotent and checksum-verified, so it is safe to
	// run this tool before cmd/server has ever started against a fresh
	// database (an operator preparing the very first bind).
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// IssueBootstrapGate never calls the upstream provider (it only
	// creates the gate row), so a nil UpstreamProvider is safe here and
	// documents that this tool never itself talks to IDENTITY_ORIGIN.
	svc := miauth.NewService(db, db.Repos, nil, miauth.Config{IdentityOrigin: cfg.Auth.IdentityOrigin})

	gateID, err := svc.IssueBootstrapGate(ctx)
	if err != nil {
		if errors.Is(err, miauth.ErrAlreadyBound) {
			return fmt.Errorf("this deployment already has an owner bound; bootstrapctl only creates the first binding: %w", err)
		}
		return fmt.Errorf("issue bootstrap gate: %w", err)
	}

	fmt.Println("Bootstrap gate issued. It is single-use and expires in 15 minutes.")
	fmt.Println("Open this URL, as the owner, from the upstream Misskey account to bind:")
	fmt.Println()
	fmt.Printf("  %s/miauth/bootstrap/%s\n", cfg.Auth.LocalOrigin, gateID)
	fmt.Println()
	fmt.Println("Do not share this URL: possessing it is sufficient to bind this deployment's owner.")
	return nil
}

// configFilePath mirrors cmd/server's own helper: an optional
// dotenv-style config file, defaulting to .env in the working
// directory, overridden by CONFIG_FILE. A missing file is not an error
// (config.Load falls back to environment variables and defaults).
func configFilePath() string {
	if v, ok := os.LookupEnv("CONFIG_FILE"); ok && v != "" {
		return v
	}
	return ".env"
}
