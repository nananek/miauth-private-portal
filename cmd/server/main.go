// Command server runs the miauth-private-portal HTTP service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/httpserver"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/provider/misskey"
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

	logger := logging.New(os.Stdout, logging.Config{Level: cfg.Log.Level, Format: cfg.Log.Format})
	slog.SetDefault(logger)
	logger.Info("configuration loaded", "config", cfg.Redacted())

	// Registered here, not left to httpserver.Run, so SIGINT/SIGTERM
	// during the blocking database open/migrate/seed steps below (a slow
	// disk, a lock held by another process, a stuck migration) still gets
	// a clean shutdown instead of requiring a SIGKILL. Run installs its
	// own signal.NotifyContext on top of this ctx for the HTTP serve
	// loop; registering twice is harmless since both fire from the same
	// signal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(ctx, sqlite.Config{
		Path:         cfg.DB.Path,
		BusyTimeout:  cfg.DB.BusyTimeout,
		MaxOpenConns: cfg.DB.MaxOpenConns,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	logger.Info("sqlite pragmas applied", "foreign_keys", "on", "journal_mode", "WAL", "busy_timeout", cfg.DB.BusyTimeout)

	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	if err := db.Actors.EnsureReservedActors(ctx); err != nil {
		return fmt.Errorf("seed reserved actors: %w", err)
	}

	reg := health.NewRegistry()
	reg.Register(db.Checker())

	upstream := misskey.NewClient(cfg.Auth.IdentityOrigin, cfg.Auth.UpstreamHTTPTimeout)
	miauthSvc := miauth.NewService(db, db.Repos, upstream, miauth.Config{
		IdentityOrigin:       cfg.Auth.IdentityOrigin,
		AllowedMisskeyUserID: cfg.Auth.AllowedMisskeyUserID,
		ClientCallbacks:      cfg.Auth.AriaClientCallbacks,
		OwnerUsername:        cfg.Auth.OwnerUsername,
		OwnerDisplayName:     cfg.Auth.OwnerDisplayName,
	})

	opts := httpserver.Options{
		Addr:                cfg.HTTP.Addr(),
		ReadTimeout:         cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout:   cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:        cfg.HTTP.WriteTimeout,
		IdleTimeout:         cfg.HTTP.IdleTimeout,
		MaxRequestBodyBytes: cfg.HTTP.MaxRequestBodyBytes,
		ShutdownGracePeriod: cfg.HTTP.ShutdownGracePeriod,
		MiAuthService:       miauthSvc,
		LocalOrigin:         cfg.Auth.LocalOrigin,
		IdentityOrigin:      cfg.Auth.IdentityOrigin,
	}

	return httpserver.Run(ctx, opts, logger, reg)
}

// configFilePath returns the dotenv-style config file to load, defaulting
// to .env in the working directory. CONFIG_FILE overrides it; a missing
// file at either path is not an error (config.Load falls back to
// environment variables and defaults).
func configFilePath() string {
	if v, ok := os.LookupEnv("CONFIG_FILE"); ok && v != "" {
		return v
	}
	return ".env"
}
