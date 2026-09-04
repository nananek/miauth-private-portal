// Command server runs the miauth-private-portal HTTP service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/httpserver"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/llmreply"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/provider/misskey"
	"github.com/nananek/miauth-private-portal/internal/provider/openai"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/timeline"
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
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

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
		TimelineService:     timelineSvc,
		LLMEnabled:          cfg.LLM.Enabled,
	}

	jobsManager := jobs.NewManager(db.Jobs, jobsConfigFrom(cfg.Jobs), logger)

	// Registered only when the feature is on: no Provider (and therefore
	// no request to LLM_BASE_URL) is ever constructed while LLM_ENABLED
	// is false. If it is later turned off after having been on, any
	// already-enqueued "llm_generation" job is left pending rather than
	// dropped — internal/jobs treats an unregistered job type as
	// retryable, the same recovery path a rolling deployment relies on.
	if cfg.LLM.Enabled {
		llmProvider := openai.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model, cfg.LLM.Timeout)
		llmReplySvc := llmreply.NewService(db.Repos, timelineSvc, llmProvider, llmreply.Config{
			ProviderName:    "openai",
			Model:           cfg.LLM.Model,
			MaxOutputTokens: cfg.LLM.MaxOutputTokens,
			ThreadContext: llmreply.ContextBudget{
				MaxMessages: cfg.LLM.ThreadContextMaxMessages,
				MaxChars:    cfg.LLM.ThreadContextMaxChars,
			},
			MaxAttempts: cfg.Jobs.MaxAttempts,
		}, logger)
		jobsManager.Register(llmreply.JobType, llmReplySvc.Handle)
	}

	// The HTTP server and durable worker share one lifecycle. If either
	// component exits unexpectedly, cancel the sibling and wait for its
	// graceful shutdown before closing the shared database.
	serviceCtx, cancelServices := context.WithCancel(ctx)
	defer cancelServices()
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errCh <- httpserver.Run(serviceCtx, opts, logger, reg)
		cancelServices()
	}()
	go func() {
		defer wg.Done()
		errCh <- jobsManager.Run(serviceCtx)
		cancelServices()
	}()
	wg.Wait()
	close(errCh)
	for serviceErr := range errCh {
		if serviceErr != nil {
			return serviceErr
		}
	}
	return nil
}

func jobsConfigFrom(cfg config.JobsConfig) jobs.Config {
	return jobs.Config{
		WorkerID:            cfg.WorkerID,
		PollInterval:        cfg.PollInterval,
		ClaimBatchSize:      cfg.ClaimBatchSize,
		LeaseDuration:       cfg.LeaseDuration,
		LeaseRenewMargin:    cfg.LeaseRenewMargin,
		MaxAttempts:         cfg.MaxAttempts,
		BackoffBase:         cfg.BackoffBase,
		BackoffMax:          cfg.BackoffMax,
		BackoffJitter:       0.2,
		MaxConcurrentJobs:   cfg.MaxConcurrentJobs,
		ShutdownGracePeriod: cfg.ShutdownGracePeriod,
	}
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
