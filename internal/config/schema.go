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
	KeyIdentityOrigin       = "IDENTITY_ORIGIN"
	KeyAllowedMisskeyUserID = "ALLOWED_MISSKEY_USER_ID"
	KeyAriaClientCallbacks  = "ARIA_CLIENT_CALLBACKS"
	KeyUpstreamHTTPTimeout  = "UPSTREAM_HTTP_TIMEOUT"
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
	KeyIdentityOrigin,
	KeyAllowedMisskeyUserID,
	KeyAriaClientCallbacks,
	KeyUpstreamHTTPTimeout,
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
