// Command server runs the miauth-private-portal HTTP service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
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
	"github.com/nananek/miauth-private-portal/internal/ingest/imap"
	"github.com/nananek/miauth-private-portal/internal/ingest/rss"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/llmclassify"
	"github.com/nananek/miauth-private-portal/internal/llmreply"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
	"github.com/nananek/miauth-private-portal/internal/provider/openai"
	owuiprovider "github.com/nananek/miauth-private-portal/internal/provider/openwebui"
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
	if err := miauthSvc.BackfillOwnerDisplayName(ctx); err != nil {
		return fmt.Errorf("backfill owner display name: %w", err)
	}
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{OwnerUsername: cfg.Auth.OwnerUsername})

	// jobsManager is built here, ahead of the feature blocks below (rather
	// than just before Run, as it was before Issue #53), so the Open
	// WebUI turn job can register on it in the same block that decides
	// whether to build one at all. Registration itself is side-effect
	// free until jobsManager.Run is called at the very end, so moving its
	// construction earlier changes nothing about the other registrations
	// that follow it.
	jobsManager := jobs.NewManager(db.Jobs, jobsConfigFrom(cfg.Jobs), logger)

	// Constructed and seeded only when the feature is on: no
	// openwebui_workspaces/openwebui_models row is ever written, and
	// httpserver's VirtualActors resolver stays nil (its safe default),
	// while OPENWEBUI_ENABLED is false. openwebui.SecretRefAPIKey is
	// duplicated from config.KeyOpenWebUIAPIKey rather than imported
	// (internal/openwebui depends only on internal/domain), so passing
	// the config constant here is what keeps the two checked against
	// each other at every startup instead of silently drifting apart.
	var virtualActors httpserver.VirtualActorResolver
	var openWebUIBridge timeline.EntryHook
	var openWebUICatalogScheduler *openwebui.CatalogScheduler
	if cfg.OpenWebUI.Enabled {
		registry := openwebui.NewRegistry(db, db.Repos, openwebui.RegistryConfig{
			Enabled:           cfg.OpenWebUI.Enabled,
			BaseURL:           cfg.OpenWebUI.BaseURL,
			SecretRef:         config.KeyOpenWebUIAPIKey,
			WorkspaceName:     cfg.OpenWebUI.WorkspaceName,
			PresentationHost:  cfg.OpenWebUI.PresentationHost,
			DefaultModelID:    cfg.OpenWebUI.DefaultModelID,
			OwnerUsername:     cfg.Auth.OwnerUsername,
			GenerationEnabled: cfg.OpenWebUI.GenerationEnabled,
		}, nil, nil, nil)
		if err := registry.Seed(ctx); err != nil {
			return fmt.Errorf("seed openwebui registry: %w", err)
		}
		virtualActors = registry

		// catalogClient is built and used regardless of
		// OPENWEBUI_GENERATION_ENABLED: catalog sync (Issue #75) keeps the
		// VirtualActor projection and search results in step with every
		// model the configured account can see, which is useful on a
		// deployment that never turns outbound generation on at all.
		catalogClient, err := owuiprovider.NewClient(owuiprovider.Config{
			BaseURL:          cfg.OpenWebUI.BaseURL,
			AllowedOrigins:   cfg.OpenWebUI.AllowedOrigins,
			APIKey:           cfg.OpenWebUI.APIKey,
			Timeout:          cfg.OpenWebUI.Timeout,
			MaxResponseBytes: cfg.OpenWebUI.MaxResponseBytes,
			MaxRequestBytes:  cfg.OpenWebUI.MaxRequestBytes,
		})
		if err != nil {
			return fmt.Errorf("build openwebui catalog client: %w", err)
		}

		// toolCache holds every active model's resolved tool_ids (Issue
		// #75 PR5): Registry.SyncCatalog (below, and every later job/
		// startup round) is its only writer, and TurnJob its reader, once
		// generation is on. It replaces Issue #74's single deployment-wide
		// OPENWEBUI_TOOL_IDS override, per owner decision — every model's
		// tool_ids now come from its own info.meta.toolIds alone.
		toolCache := openwebui.NewToolConfigCache()

		// A bounded, best-effort attempt at boot: a target that is merely
		// unreachable at startup must not prevent the rest of the server
		// from starting. Registry.Seed's own fallback model (and an empty
		// toolCache, resolving to no tool_ids) is what this deployment
		// keeps using until the periodic scheduler's next successful
		// round.
		startupSyncCtx, cancelStartupSync := context.WithTimeout(ctx, cfg.OpenWebUI.Timeout)
		if _, err := registry.SyncCatalog(startupSyncCtx, catalogClient, toolCache, logger); err != nil {
			logger.Error("openwebui: startup catalog sync failed; continuing with the last known registry", "error", err)
		}
		cancelStartupSync()

		jobsManager.Register(openwebui.JobTypeCatalogSync, openwebui.NewCatalogSyncJob(registry, catalogClient, toolCache, logger).Handle)
		openWebUICatalogScheduler = openwebui.NewCatalogScheduler(db.Jobs, openwebui.CatalogSchedulerConfig{
			Interval: cfg.OpenWebUI.CatalogSyncInterval,
		}, logger)

		// Registered only when generation itself is on: no
		// internal/provider/openwebui.Client is ever built (and so no
		// request to OPENWEBUI_BASE_URL is ever possible) while
		// OPENWEBUI_GENERATION_ENABLED is false, mirroring LLM_ENABLED's
		// gate below. If generation is later turned off after having been
		// on, any already-enqueued "openwebui_turn" job is left pending
		// rather than dropped — the same unregistered-job-type recovery
		// path LLM's gate relies on.
		if cfg.OpenWebUI.GenerationEnabled {
			owuiProvider, err := owuiprovider.NewClient(owuiprovider.Config{
				BaseURL:          cfg.OpenWebUI.BaseURL,
				AllowedOrigins:   cfg.OpenWebUI.AllowedOrigins,
				APIKey:           cfg.OpenWebUI.APIKey,
				Timeout:          cfg.OpenWebUI.Timeout,
				MaxResponseBytes: cfg.OpenWebUI.MaxResponseBytes,
				MaxRequestBytes:  cfg.OpenWebUI.MaxRequestBytes,
				WebSearchEnabled: cfg.OpenWebUI.WebSearchEnabled,
			})
			if err != nil {
				return fmt.Errorf("build openwebui provider client: %w", err)
			}
			bridge := openwebui.NewBridge(openwebui.BridgeConfig{
				MaxContextMessages: cfg.OpenWebUI.MaxContextMessages,
			}, nil, logger)
			turnJob := openwebui.NewTurnJob(db.Repos, timelineSvc, owuiProvider, toolCache, openwebui.TurnJobConfig{
				MaxAttempts:        cfg.Jobs.MaxAttempts,
				MaxContextMessages: cfg.OpenWebUI.MaxContextMessages,
			}, nil, logger)
			openWebUIBridge = bridge.EnqueueTurn
			jobsManager.Register(openwebui.JobType, turnJob.Handle)
		}
	}

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
		VirtualActors:            virtualActors,
		OpenWebUIBridge:          openWebUIBridge,
	}

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

	// ingestSvc is shared by every internal/ingest.Adapter kind (RSS,
	// IMAP, ...): internal/ingest.Service.Handle dispatches to the right
	// one by the claimed job's source.Kind, and jobs.Manager.Register
	// overwrites any earlier registration for the same job type, so
	// registering ingest.JobType more than once (one Service per kind)
	// would silently make only the last-registered kind's adapters
	// reachable. Constructed only when at least one ingestion feature is
	// enabled, so a deployment with both disabled never builds one at all.
	var ingestSvc *ingest.Service
	if cfg.RSS.Enabled || cfg.IMAP.Enabled {
		ingestSvc = ingest.NewService(db.Repos, timelineSvc, logger)
		jobsManager.Register(ingest.JobType, ingestSvc.Handle)
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
		ingestSvc.RegisterAdapter(rssAdapter)

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

	// Registered, seeded, and scheduled only when the feature is on: no
	// cmd/mailfetch socket is ever dialed while IMAP_ENABLED is false, and
	// no domain.ExternalSource row is ever created either. Issue #12
	// supports exactly one configured mailbox (unlike RSS's list of feed
	// URLs), so exactly one source is seeded here; its URI exists only as
	// that source's (kind, uri) database identity (see
	// internal/ingest/imap.Config's doc comment) and is never
	// re-interpreted by the adapter.
	var imapScheduler *ingest.Scheduler
	if cfg.IMAP.Enabled {
		imapAdapter := imap.NewAdapter(imap.Config{
			Host:             cfg.IMAP.Host,
			Port:             cfg.IMAP.Port,
			TLSMode:          cfg.IMAP.TLSMode,
			Username:         cfg.IMAP.Username,
			Password:         cfg.IMAP.Password,
			Mailbox:          cfg.IMAP.Mailbox,
			SocketPath:       cfg.IMAP.MailfetchSocket,
			FetchTimeout:     cfg.IMAP.FetchTimeout,
			MaxMessageBytes:  cfg.IMAP.MaxMessageBytes,
			SnippetMaxChars:  cfg.IMAP.SnippetMaxChars,
			StoreFullBody:    cfg.IMAP.StoreFullBody,
			FullBodyMaxChars: cfg.IMAP.FullBodyMaxChars,
		})
		ingestSvc.RegisterAdapter(imapAdapter)

		imapURI := (&url.URL{Scheme: "imap", Host: fmt.Sprintf("%s:%d", cfg.IMAP.Host, cfg.IMAP.Port), Path: "/" + cfg.IMAP.Mailbox}).String()
		imapSource := domain.ExternalSource{ID: domain.NewID(), Kind: imap.Kind, URI: imapURI, CreatedAt: time.Now().UTC()}
		if err := db.ExternalSources.EnsureFromConfig(ctx, []domain.ExternalSource{imapSource}); err != nil {
			return fmt.Errorf("seed imap source: %w", err)
		}

		imapScheduler = ingest.NewScheduler(db.ExternalSources, db.Jobs, ingest.SchedulerConfig{
			Kind:         imap.Kind,
			PollInterval: cfg.IMAP.PollInterval,
		}, logger)
	}

	// The HTTP server, durable worker, and (when enabled) ingestion
	// schedulers share one lifecycle. If any component exits unexpectedly,
	// cancel the others and wait for graceful shutdown before closing the
	// shared database.
	serviceCtx, cancelServices := context.WithCancel(ctx)
	defer cancelServices()
	var wg sync.WaitGroup
	errCh := make(chan error, 5)
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
	if imapScheduler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- imapScheduler.Run(serviceCtx)
			cancelServices()
		}()
	}
	if openWebUICatalogScheduler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- openWebUICatalogScheduler.Run(serviceCtx)
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
