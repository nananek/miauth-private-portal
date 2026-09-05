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
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/httpserver"
	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/ingest/rss"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/llmclassify"
	"github.com/nananek/miauth-private-portal/internal/llmreply"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
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

	miauthSvc := miauth.NewService(db, db.Repos, miauth.Config{
		ClientCallbacks:  cfg.Auth.AriaClientCallbacks,
		OwnerUsername:    cfg.Auth.OwnerUsername,
		OwnerDisplayName: cfg.Auth.OwnerDisplayName,
	})
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	opts := httpserver.Options{
		Addr:                     cfg.HTTP.Addr(),
		ReadTimeout:              cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout:        cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:             cfg.HTTP.WriteTimeout,
		IdleTimeout:              cfg.HTTP.IdleTimeout,
		MaxRequestBodyBytes:      cfg.HTTP.MaxRequestBodyBytes,
		ShutdownGracePeriod:      cfg.HTTP.ShutdownGracePeriod,
		MiAuthService:            miauthSvc,
		LocalOrigin:              cfg.Auth.LocalOrigin,
		TimelineService:          timelineSvc,
		LLMEnabled:               cfg.LLM.Enabled,
		LLMClassificationEnabled: cfg.LLM.ClassificationEnabled,
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

	// Registered independently of cfg.LLM.Enabled: an operator can run
	// classification without reply generation, or vice versa. No
	// Provider is constructed while LLM_CLASSIFICATION_ENABLED is false.
	if cfg.LLM.ClassificationEnabled {
		classifyModel := cfg.LLM.ClassificationModelOrDefault()
		classifyProvider := openai.NewClassificationClient(
			openai.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, classifyModel, cfg.LLM.Timeout),
		)
		llmClassifySvc := llmclassify.NewService(db, db.Repos, classifyProvider, llmclassify.Config{
			ProviderName:    "openai",
			Model:           classifyModel,
			MaxOutputTokens: cfg.LLM.ClassificationMaxOutputTokens,
			ThreadContext: llmclassify.ContextBudget{
				MaxMessages: cfg.LLM.ClassificationThreadContextMaxMessages,
				MaxChars:    cfg.LLM.ClassificationThreadContextMaxChars,
			},
			MaxAttempts: cfg.Jobs.MaxAttempts,
		}, logger)
		jobsManager.Register(llmclassify.JobType, llmClassifySvc.Handle)
	}

	// Registered, seeded, and scheduled only when the feature is on: no
	// safehttp.Client request ever reaches a configured feed URL while
	// RSS_ENABLED is false, and no domain.ExternalSource row is ever
	// created from RSS_FEED_URLS either. If RSS is later disabled after
	// having been on, an already-scheduled "external_source_poll" job is
	// left pending rather than dropped, the same unregistered-job-type
	// recovery path LLM's Enabled gate relies on.
	var rssScheduler *ingest.Scheduler
	if cfg.RSS.Enabled {
		rssAdapter := rss.NewAdapter(safehttp.NewClient(safehttp.Config{
			MaxRedirects:      cfg.RSS.MaxRedirects,
			AllowInsecureHTTP: cfg.RSS.AllowInsecureHTTP,
		}), rss.Config{
			FetchTimeout:     cfg.RSS.FetchTimeout,
			MaxResponseBytes: cfg.RSS.MaxResponseBytes,
			SummaryMaxChars:  cfg.RSS.SummaryMaxChars,
		})
		ingestSvc := ingest.NewService(db.Repos, timelineSvc, logger)
		ingestSvc.RegisterAdapter(rssAdapter)
		jobsManager.Register(ingest.JobType, ingestSvc.Handle)

		seedNow := time.Now().UTC()
		rssSources := make([]domain.ExternalSource, len(cfg.RSS.FeedURLs))
		for i, feedURL := range cfg.RSS.FeedURLs {
			rssSources[i] = domain.ExternalSource{ID: domain.NewID(), Kind: rss.Kind, URI: feedURL, CreatedAt: seedNow}
		}
		if err := db.ExternalSources.EnsureFromConfig(ctx, rssSources); err != nil {
			return fmt.Errorf("seed rss sources: %w", err)
		}

		rssScheduler = ingest.NewScheduler(db.ExternalSources, db.Jobs, ingest.SchedulerConfig{
			Kind:         rss.Kind,
			PollInterval: cfg.RSS.PollInterval,
		}, logger)
	}

	// The HTTP server, durable worker, and (when enabled) ingestion
	// scheduler share one lifecycle. If any component exits unexpectedly,
	// cancel the others and wait for graceful shutdown before closing the
	// shared database.
	serviceCtx, cancelServices := context.WithCancel(ctx)
	defer cancelServices()
	var wg sync.WaitGroup
	errCh := make(chan error, 3)
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
	if rssScheduler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- rssScheduler.Run(serviceCtx)
			cancelServices()
		}()
	}
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
