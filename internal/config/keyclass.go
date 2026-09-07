package config

// KeyClass classifies a known config key for ADR-0006's DB-backed
// runtime configuration overlay: which keys may ever get an app_config
// row (db-eligible), which must never leave config's own file/env
// resolution (secret), and everything else (bootstrap-only).
type KeyClass int

const (
	// ClassBootstrapOnly is every key not explicitly classified as
	// secret or db-eligible below: process topology, network
	// destinations and their allowlists, and every value this service
	// only ever reads once at startup, before a database connection or
	// any continuously-running consumer exists. It is also the default
	// classification, so a newly added config key is bootstrap-only
	// unless someone deliberately adds it to secretKeys or
	// dbEligibleKeys.
	ClassBootstrapOnly KeyClass = iota
	// ClassSecret keys are never written to app_config at all (ADR-0005
	// D10, reaffirmed by ADR-0006): miauthctl config set/import reject
	// them outright, so "cannot be stored" is enforced by this
	// classification rather than by remembering to redact a column.
	ClassSecret
	// ClassDBEligible keys may have an app_config row that overrides
	// config.Load's file/env/default resolution — ADR-0006's Tier A.
	ClassDBEligible
)

// secretKeys are the credentials ADR-0005 D10 keeps out of the database
// entirely: config (env/file) remains their only home, exactly as
// before ADR-0006.
var secretKeys = map[string]bool{
	KeyLLMAPIKey:       true,
	KeyOpenWebUIAPIKey: true,
	KeyIMAPUsername:    true,
	KeyIMAPPassword:    true,
}

// dbEligibleKeys is ADR-0006's Tier A: keys a continuously-running or
// continuously-polled component re-reads often enough that an
// app_config override actually reaches it without a restart. See each
// component's own reload wiring (internal/jobs, internal/ingest,
// internal/llmreply, internal/llmclassify, internal/openwebui) for how.
//
// Every other known key is bootstrap-only, including:
//   - JOBS_WORKER_ID: identifies this process's own in-flight lease
//     ownership, so changing it mid-run would make an existing lease's
//     owner ambiguous.
//   - RSS_ENABLED, IMAP_ENABLED, LLM_ENABLED, LLM_CLASSIFICATION_ENABLED,
//     OPENWEBUI_ENABLED: the five subsystem-construction flags ADR-0006
//     explicitly leaves out of scope — toggling one at runtime would
//     require dynamically starting/stopping goroutines cmd/server only
//     ever builds once, at startup.
//   - OPENWEBUI_GENERATION_ENABLED: already independently live via the
//     openwebui_workspaces row (Registry.Seed/SetGenerationEnabled), not
//     through internal/config or app_config at all.
//   - Every network-destination setting (OPENWEBUI_BASE_URL,
//     OPENWEBUI_ALLOWED_ORIGINS, IMAP_HOST/PORT/TLS_MODE, LLM_BASE_URL,
//     ...): switching a connection target at runtime, without repeating
//     the SSRF-allowlist scrutiny config.Validate applies once at
//     startup, is deliberately out of ADR-0006's scope.
var dbEligibleKeys = map[string]bool{
	// internal/jobs.Manager.
	KeyJobsPollInterval:     true,
	KeyJobsClaimBatchSize:   true,
	KeyJobsLeaseDuration:    true,
	KeyJobsLeaseRenewMargin: true,
	KeyJobsMaxAttempts:      true,
	KeyJobsBackoffBase:      true,
	KeyJobsBackoffMax:       true,
	KeyJobsMaxConcurrent:    true,
	KeyJobsShutdownGrace:    true,

	// internal/ingest.Scheduler, shared by RSS and IMAP (Issue #76's own
	// motivating example: adding/removing an RSS feed with no restart).
	KeyRSSPollInterval:    true,
	KeyRSSFeedURLs:        true,
	KeyRSSSummaryMaxChars: true,
	KeyIMAPPollInterval:   true,

	// The IMAP fetch job handler's own per-request tunables.
	// cmd/mailfetch itself carries no config (ADR-0003): connection
	// details are passed per request, not read from its own config.
	KeyIMAPFetchTimeout:     true,
	KeyIMAPMaxMessageBytes:  true,
	KeyIMAPSnippetMaxChars:  true,
	KeyIMAPStoreFullBody:    true,
	KeyIMAPFullBodyMaxChars: true,

	// internal/llmreply.Service / internal/llmclassify.Service.
	KeyLLMModel:                                  true,
	KeyLLMTimeout:                                true,
	KeyLLMMaxOutputTokens:                        true,
	KeyLLMThreadContextMaxMessages:               true,
	KeyLLMThreadContextMaxChars:                  true,
	KeyLLMClassificationModel:                    true,
	KeyLLMClassificationMaxOutputTokens:          true,
	KeyLLMClassificationThreadContextMaxMessages: true,
	KeyLLMClassificationThreadContextMaxChars:    true,

	// Open WebUI's catalog scheduler and turn job.
	KeyOpenWebUICatalogSyncInterval: true,
	KeyOpenWebUIWebSearchEnabled:    true,
}

// ClassOf returns key's ADR-0006 classification. An unknown key (one
// isKnownKey rejects) is ClassBootstrapOnly, the safest possible answer
// for a value that can never reach config.Load in the first place.
func ClassOf(key string) KeyClass {
	if secretKeys[key] {
		return ClassSecret
	}
	if dbEligibleKeys[key] {
		return ClassDBEligible
	}
	return ClassBootstrapOnly
}

// IsSecretKey reports whether key must never be written to app_config.
func IsSecretKey(key string) bool { return secretKeys[key] }

// IsDBEligibleKey reports whether key may have an app_config row.
func IsDBEligibleKey(key string) bool { return dbEligibleKeys[key] }

// DBEligibleKeys returns every db-eligible key, in knownKeyOrder's
// order, for miauthctl config import/list and the startup auto-seed
// path (ADR-0006 §2-4) to iterate deterministically.
func DBEligibleKeys() []string {
	out := make([]string, 0, len(dbEligibleKeys))
	for _, k := range knownKeyOrder {
		if dbEligibleKeys[k] {
			out = append(out, k)
		}
	}
	return out
}
