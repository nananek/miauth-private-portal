// Command server runs the miauth-private-portal HTTP service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/configstore"
	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/drive"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/httpserver"
	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/ingest/favicon"
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
	"github.com/nananek/miauth-private-portal/internal/streamhub"
	"github.com/nananek/miauth-private-portal/internal/timeline"
	"github.com/nananek/miauth-private-portal/internal/userlist"
	"github.com/nananek/miauth-private-portal/internal/webadmin"
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

	// Issue #76 (ADR-0006 §2-4): idempotent migration of every db-eligible
	// key's current effective value into app_config, so miauthctl config
	// has something to list/get from the very first boot rather than only
	// after an operator's first explicit set. Attributed to the System
	// actor (not an owner, who may not be bound yet at this point in
	// startup) since this runs automatically, not on an operator's
	// command — see internal/configstore.Seed's own doc comment for why
	// an existing row is never touched here.
	systemActor, err := db.Actors.GetByType(ctx, domain.ActorSystem)
	if err != nil {
		return fmt.Errorf("resolve system actor: %w", err)
	}
	seeded, err := configstore.Seed(ctx, db, cfg, systemActor.ID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("seed app config: %w", err)
	}
	if seeded > 0 {
		logger.Info("app config seeded", "keys_seeded", seeded)
	}

	// configStore is Issue #76 PR4a/4b/4c's shared read path into the DB
	// configuration overlay (ADR-0006): every reload-capable component
	// below is given a closure reading through it, falling back to its
	// own bootstrap cfg value, rather than a direct reference — so each
	// stays ignorant of internal/configstore/internal/config entirely
	// and is exercised in tests the same way it always was, by
	// constructing it with plain values.
	configStore := configstore.New(db.Config, logger)

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
	// webAdminSvc backs Issue #136 Phase 1's admin bootstrap/registration
	// routes (ADR-0010). Built unconditionally, like miauthSvc above, with
	// one exception: WebAuthn's RPID must be a real domain, not an IP
	// address (an inherent protocol constraint, not a bug — see
	// webadmin.NewService's own error). LOCAL_ORIGIN pointed at a bare IP
	// (this codebase's own dev/E2E-test convention, and some local
	// deployments) can never satisfy that, so this treats a construction
	// failure as "the admin Web UI is unavailable on this deployment" —
	// the same nil-means-disabled convention virtualActors/
	// openWebUIBridge below use — rather than refusing to start the rest
	// of the server over it.
	var webAdminSvc *webadmin.Service
	if webAdminRPID, err := url.Parse(cfg.Auth.LocalOrigin); err != nil {
		// Unreachable in practice: internal/config.Validate already
		// enforces LOCAL_ORIGIN is a well-formed origin at startup.
		return fmt.Errorf("parse LOCAL_ORIGIN: %w", err)
	} else if svc, err := webadmin.NewService(db, db.Repos, webadmin.Config{
		RPID: webAdminRPID.Hostname(), RPDisplayName: "miauth-private-portal", RPOrigins: []string{cfg.Auth.LocalOrigin},
		OwnerUsername: cfg.Auth.OwnerUsername, OwnerDisplayName: cfg.Auth.OwnerDisplayName,
		SessionCookie: webadmin.SessionCookieConfig{
			SessionTTL: cfg.WebAdmin.SessionTTL,
			ReloadSessionTTL: func(ctx context.Context) time.Duration {
				return configStore.Duration(ctx, config.KeyAdminSessionTTL, cfg.WebAdmin.SessionTTL)
			},
		},
	}); err != nil {
		logger.Warn("admin web UI bootstrap unavailable: webadmin service init failed", "error", err.Error())
	} else {
		webAdminSvc = svc
	}
	// streamHub is built before timelineSvc/httpserver.Options and given
	// to both (timeline.Config.Broadcaster below, httpserver.Options.
	// StreamHub further down): internal/streamhub depends on neither, so
	// this ordering avoids the construction-order cycle a direct
	// timeline<->httpserver dependency would otherwise need (Issue #95
	// PR2, docs/compat/aria-v1.5.11.md's push event section).
	streamHub := streamhub.NewHub()
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{
		OwnerUsername: cfg.Auth.OwnerUsername,
		Broadcaster:   streamHub,
	})
	// userlistSvc backs Issue #115's users/lists/* CRUD and
	// notes/user-list-timeline. Built unconditionally, like timelineSvc
	// above: there is no feature flag gating it off.
	userlistSvc := userlist.NewService(db.Repos, userlist.Config{})

	// driveSvc backs Issue #77 PR3's Misskey-compatible Drive API. Built
	// unconditionally, like internal/drive.Local/S3 in PR1: DriveConfig
	// has no Enabled flag (internal/config.DriveConfig's own doc
	// comment), so there is no "off" state to skip this behind.
	var driveStorage drive.Storage
	switch cfg.Drive.Backend {
	case "s3compat":
		driveStorage, err = drive.NewS3(drive.S3Config{
			Endpoint:        cfg.Drive.S3Endpoint,
			Bucket:          cfg.Drive.S3Bucket,
			AccessKeyID:     cfg.Drive.S3AccessKeyID,
			SecretAccessKey: cfg.Drive.S3SecretAccessKey,
			UseSSL:          cfg.Drive.S3UseSSL,
			Region:          cfg.Drive.S3Region,
		})
		if err != nil {
			return fmt.Errorf("build drive S3 client: %w", err)
		}
	default:
		driveStorage = drive.NewLocal(cfg.Drive.DataDir)
	}
	driveSvc := drive.NewService(
		driveStorage,
		// upload-from-url's own SSRF-protected fetcher (AGENTS.md:
		// "External fetchers require ... SSRF protections"), separate
		// from RSS's client below: fixed, conservative redirect/scheme
		// policy rather than a configurable one — no issue or plan
		// section asked for per-feature-tunable fetch policy here, only
		// for the fetch itself to be safe.
		safehttp.NewClient(safehttp.Config{MaxRedirects: 3, AllowInsecureHTTP: false}),
		db.Repos,
		drive.Config{
			MaxFileBytes:   cfg.Drive.MaxFileBytes,
			MaxImageWidth:  cfg.Drive.MaxImageWidth,
			MaxImageHeight: cfg.Drive.MaxImageHeight,
			// CapacityBytes is POST /api/drive's advertised capacity —
			// cosmetic only, see drive.Config.CapacityBytes's doc
			// comment; this deployment enforces no real quota beyond
			// MaxFileBytes per upload, so no configuration key exists
			// for it.
			CapacityBytes: driveCapacityBytes,
		},
	)

	// jobsManager is built here, ahead of the feature blocks below (rather
	// than just before Run, as it was before Issue #53), so the Open
	// WebUI turn job can register on it in the same block that decides
	// whether to build one at all. Registration itself is side-effect
	// free until jobsManager.Run is called at the very end, so moving its
	// construction earlier changes nothing about the other registrations
	// that follow it.
	jobsCfg := jobsConfigFrom(cfg.Jobs)
	jobsCfg.Reload = func(ctx context.Context) jobs.Config {
		return jobsConfigFrom(config.JobsConfig{
			WorkerID:            cfg.Jobs.WorkerID,
			PollInterval:        configStore.Duration(ctx, config.KeyJobsPollInterval, cfg.Jobs.PollInterval),
			ClaimBatchSize:      configStore.Int(ctx, config.KeyJobsClaimBatchSize, cfg.Jobs.ClaimBatchSize),
			LeaseDuration:       configStore.Duration(ctx, config.KeyJobsLeaseDuration, cfg.Jobs.LeaseDuration),
			LeaseRenewMargin:    configStore.Duration(ctx, config.KeyJobsLeaseRenewMargin, cfg.Jobs.LeaseRenewMargin),
			MaxAttempts:         configStore.Int(ctx, config.KeyJobsMaxAttempts, cfg.Jobs.MaxAttempts),
			BackoffBase:         configStore.Duration(ctx, config.KeyJobsBackoffBase, cfg.Jobs.BackoffBase),
			BackoffMax:          configStore.Duration(ctx, config.KeyJobsBackoffMax, cfg.Jobs.BackoffMax),
			MaxConcurrentJobs:   configStore.Int(ctx, config.KeyJobsMaxConcurrent, cfg.Jobs.MaxConcurrentJobs),
			ShutdownGracePeriod: configStore.Duration(ctx, config.KeyJobsShutdownGrace, cfg.Jobs.ShutdownGracePeriod),
		})
	}
	jobsManager := jobs.NewManager(db.Jobs, jobsCfg, logger)

	// Registered and scheduled unconditionally, like driveSvc itself
	// above (Drive has no "off" state): Issue #77 PR7's orphan-file GC
	// sweep reconciles the configured Storage backend against every
	// files.storage_key on a fixed interval, regardless of which backend
	// or how many files this deployment currently has.
	jobsManager.Register(drive.JobTypeOrphanGC, drive.NewOrphanGCJob(driveSvc, logger).Handle)
	driveGCScheduler := drive.NewGCScheduler(db.Jobs, drive.GCSchedulerConfig{
		Interval: cfg.Drive.OrphanGCInterval,
	}, logger)

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
	// openWebUITurnLinks backs Issues #81/#84's wire-projection
	// enrichment (httpserver.Options.OpenWebUITurnLinks); nil (the
	// default, OPENWEBUI_ENABLED off) leaves every reply projected
	// exactly as before these issues existed, the same nil-means-
	// unchanged convention virtualActors/openWebUIBridge already use.
	var openWebUITurnLinks domain.OpenWebUITurnLinkRepository
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
		openWebUITurnLinks = db.Repos.OpenWebUITurnLinks

		// catalogClient is built and used regardless of
		// OPENWEBUI_GENERATION_ENABLED: catalog sync (Issue #75) keeps the
		// VirtualActor projection and search results in step with every
		// model the configured account can see, which is useful on a
		// deployment that never turns outbound generation on at all.
		catalogClient, err := owuiprovider.NewClient(owuiprovider.ConfigFrom(cfg.OpenWebUI))
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
		// featureCache holds every active model's own "defaults to
		// web_search" flag (Issue #75 AC#11, ADR-0005 D21), the same
		// lifecycle as toolCache — read by TurnJob only when
		// OPENWEBUI_WEB_SEARCH_ENABLED is left unset.
		featureCache := openwebui.NewFeatureDefaultCache()

		// A bounded, best-effort attempt at boot: a target that is merely
		// unreachable at startup must not prevent the rest of the server
		// from starting. Registry.Seed's own fallback model (and an empty
		// toolCache, resolving to no tool_ids) is what this deployment
		// keeps using until the periodic scheduler's next successful
		// round.
		startupSyncCtx, cancelStartupSync := context.WithTimeout(ctx, cfg.OpenWebUI.Timeout)
		if _, err := registry.SyncCatalog(startupSyncCtx, catalogClient, toolCache, featureCache, logger); err != nil {
			logger.Error("openwebui: startup catalog sync failed; continuing with the last known registry", "error", err)
		}
		cancelStartupSync()

		jobsManager.Register(openwebui.JobTypeCatalogSync, openwebui.NewCatalogSyncJob(registry, catalogClient, toolCache, featureCache, logger).Handle)
		openWebUICatalogScheduler = openwebui.NewCatalogScheduler(db.Jobs, openwebui.CatalogSchedulerConfig{
			Interval: cfg.OpenWebUI.CatalogSyncInterval,
			ReloadInterval: func(ctx context.Context) time.Duration {
				return configStore.Duration(ctx, config.KeyOpenWebUICatalogSyncInterval, cfg.OpenWebUI.CatalogSyncInterval)
			},
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
			owuiProvider, err := owuiprovider.NewClient(owuiprovider.ConfigFrom(cfg.OpenWebUI))
			if err != nil {
				return fmt.Errorf("build openwebui provider client: %w", err)
			}
			bridge := openwebui.NewBridge(openwebui.BridgeConfig{
				MaxContextMessages: cfg.OpenWebUI.MaxContextMessages,
			}, nil, logger)
			turnJob := openwebui.NewTurnJob(db.Repos, timelineSvc, owuiProvider, toolCache, featureCache, openwebui.TurnJobConfig{
				MaxAttempts:        cfg.Jobs.MaxAttempts,
				MaxContextMessages: cfg.OpenWebUI.MaxContextMessages,
				WebSearchOverride:  cfg.OpenWebUI.WebSearchEnabled,
				ReloadWebSearchOverride: func(ctx context.Context) *bool {
					return configStore.BoolPtr(ctx, config.KeyOpenWebUIWebSearchEnabled, cfg.OpenWebUI.WebSearchEnabled)
				},
				ViewerBaseURL: cfg.OpenWebUI.ViewerBaseURL,
			}, nil, logger)
			openWebUIBridge = bridge.EnqueueTurn
			jobsManager.Register(openwebui.JobType, turnJob.Handle)
		}
	}

	opts := httpserver.Options{
		Addr:              cfg.HTTP.Addr(),
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		// max(HTTP_MAX_REQUEST_BODY_BYTES, DRIVE_MAX_FILE_BYTES +
		// overhead): the global withMaxBody wrap around the whole mux
		// (internal/httpserver/run.go) shares one ceiling across every
		// route, so drive/files/create's multipart upload needs the
		// outer cap to be no tighter than what Drive itself allows —
		// HTTP_MAX_REQUEST_BODY_BYTES's own 1 MiB default exists to
		// bound ordinary JSON API bodies, not uploads, and must not
		// silently truncate a within-DRIVE_MAX_FILE_BYTES upload before
		// internal/httpserver/drive_handlers.go's own, more specific
		// size check ever runs.
		MaxRequestBodyBytes:      max(cfg.HTTP.MaxRequestBodyBytes, cfg.Drive.MaxFileBytes+driveMultipartOverheadBytes),
		ShutdownGracePeriod:      cfg.HTTP.ShutdownGracePeriod,
		MiAuthService:            miauthSvc,
		LocalOrigin:              cfg.Auth.LocalOrigin,
		TimelineService:          timelineSvc,
		UserListService:          userlistSvc,
		StreamHub:                streamHub,
		LLMEnabled:               cfg.LLM.Enabled,
		LLMClassificationEnabled: cfg.LLM.ClassificationEnabled,
		VirtualActors:            virtualActors,
		ExternalSources:          db.Repos.ExternalSources,
		OpenWebUIBridge:          openWebUIBridge,
		OpenWebUITurnLinks:       openWebUITurnLinks,
		OpenWebUIViewerBaseURL:   cfg.OpenWebUI.ViewerBaseURL,
		Drive:                    driveSvc,
		DriveMaxFileBytes:        cfg.Drive.MaxFileBytes,
		WebAdmin:                 webAdminSvc,
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
			Timeout:         cfg.LLM.Timeout,
			MaxOutputTokens: cfg.LLM.MaxOutputTokens,
			ThreadContext: llmreply.ContextBudget{
				MaxMessages: cfg.LLM.ThreadContextMaxMessages,
				MaxChars:    cfg.LLM.ThreadContextMaxChars,
			},
			MaxAttempts: cfg.Jobs.MaxAttempts,
			Reload: func(ctx context.Context) llmreply.Config {
				return llmreply.Config{
					ProviderName:    "openai",
					Model:           configStore.String(ctx, config.KeyLLMModel, cfg.LLM.Model),
					Timeout:         configStore.Duration(ctx, config.KeyLLMTimeout, cfg.LLM.Timeout),
					MaxOutputTokens: configStore.Int(ctx, config.KeyLLMMaxOutputTokens, cfg.LLM.MaxOutputTokens),
					ThreadContext: llmreply.ContextBudget{
						MaxMessages: configStore.Int(ctx, config.KeyLLMThreadContextMaxMessages, cfg.LLM.ThreadContextMaxMessages),
						MaxChars:    configStore.Int(ctx, config.KeyLLMThreadContextMaxChars, cfg.LLM.ThreadContextMaxChars),
					},
					MaxAttempts: cfg.Jobs.MaxAttempts,
				}
			},
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
			Timeout:         cfg.LLM.Timeout,
			MaxOutputTokens: cfg.LLM.ClassificationMaxOutputTokens,
			ThreadContext: llmclassify.ContextBudget{
				MaxMessages: cfg.LLM.ClassificationThreadContextMaxMessages,
				MaxChars:    cfg.LLM.ClassificationThreadContextMaxChars,
			},
			MaxAttempts: cfg.Jobs.MaxAttempts,
			Reload: func(ctx context.Context) llmclassify.Config {
				return llmclassify.Config{
					ProviderName:    "openai",
					Model:           configStore.String(ctx, config.KeyLLMClassificationModel, classifyModel),
					Timeout:         configStore.Duration(ctx, config.KeyLLMTimeout, cfg.LLM.Timeout),
					MaxOutputTokens: configStore.Int(ctx, config.KeyLLMClassificationMaxOutputTokens, cfg.LLM.ClassificationMaxOutputTokens),
					ThreadContext: llmclassify.ContextBudget{
						MaxMessages: configStore.Int(ctx, config.KeyLLMClassificationThreadContextMaxMessages, cfg.LLM.ClassificationThreadContextMaxMessages),
						MaxChars:    configStore.Int(ctx, config.KeyLLMClassificationThreadContextMaxChars, cfg.LLM.ClassificationThreadContextMaxChars),
					},
					MaxAttempts: cfg.Jobs.MaxAttempts,
				}
			},
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
		// A bad script fails startup closed, before the server ever
		// binds a port — the same posture every other startup-time
		// config problem in this function already gets (Issue #135).
		var rssFilter *rss.Filter
		if cfg.RSS.FilterScriptPath != "" {
			var err error
			rssFilter, err = rss.LoadFilter(cfg.RSS.FilterScriptPath)
			if err != nil {
				return fmt.Errorf("load rss filter script: %w", err)
			}
		}
		rssAdapter := rss.NewAdapter(safehttp.NewClient(safehttp.Config{
			MaxRedirects:      cfg.RSS.MaxRedirects,
			AllowInsecureHTTP: cfg.RSS.AllowInsecureHTTP,
		}), rss.Config{
			FetchTimeout:     cfg.RSS.FetchTimeout,
			MaxResponseBytes: cfg.RSS.MaxResponseBytes,
			SummaryMaxChars:  cfg.RSS.SummaryMaxChars,
			ReloadSummaryMaxChars: func(ctx context.Context) int {
				return configStore.Int(ctx, config.KeyRSSSummaryMaxChars, cfg.RSS.SummaryMaxChars)
			},
		}, rssFilter, logger)
		ingestSvc.RegisterAdapter(rssAdapter)

		// ReconcileFromConfig (Issue #76 PR4a) first: it creates a bare row
		// for every RSS_FEED_URLS entry not yet registered at all
		// (reactivating one whose URI came back after being removed, and
		// deactivating one no longer configured), but knows nothing about
		// actors — that path must stay pure SQL reconciliation (AGENTS.md's
		// persistence-boundary rule; see external_repository.go's own doc
		// comment). ensureRSSSourceActors (Issue #77 PR4, self-healing as
		// of Issue #134) runs right after: it provisions a paired
		// ActorExternalSource (with its own username/host, ADR-0008) for
		// every rss-kind row — freshly created by the call just above, or
		// left actor-less by a pre-Issue-#134 deployment (this single
		// startup pass doubles as that backfill) — that does not have one
		// yet, without ever recomputing an already-set one.
		seedNow := time.Now().UTC()
		// A separate, fixed-policy client from rssAdapter's own
		// (cfg.RSS.AllowInsecureHTTP-controlled) one above: favicon fetch
		// always requires https, matching driveSvc's upload-from-url
		// client's reasoning — see internal/ingest/favicon.Fetch's doc
		// comment.
		faviconClient := safehttp.NewClient(safehttp.Config{MaxRedirects: 3, AllowInsecureHTTP: false})
		if err := db.ExternalSources.ReconcileFromConfig(ctx, rss.Kind, cfg.RSS.FeedURLs, seedNow); err != nil {
			return fmt.Errorf("reconcile rss sources: %w", err)
		}
		if err := ensureRSSSourceActors(ctx, db, cfg.RSS.FeedURLs, cfg.RSS.FeedUsernames, seedNow, driveSvc, faviconClient, cfg.Drive.MaxFileBytes, logger); err != nil {
			return fmt.Errorf("ensure rss source actors: %w", err)
		}

		rssScheduler = ingest.NewScheduler(db.ExternalSources, db.Jobs, ingest.SchedulerConfig{
			Kind:         rss.Kind,
			PollInterval: cfg.RSS.PollInterval,
			ReloadPollInterval: func(ctx context.Context) time.Duration {
				return configStore.Duration(ctx, config.KeyRSSPollInterval, cfg.RSS.PollInterval)
			},
			DesiredURIs: func(ctx context.Context) []string {
				return configStore.StringList(ctx, config.KeyRSSFeedURLs, cfg.RSS.FeedURLs)
			},
			EnsureActors: func(ctx context.Context) error {
				return ensureRSSSourceActors(ctx, db, cfg.RSS.FeedURLs, cfg.RSS.FeedUsernames, time.Now().UTC(), driveSvc, faviconClient, cfg.Drive.MaxFileBytes, logger)
			},
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
			Reload: func(ctx context.Context) imap.Config {
				return imap.Config{
					Host:             cfg.IMAP.Host,
					Port:             cfg.IMAP.Port,
					TLSMode:          cfg.IMAP.TLSMode,
					Username:         cfg.IMAP.Username,
					Password:         cfg.IMAP.Password,
					Mailbox:          cfg.IMAP.Mailbox,
					SocketPath:       cfg.IMAP.MailfetchSocket,
					FetchTimeout:     configStore.Duration(ctx, config.KeyIMAPFetchTimeout, cfg.IMAP.FetchTimeout),
					MaxMessageBytes:  configStore.Int64(ctx, config.KeyIMAPMaxMessageBytes, cfg.IMAP.MaxMessageBytes),
					SnippetMaxChars:  configStore.Int(ctx, config.KeyIMAPSnippetMaxChars, cfg.IMAP.SnippetMaxChars),
					StoreFullBody:    configStore.Bool(ctx, config.KeyIMAPStoreFullBody, cfg.IMAP.StoreFullBody),
					FullBodyMaxChars: configStore.Int(ctx, config.KeyIMAPFullBodyMaxChars, cfg.IMAP.FullBodyMaxChars),
				}
			},
		})
		ingestSvc.RegisterAdapter(imapAdapter)

		// IMAP_HOST/PORT/MAILBOX stay bootstrap-only (ADR-0006's
		// network-destination carve-out), so imapURI can only ever
		// change via a restart — this one-time reconcile is therefore
		// the only reconciliation IMAP's source ever needs; unlike RSS
		// above, imapScheduler is given no DesiredURIs closure.
		imapURI := (&url.URL{Scheme: "imap", Host: fmt.Sprintf("%s:%d", cfg.IMAP.Host, cfg.IMAP.Port), Path: "/" + cfg.IMAP.Mailbox}).String()
		if err := db.ExternalSources.ReconcileFromConfig(ctx, imap.Kind, []string{imapURI}, time.Now().UTC()); err != nil {
			return fmt.Errorf("seed imap source: %w", err)
		}

		imapScheduler = ingest.NewScheduler(db.ExternalSources, db.Jobs, ingest.SchedulerConfig{
			Kind:         imap.Kind,
			PollInterval: cfg.IMAP.PollInterval,
			ReloadPollInterval: func(ctx context.Context) time.Duration {
				return configStore.Duration(ctx, config.KeyIMAPPollInterval, cfg.IMAP.PollInterval)
			},
		}, logger)
	}

	// The HTTP server, durable worker, and (when enabled) ingestion
	// schedulers share one lifecycle. If any component exits unexpectedly,
	// cancel the others and wait for graceful shutdown before closing the
	// shared database.
	serviceCtx, cancelServices := context.WithCancel(ctx)
	defer cancelServices()
	var wg sync.WaitGroup
	errCh := make(chan error, 6)
	wg.Add(3)
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
	// Unconditional, like driveSvc/driveGCScheduler's own construction
	// above: Drive has no "off" state to gate this behind.
	go func() {
		defer wg.Done()
		errCh <- driveGCScheduler.Run(serviceCtx)
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

// ensureRSSSourceActors idempotently fills in actor_id/username/host
// (Issue #77 PR4, ADR-0008) for every currently *active* rss-kind
// external_sources row that does not have one yet. A row can reach that
// state two ways this function treats identically: (1)
// ReconcileFromConfig's create-if-missing path just created it with no
// paired actor — that path knows nothing about actors at all, by design
// (see external_repository.go's own doc comment) — whether at startup
// or via a live RSS_FEED_URLS reload (Issue #134's bug); or (2) it was
// left in that state by a version of this service that predates this
// fix (Issue #134's backfill case — same code path, no separate
// one-off tool needed). It never recomputes an already-set actor
// identity: only a row with ActorID == nil is touched, and
// SetActorIdentity's own `WHERE actor_id IS NULL` makes that enforced,
// not just intended (ADR-0008: "computed once ... and never
// recomputed").
//
// Callers must run this again after every ReconcileFromConfig round,
// not only once at process startup — cmd/server's startup sequence and
// ingest.Scheduler's tick (via SchedulerConfig.EnsureActors) both do.
//
// bootstrapFeedURLs/bootstrapFeedUsernames — this process's own
// RSS_FEED_URLS at startup, already split into parallel slices by
// internal/config's splitRSSFeedURLs — are consulted only to find an
// owner-chosen username for a row whose URI happens to match one of
// them (the startup call passes cfg.RSS.FeedURLs/FeedUsernames; the
// scheduler-tick call passes nil, nil, since configstore.Store.
// StringList does not carry a "|username" suffix through the DB
// overlay — see that function's own doc comment). Every other
// actor-less row gets an auto-derived username instead, exactly as an
// unset feedUsernames[i] entry always has.
//
// After each row's actor commits, its host's favicon.ico is fetched,
// validated, and stored as that actor's avatar (Issue #77 PR5, folded in
// per plan-77 requirement 2). This step is strictly best-effort — see
// fetchAndSetSourceFavicon's doc comment — and runs outside the actor/
// source transaction: a network fetch has no business holding a database
// write lock, and a favicon failure must never unwind an otherwise
// successful actor provisioning.
func ensureRSSSourceActors(ctx context.Context, db *sqlite.DB, bootstrapFeedURLs []string, bootstrapFeedUsernames []*string, now time.Time, driveSvc *drive.Service, faviconClient *safehttp.Client, faviconMaxBytes int64, logger *slog.Logger) error {
	usernameByURI := make(map[string]string, len(bootstrapFeedURLs))
	for i, u := range bootstrapFeedURLs {
		if i < len(bootstrapFeedUsernames) && bootstrapFeedUsernames[i] != nil {
			usernameByURI[u] = *bootstrapFeedUsernames[i]
		}
	}

	sources, err := db.ExternalSources.List(ctx, rss.Kind)
	if err != nil {
		return fmt.Errorf("list rss sources: %w", err)
	}

	// reservedByHost seeds from every source that already has a
	// username, then grows as this pass provisions more, so two
	// actor-less rows discovered in the very same pass never derive the
	// same candidate for the same host (the DB's own UNIQUE(host,
	// username) index is the final backstop if this bookkeeping is ever
	// wrong, but should never need to be exercised in practice).
	reservedByHost := make(map[string]map[string]bool)
	for _, s := range sources {
		if s.Host != nil && s.Username != nil {
			if reservedByHost[*s.Host] == nil {
				reservedByHost[*s.Host] = make(map[string]bool)
			}
			reservedByHost[*s.Host][*s.Username] = true
		}
	}

	for _, source := range sources {
		if source.ActorID != nil {
			continue // already has its own actor; never recomputed (ADR-0008)
		}

		host, err := rss.HostFromFeedURL(source.URI)
		if err != nil {
			return fmt.Errorf("derive host for rss source %q: %w", source.URI, err)
		}

		username := usernameByURI[source.URI]
		if username == "" {
			reserved := reservedByHost[host]
			username = rss.DefaultUsername(host, source.URI, func(candidate string) bool { return reserved[candidate] })
		}
		if reservedByHost[host] == nil {
			reservedByHost[host] = make(map[string]bool)
		}
		reservedByHost[host][username] = true

		actorID := domain.NewID()
		err = db.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
			if err := repos.Actors.Create(ctx, domain.Actor{ID: actorID, Type: domain.ActorExternalSource, CreatedAt: now}); err != nil {
				return fmt.Errorf("create rss source actor: %w", err)
			}
			return repos.ExternalSources.SetActorIdentity(ctx, source.ID, actorID, username, host)
		})
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				continue // provisioned by a concurrent pass since List above; safe to skip
			}
			return fmt.Errorf("provision rss source actor %q: %w", source.URI, err)
		}

		fetchAndSetSourceFavicon(ctx, db, driveSvc, faviconClient, faviconMaxBytes, actorID, host, username, logger)
	}
	return nil
}

// fetchAndSetSourceFavicon best-effort fetches host's favicon.ico,
// stores it as an owner-less Drive file (domain.FilePurposeSourceFavicon),
// and points actorID's avatar_file_id at it. Every failure — no
// favicon.ico, an unsupported legacy BMP-in-ICO, a favicon exceeding
// faviconMaxBytes, or a Drive validation/storage error — is logged and
// swallowed: this is a cosmetic enhancement to a source actor a caller
// has already committed to the database, not a precondition for it, so
// none of these failures may propagate to ensureRSSSourceActors' caller
// (which would otherwise fail this process's entire startup, or a
// scheduler tick, over a third party's missing icon).
func fetchAndSetSourceFavicon(ctx context.Context, db *sqlite.DB, driveSvc *drive.Service, client *safehttp.Client, maxBytes int64, actorID, host, username string, logger *slog.Logger) {
	pngData, err := favicon.Fetch(ctx, client, host, maxBytes)
	if err != nil {
		logger.Info("rss source favicon fetch skipped", "host", host, "username", username, "error", err)
		return
	}
	file, err := driveSvc.CreateSystemFile(ctx, domain.FilePurposeSourceFavicon, host+"-favicon.png", pngData)
	if err != nil {
		logger.Warn("rss source favicon store failed", "host", host, "username", username, "error", err)
		return
	}
	if err := db.Actors.SetAvatarFileID(ctx, actorID, &file.ID); err != nil {
		logger.Warn("rss source favicon avatar assignment failed", "host", host, "username", username, "error", err)
	}
}

// driveCapacityBytes is POST /api/drive's advertised capacity — cosmetic
// only (see drive.Config.CapacityBytes's doc comment), so a fixed
// generous constant rather than a configuration key. 10 GiB comfortably
// exceeds what a single-owner deployment's raster-image Drive uploads
// (DRIVE_MAX_FILE_BYTES-bounded, 10 MiB by default) would accumulate
// before an operator notices and raises it in a future release, if ever
// needed.
const driveCapacityBytes = 10 * 1024 * 1024 * 1024

// driveMultipartOverheadBytes is added to DRIVE_MAX_FILE_BYTES when
// deriving the effective HTTP body-size ceiling (see opts.
// MaxRequestBodyBytes below): headroom for POST /api/drive/files/
// create's multipart boundary markers and its other form fields
// (folderId, name, comment, isSensitive, the "i" token — each itself
// bounded far below this by maxDriveTextFieldBytes in
// internal/httpserver/drive_handlers.go), not for the file part itself.
const driveMultipartOverheadBytes = 64 * 1024

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
