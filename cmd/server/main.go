// Command server runs the miauth-private-portal HTTP service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/httpserver"
	"github.com/nananek/miauth-private-portal/internal/logging"
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
	logger.Info("sqlite pragmas applied", "foreign_keys", "on", "journal_mode", "WAL", "busy_timeout", cfg.DB.BusyTimeout)

	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	if err := db.Actors.EnsureReservedActors(ctx); err != nil {
		return fmt.Errorf("seed reserved actors: %w", err)
	}

	reg := health.NewRegistry()
	reg.Register(db.Checker())

	opts := httpserver.Options{
		Addr:                cfg.HTTP.Addr(),
		ReadTimeout:         cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout:   cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:        cfg.HTTP.WriteTimeout,
		IdleTimeout:         cfg.HTTP.IdleTimeout,
		MaxRequestBodyBytes: cfg.HTTP.MaxRequestBodyBytes,
		ShutdownGracePeriod: cfg.HTTP.ShutdownGracePeriod,
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
