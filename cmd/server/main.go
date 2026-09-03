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

	reg := health.NewRegistry()

	opts := httpserver.Options{
		Addr:                cfg.HTTP.Addr(),
		ReadTimeout:         cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout:   cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:        cfg.HTTP.WriteTimeout,
		IdleTimeout:         cfg.HTTP.IdleTimeout,
		MaxRequestBodyBytes: cfg.HTTP.MaxRequestBodyBytes,
		ShutdownGracePeriod: cfg.HTTP.ShutdownGracePeriod,
	}

	return httpserver.Run(context.Background(), opts, logger, reg)
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
