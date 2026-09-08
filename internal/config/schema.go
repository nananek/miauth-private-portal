package config

// Known environment variable and config-file key names. These constants
// are the single source of truth for every config key this service
// recognizes: config file parsing rejects any key not listed here, and
// environment variable lookup only reads these known names rather than
// scanning the full OS environment (PATH, HOME, and similar unrelated
// variables must never fail startup).
const (
	KeyAppEnv                = "APP_ENV"
	KeyHTTPHost              = "HTTP_HOST"
	KeyHTTPPort              = "HTTP_PORT"
	KeyHTTPReadTimeout       = "HTTP_READ_TIMEOUT"
	KeyHTTPReadHeaderTimeout = "HTTP_READ_HEADER_TIMEOUT"
	KeyHTTPWriteTimeout      = "HTTP_WRITE_TIMEOUT"
	KeyHTTPIdleTimeout       = "HTTP_IDLE_TIMEOUT"
	KeyHTTPMaxBodyBytes      = "HTTP_MAX_BODY_BYTES"
	KeyHTTPShutdownGrace     = "HTTP_SHUTDOWN_GRACE_PERIOD"
	KeyLogLevel              = "LOG_LEVEL"
	KeyLogFormat             = "LOG_FORMAT"
	KeyDBPath                = "DB_PATH"
	KeyDBBusyTimeoutMS       = "DB_BUSY_TIMEOUT_MS"
	KeyDBMaxOpenConns        = "DB_MAX_OPEN_CONNS"

	KeyLocalOrigin          = "LOCAL_ORIGIN"
	KeyAriaClientCallbacks  = "ARIA_CLIENT_CALLBACKS"
	KeyOwnerUsername        = "OWNER_USERNAME"
	KeyOwnerDisplayName     = "OWNER_DISPLAY_NAME"
	KeyJobsWorkerID         = "JOBS_WORKER_ID"
	KeyJobsPollInterval     = "JOBS_POLL_INTERVAL"
	KeyJobsClaimBatchSize   = "JOBS_CLAIM_BATCH_SIZE"
	KeyJobsLeaseDuration    = "JOBS_LEASE_DURATION"
	KeyJobsLeaseRenewMargin = "JOBS_LEASE_RENEW_MARGIN"
	KeyJobsMaxAttempts      = "JOBS_MAX_ATTEMPTS"
	KeyJobsBackoffBase      = "JOBS_BACKOFF_BASE"
	KeyJobsBackoffMax       = "JOBS_BACKOFF_MAX"
	KeyJobsMaxConcurrent    = "JOBS_MAX_CONCURRENT"
	KeyJobsShutdownGrace    = "JOBS_SHUTDOWN_GRACE_PERIOD"

	KeyLLMEnabled                  = "LLM_ENABLED"
	KeyLLMBaseURL                  = "LLM_BASE_URL"
	KeyLLMAPIKey                   = "LLM_API_KEY"
	KeyLLMModel                    = "LLM_MODEL"
	KeyLLMTimeout                  = "LLM_TIMEOUT"
	KeyLLMMaxOutputTokens          = "LLM_MAX_OUTPUT_TOKENS"
	KeyLLMThreadContextMaxMessages = "LLM_THREAD_CONTEXT_MAX_MESSAGES"
	KeyLLMThreadContextMaxChars    = "LLM_THREAD_CONTEXT_MAX_CHARS"

	KeyLLMClassificationEnabled                  = "LLM_CLASSIFICATION_ENABLED"
	KeyLLMClassificationModel                    = "LLM_CLASSIFICATION_MODEL"
	KeyLLMClassificationMaxOutputTokens          = "LLM_CLASSIFICATION_MAX_OUTPUT_TOKENS"
	KeyLLMClassificationThreadContextMaxMessages = "LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_MESSAGES"
	KeyLLMClassificationThreadContextMaxChars    = "LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_CHARS"

	KeyRSSEnabled           = "RSS_ENABLED"
	KeyRSSFeedURLs          = "RSS_FEED_URLS"
	KeyRSSPollInterval      = "RSS_POLL_INTERVAL"
	KeyRSSFetchTimeout      = "RSS_FETCH_TIMEOUT"
	KeyRSSMaxResponseBytes  = "RSS_MAX_RESPONSE_BYTES"
	KeyRSSMaxRedirects      = "RSS_MAX_REDIRECTS"
	KeyRSSSummaryMaxChars   = "RSS_SUMMARY_MAX_CHARS"
	KeyRSSAllowInsecureHTTP = "RSS_ALLOW_INSECURE_HTTP"

	KeyIMAPEnabled          = "IMAP_ENABLED"
	KeyIMAPHost             = "IMAP_HOST"
	KeyIMAPPort             = "IMAP_PORT"
	KeyIMAPTLSMode          = "IMAP_TLS_MODE"
	KeyIMAPUsername         = "IMAP_USERNAME"
	KeyIMAPPassword         = "IMAP_PASSWORD"
	KeyIMAPMailbox          = "IMAP_MAILBOX"
	KeyIMAPPollInterval     = "IMAP_POLL_INTERVAL"
	KeyIMAPFetchTimeout     = "IMAP_FETCH_TIMEOUT"
	KeyIMAPMaxMessageBytes  = "IMAP_MAX_MESSAGE_BYTES"
	KeyIMAPSnippetMaxChars  = "IMAP_SNIPPET_MAX_CHARS"
	KeyIMAPStoreFullBody    = "IMAP_STORE_FULL_BODY"
	KeyIMAPFullBodyMaxChars = "IMAP_FULL_BODY_MAX_CHARS"
	KeyIMAPMailfetchSocket  = "IMAP_MAILFETCH_SOCKET"

	KeyOpenWebUIEnabled          = "OPENWEBUI_ENABLED"
	KeyOpenWebUIBaseURL          = "OPENWEBUI_BASE_URL"
	KeyOpenWebUIAllowedOrigins   = "OPENWEBUI_ALLOWED_ORIGINS"
	KeyOpenWebUIAPIKey           = "OPENWEBUI_API_KEY"
	KeyOpenWebUIWorkspaceName    = "OPENWEBUI_WORKSPACE_NAME"
	KeyOpenWebUIDefaultModelID   = "OPENWEBUI_DEFAULT_MODEL_ID"
	KeyOpenWebUIPresentationHost = "OPENWEBUI_PRESENTATION_HOST"

	// KeyOpenWebUICatalogSyncInterval is Issue #75's model catalog sync
	// interval (Registry.SyncCatalog, run by CatalogScheduler).
	KeyOpenWebUICatalogSyncInterval = "OPENWEBUI_CATALOG_SYNC_INTERVAL"

	// The five keys below are Issue #53's (OWUI-B) client-side bounds and
	// generation gate. They exist from this PR (Issue #53 PR1) on, but
	// nothing reads them yet: no bridge, job, or provider adapter is
	// wired up until Issue #53's later PRs build one.
	KeyOpenWebUIGenerationEnabled  = "OPENWEBUI_GENERATION_ENABLED"
	KeyOpenWebUITimeout            = "OPENWEBUI_TIMEOUT"
	KeyOpenWebUIMaxResponseBytes   = "OPENWEBUI_MAX_RESPONSE_BYTES"
	KeyOpenWebUIMaxRequestBytes    = "OPENWEBUI_MAX_REQUEST_BYTES"
	KeyOpenWebUIMaxContextMessages = "OPENWEBUI_MAX_CONTEXT_MESSAGES"

	// KeyOpenWebUIToolTurnTimeout is Issue #93's own per-HTTP-call bound
	// for Client.StreamTurn (ADR-0005 D24) — deliberately separate from
	// OPENWEBUI_TIMEOUT because a StreamTurn connection stays open for
	// Open WebUI's whole native tool-call loop, potentially several
	// rounds, not one buffered call. Consumed by
	// internal/provider/openwebui.Client.StreamTurn, and — since D26's
	// dispatch wiring (TurnJob.handleCreationPending) — reached whenever
	// a branch's first turn resolves a non-empty tool_ids/web_search.
	KeyOpenWebUIToolTurnTimeout = "OPENWEBUI_TOOL_TURN_TIMEOUT"

	// KeyOpenWebUIWebSearchEnabled is Issue #72's opt-in web-search flag
	// for the outbound completions call, tri-state since Issue #75 AC#11
	// (ADR-0005 D21): unset defers per model to that model's own synced
	// GET /api/models info.meta.defaultFeatureIds (whether it contains
	// "web_search"), while an explicit true/false overrides every model
	// uniformly regardless of its own default. Unlike per-model tool_ids
	// (OPENWEBUI_TOOL_IDS, Issue #72's sibling flag, removed entirely by
	// Issue #75 — every model's tool_ids come from its own toolIds alone,
	// with no config override left at all), this key is kept, just no
	// longer forced to a single deployment-wide boolean.
	KeyOpenWebUIWebSearchEnabled = "OPENWEBUI_WEB_SEARCH_ENABLED"

	// KeyOpenWebUIViewerBaseURL is Issues #81+#84's one new config key
	// (ADR-0005 D23): a browser-reachable origin for the same Open WebUI
	// instance OPENWEBUI_BASE_URL names, used only to render an
	// owner-facing "<value>/c/<remote_chat_id>" link into a generated
	// reply's own text — never dialed by this server (D23: not added to
	// OPENWEBUI_ALLOWED_ORIGINS, D11's SSRF policy does not apply to it).
	// Optional; unset (the default) also gates OPENWEBUI_ENABLED
	// deployments' title-generation request entirely (see
	// internal/openwebui.TurnJobConfig.ViewerBaseURL), so leaving it
	// unset reproduces pre-#84 behavior exactly.
	KeyOpenWebUIViewerBaseURL = "OPENWEBUI_VIEWER_BASE_URL"

	// The keys below configure Issue #77 PR1's Drive storage foundation
	// (ADR-0006). Nothing reads files through them yet — no HTTP
	// endpoint, job, or repository exists until PR3/PR4/PR5/PR6 build
	// one — but they are parsed and validated from this PR on, the same
	// "config exists before its first reader" shape
	// KeyOpenWebUIGenerationEnabled and its siblings used for Issue #53.
	KeyDriveBackend           = "DRIVE_BACKEND"
	KeyDriveDataDir           = "DRIVE_DATA_DIR"
	KeyDriveS3Endpoint        = "DRIVE_S3_ENDPOINT"
	KeyDriveS3Bucket          = "DRIVE_S3_BUCKET"
	KeyDriveS3AccessKeyID     = "DRIVE_S3_ACCESS_KEY_ID"
	KeyDriveS3SecretAccessKey = "DRIVE_S3_SECRET_ACCESS_KEY"
	KeyDriveS3UseSSL          = "DRIVE_S3_USE_SSL"
	KeyDriveS3Region          = "DRIVE_S3_REGION"
	KeyDriveMaxFileBytes      = "DRIVE_MAX_FILE_BYTES"
	KeyDriveMaxImageWidth     = "DRIVE_MAX_IMAGE_WIDTH"
	KeyDriveMaxImageHeight    = "DRIVE_MAX_IMAGE_HEIGHT"
	// KeyDriveOrphanGCInterval is Issue #77 PR7's orphan-file GC sweep
	// interval (internal/drive.GCScheduler/RunOrphanGC): how often this
	// deployment reconciles the configured Storage backend against every
	// files.storage_key, deleting any object no row references.
	KeyDriveOrphanGCInterval = "DRIVE_ORPHAN_GC_INTERVAL"
)

// knownKeyOrder lists every known key once, in the order environment
// variables are looked up and documentation is generated.
var knownKeyOrder = []string{
	KeyAppEnv,
	KeyHTTPHost,
	KeyHTTPPort,
	KeyHTTPReadTimeout,
	KeyHTTPReadHeaderTimeout,
	KeyHTTPWriteTimeout,
	KeyHTTPIdleTimeout,
	KeyHTTPMaxBodyBytes,
	KeyHTTPShutdownGrace,
	KeyLogLevel,
	KeyLogFormat,
	KeyDBPath,
	KeyDBBusyTimeoutMS,
	KeyDBMaxOpenConns,
	KeyLocalOrigin,
	KeyAriaClientCallbacks,
	KeyOwnerUsername,
	KeyOwnerDisplayName,
	KeyJobsWorkerID,
	KeyJobsPollInterval,
	KeyJobsClaimBatchSize,
	KeyJobsLeaseDuration,
	KeyJobsLeaseRenewMargin,
	KeyJobsMaxAttempts,
	KeyJobsBackoffBase,
	KeyJobsBackoffMax,
	KeyJobsMaxConcurrent,
	KeyJobsShutdownGrace,
	KeyLLMEnabled,
	KeyLLMBaseURL,
	KeyLLMAPIKey,
	KeyLLMModel,
	KeyLLMTimeout,
	KeyLLMMaxOutputTokens,
	KeyLLMThreadContextMaxMessages,
	KeyLLMThreadContextMaxChars,
	KeyLLMClassificationEnabled,
	KeyLLMClassificationModel,
	KeyLLMClassificationMaxOutputTokens,
	KeyLLMClassificationThreadContextMaxMessages,
	KeyLLMClassificationThreadContextMaxChars,
	KeyRSSEnabled,
	KeyRSSFeedURLs,
	KeyRSSPollInterval,
	KeyRSSFetchTimeout,
	KeyRSSMaxResponseBytes,
	KeyRSSMaxRedirects,
	KeyRSSSummaryMaxChars,
	KeyRSSAllowInsecureHTTP,
	KeyIMAPEnabled,
	KeyIMAPHost,
	KeyIMAPPort,
	KeyIMAPTLSMode,
	KeyIMAPUsername,
	KeyIMAPPassword,
	KeyIMAPMailbox,
	KeyIMAPPollInterval,
	KeyIMAPFetchTimeout,
	KeyIMAPMaxMessageBytes,
	KeyIMAPSnippetMaxChars,
	KeyIMAPStoreFullBody,
	KeyIMAPFullBodyMaxChars,
	KeyIMAPMailfetchSocket,
	KeyOpenWebUIEnabled,
	KeyOpenWebUIBaseURL,
	KeyOpenWebUIAllowedOrigins,
	KeyOpenWebUIAPIKey,
	KeyOpenWebUIWorkspaceName,
	KeyOpenWebUIDefaultModelID,
	KeyOpenWebUIPresentationHost,
	KeyOpenWebUICatalogSyncInterval,
	KeyOpenWebUIGenerationEnabled,
	KeyOpenWebUITimeout,
	KeyOpenWebUIMaxResponseBytes,
	KeyOpenWebUIMaxRequestBytes,
	KeyOpenWebUIMaxContextMessages,
	KeyOpenWebUIWebSearchEnabled,
	KeyOpenWebUIViewerBaseURL,
	KeyOpenWebUIToolTurnTimeout,
	KeyDriveBackend,
	KeyDriveDataDir,
	KeyDriveS3Endpoint,
	KeyDriveS3Bucket,
	KeyDriveS3AccessKeyID,
	KeyDriveS3SecretAccessKey,
	KeyDriveS3UseSSL,
	KeyDriveS3Region,
	KeyDriveMaxFileBytes,
	KeyDriveMaxImageWidth,
	KeyDriveMaxImageHeight,
	KeyDriveOrphanGCInterval,
}

func isKnownKey(key string) bool {
	for _, k := range knownKeyOrder {
		if k == key {
			return true
		}
	}
	return false
}

// KnownKeys returns every config key this service recognizes, in lookup
// order. It exists for documentation and operational tooling; Load uses
// knownKeyOrder directly.
func KnownKeys() []string {
	out := make([]string, len(knownKeyOrder))
	copy(out, knownKeyOrder)
	return out
}
