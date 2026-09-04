// Package config loads and validates this service's startup configuration
// from defaults, an optional dotenv-style config file, and environment
// variables, in that increasing priority order. It never depends on any
// other internal package, and it never lets an invalid or unknown value's
// raw text reach an error message or log line.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment selects environment-dependent validation, primarily
// production hardening in Validate.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

// Config is this service's fully validated startup configuration.
type Config struct {
	Env  Environment
	HTTP HTTPConfig
	Log  LogConfig
	DB   DBConfig
	Auth AuthConfig
	Jobs JobsConfig
	LLM  LLMConfig
	RSS  RSSConfig
}

// HTTPConfig bounds the HTTP server's listen address, timeouts, request
// size, and shutdown behavior.
type HTTPConfig struct {
	Host                string
	Port                int
	ReadTimeout         time.Duration
	ReadHeaderTimeout   time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	MaxRequestBodyBytes int64
	ShutdownGracePeriod time.Duration
}

// Addr returns the host:port the HTTP server should listen on.
func (h HTTPConfig) Addr() string {
	return net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
}

// LogConfig selects the structured logger's minimum level and encoding.
type LogConfig struct {
	Level  string
	Format string
}

// DBConfig bounds the SQLite database file path, busy timeout, and
// connection pool size (internal/storage/sqlite.Open).
type DBConfig struct {
	Path         string
	BusyTimeout  time.Duration
	MaxOpenConns int
}

// JobsConfig bounds durable background-job polling, leasing, retries,
// concurrency, and shutdown behavior.
type JobsConfig struct {
	WorkerID            string
	PollInterval        time.Duration
	ClaimBatchSize      int
	LeaseDuration       time.Duration
	LeaseRenewMargin    time.Duration
	MaxAttempts         int
	BackoffBase         time.Duration
	BackoffMax          time.Duration
	MaxConcurrentJobs   int
	ShutdownGracePeriod time.Duration
}

// AuthConfig configures the local MiAuth flow defined by ADR-0002.
type AuthConfig struct {
	// LocalOrigin is this service's configured public origin.
	LocalOrigin string
	// AriaClientCallbacks is the exact-match allowlist of client return
	// callbacks Aria may supply to GET /miauth/{session} (for example
	// Android's aria://aria/miauth). A non-HTTPS scheme is explicitly
	// permitted here; an empty list rejects any
	// client-supplied callback.
	AriaClientCallbacks []string
	// OwnerUsername is the Misskey-compatible username this service
	// reports for the local owner actor until Issue #5's follow-up adds
	// self-service profile editing.
	OwnerUsername string
	// OwnerDisplayName is the optional display name reported as the
	// owner's UserDetailedNotMe.name. Empty means null (unset), matching
	// Misskey's own nullable name field.
	OwnerDisplayName string
}

// LLMConfig configures Issue #9's OpenAI-compatible reply/follow-up
// generation job. Enabled defaults to false: no generation job is ever
// enqueued and no request ever reaches BaseURL until an operator
// explicitly turns this on, so a fresh deployment cannot accidentally
// leak post content to a third-party endpoint.
type LLMConfig struct {
	// Enabled gates every generation job enqueue in internal/httpserver
	// and internal/llmreply. False is the safe default.
	Enabled bool
	// BaseURL is the OpenAI-compatible API base (for example
	// "https://api.openai.com/v1" or a self-hosted equivalent).
	// internal/provider/openai appends the chat-completions path to it.
	BaseURL string
	// APIKey authenticates against BaseURL. Never logged or returned to
	// a client; see Redacted.
	APIKey string
	Model  string
	// Timeout bounds every HTTP call this service makes to BaseURL.
	Timeout time.Duration
	// MaxOutputTokens bounds a single generation's completion length.
	MaxOutputTokens int
	// ThreadContextMaxMessages and ThreadContextMaxChars bound how much
	// prior thread history internal/llmreply's prompt builder includes,
	// so a long thread cannot make a single generation request unbounded.
	ThreadContextMaxMessages int
	ThreadContextMaxChars    int

	// ClassificationEnabled gates Issue #10's post classification job,
	// independent of Enabled: an operator can run reply generation and
	// classification on independent schedules, including one without the
	// other. False is the safe default.
	ClassificationEnabled bool
	// ClassificationModel is the model internal/llmclassify records and
	// requests. Empty falls back to Model, since classification commonly
	// reuses the same model as reply generation unless overridden.
	ClassificationModel string
	// ClassificationMaxOutputTokens bounds one classification completion's
	// length.
	ClassificationMaxOutputTokens int
	// ClassificationThreadContextMaxMessages and
	// ClassificationThreadContextMaxChars bound how many same-thread
	// candidate entries internal/llmclassify's prompt builder offers the
	// model as related-post candidates. Kept separate from
	// ThreadContextMaxMessages/ThreadContextMaxChars (reply generation's
	// budget) so tuning one never silently changes the other.
	ClassificationThreadContextMaxMessages int
	ClassificationThreadContextMaxChars    int
}

// ClassificationModelOrDefault returns ClassificationModel, falling back
// to the shared reply-generation Model when unset: classification
// commonly reuses the same model unless an operator explicitly wants a
// cheaper one for it.
func (c LLMConfig) ClassificationModelOrDefault() string {
	if c.ClassificationModel != "" {
		return c.ClassificationModel
	}
	return c.Model
}

// RSSConfig configures Issue #11's RSS/Atom ingestion. Enabled defaults
// to false: no source is ever seeded, no adapter/scheduler is
// constructed, and no request ever reaches a configured feed URL until
// an operator explicitly turns this on — the same safe-default shape as
// LLMConfig.Enabled.
type RSSConfig struct {
	// Enabled gates the ingestion scheduler, adapter, and job handler
	// entirely. False is the safe default.
	Enabled bool
	// FeedURLs are the configured RSS/Atom feed URLs, seeded as
	// domain.ExternalSource rows (kind "rss") at startup.
	FeedURLs []string
	// PollInterval is how often each configured feed is re-fetched.
	PollInterval time.Duration
	// FetchTimeout bounds a single feed fetch's HTTP round trip; must be
	// less than PollInterval.
	FetchTimeout time.Duration
	// MaxResponseBytes bounds how much of a feed response is read into
	// memory.
	MaxResponseBytes int64
	// MaxRedirects bounds how many redirect hops a feed fetch follows.
	MaxRedirects int
	// SummaryMaxChars bounds each ingested item's normalized
	// title/body length after HTML tags are stripped.
	SummaryMaxChars int
	// AllowInsecureHTTP permits a feed URL to use "http" instead of
	// requiring "https", mirroring LOCAL_ORIGIN's production https
	// enforcement pattern: false (the default) rejects any http feed
	// URL at config validation time.
	AllowInsecureHTTP bool
}

// FieldError names one invalid, missing, or unknown config field. It never
// carries the offending raw value, so it is always safe to log.
type FieldError struct {
	Key    string
	Reason string
}

func (e FieldError) String() string {
	return fmt.Sprintf("%s: %s", e.Key, e.Reason)
}

// ValidationError collects one or more FieldErrors from Load or Validate.
// Its Error() output never includes a raw config value.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.String()
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

// LoadOptions controls where Load reads configuration from.
type LoadOptions struct {
	// ConfigFilePath is an optional dotenv-style file. A missing file is
	// not an error; a malformed file or one containing an unknown key is.
	ConfigFilePath string
	// Getenv looks up a single environment variable by name. It defaults
	// to os.LookupEnv and is overridden in tests. Load never scans the
	// full OS environment: only the known keys in schema.go are read, so
	// unrelated process environment variables (PATH, HOME, ...) can never
	// fail startup.
	Getenv func(string) (string, bool)
}

// Load builds a validated Config from defaults, an optional config file,
// and environment variables. It fails fast with a *ValidationError on any
// unknown config-file key, invalid value, or missing required field.
func Load(opts LoadOptions) (*Config, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.LookupEnv
	}

	values := map[string]string{}

	if opts.ConfigFilePath != "" {
		fileValues, err := loadConfigFile(opts.ConfigFilePath)
		if err != nil {
			return nil, err
		}
		for k, v := range fileValues {
			values[k] = v
		}
	}

	var errs []FieldError
	for _, key := range knownKeyOrder {
		v, ok := opts.Getenv(key)
		if !ok {
			continue
		}
		if v == "" {
			// An env var that is *set but empty* (e.g. an unresolved
			// ${VAR} in a docker-compose file or systemd EnvironmentFile)
			// is ambiguous: parseOptional* would silently treat it as
			// "unset" and fall back to the default, discarding whatever
			// the config file specified with no diagnostic at all. Fail
			// closed instead of guessing.
			errs = append(errs, FieldError{Key: key, Reason: "environment variable is set to an empty value; unset it instead of overriding with an empty string"})
			continue
		}
		values[key] = v
	}

	cfg, parseErrs := parse(values)
	errs = append(errs, parseErrs...)
	if len(errs) > 0 {
		return nil, &ValidationError{Fields: errs}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func loadConfigFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open config file %s: %w", path, err)
	}
	defer f.Close()

	raw, err := ParseEnvFile(f)
	if err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	var unknown []string
	for k := range raw {
		if !isKnownKey(k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		// Sorted so which key(s) get reported is deterministic across
		// runs of the same file, instead of depending on Go's randomized
		// map iteration order.
		sort.Strings(unknown)
		fields := make([]FieldError, len(unknown))
		for i, k := range unknown {
			fields[i] = FieldError{Key: k, Reason: fmt.Sprintf("unknown config key in %s", path)}
		}
		return nil, &ValidationError{Fields: fields}
	}
	return raw, nil
}

// allowedEnvironments is the single source of truth for valid Env values,
// shared by parse (which validates the raw config-file/env-var string) and
// Validate (which re-checks an already-typed Config built by hand).
var allowedEnvironments = []string{string(EnvDevelopment), string(EnvStaging), string(EnvProduction)}

// Field bound constants shared by parse (which validates the raw
// config-file/env-var string) and Validate (which re-checks an
// already-typed Config built by hand), so the two paths cannot drift
// apart the way they once did (DB_BUSY_TIMEOUT_MS's upper bound was
// present in parse but missing from Validate until it was fixed).
const (
	httpPortMin, httpPortMax                     = 1, 65535
	httpMaxRequestBodyBytesMin                   = 1
	dbBusyTimeoutMSMin, dbBusyTimeoutMSMax       = 1, 600_000
	dbMaxOpenConnsMin, dbMaxOpenConnsMax         = 1, 100
	jobsClaimBatchSizeMin, jobsClaimBatchSizeMax = 1, 100
	jobsMaxAttemptsMin, jobsMaxAttemptsMax       = 1, 100
	jobsMaxConcurrentMin, jobsMaxConcurrentMax   = 1, 64

	llmMaxOutputTokensMin, llmMaxOutputTokensMax                   = 1, 32768
	llmThreadContextMaxMessagesMin, llmThreadContextMaxMessagesMax = 1, 500
	llmThreadContextMaxCharsMin, llmThreadContextMaxCharsMax       = 1, 200_000

	rssMaxResponseBytesMin                       = 1
	rssMaxRedirectsMin, rssMaxRedirectsMax       = 0, 20
	rssSummaryMaxCharsMin, rssSummaryMaxCharsMax = 1, 100_000
)

func parse(values map[string]string) (Config, []FieldError) {
	var errs []FieldError
	var cfg Config

	cfg.Env = Environment(parseRequiredEnum(values, KeyAppEnv, allowedEnvironments, &errs))

	cfg.HTTP.Host = parseOptionalString(values, KeyHTTPHost, "0.0.0.0")
	cfg.HTTP.Port = parseOptionalInt(values, KeyHTTPPort, 8080, httpPortMin, httpPortMax, &errs)
	cfg.HTTP.ReadTimeout = parseOptionalDuration(values, KeyHTTPReadTimeout, 5*time.Second, &errs)
	cfg.HTTP.ReadHeaderTimeout = parseOptionalDuration(values, KeyHTTPReadHeaderTimeout, 5*time.Second, &errs)
	cfg.HTTP.WriteTimeout = parseOptionalDuration(values, KeyHTTPWriteTimeout, 15*time.Second, &errs)
	cfg.HTTP.IdleTimeout = parseOptionalDuration(values, KeyHTTPIdleTimeout, 60*time.Second, &errs)
	cfg.HTTP.MaxRequestBodyBytes = parseOptionalInt64(values, KeyHTTPMaxBodyBytes, 1<<20, httpMaxRequestBodyBytesMin, &errs)
	cfg.HTTP.ShutdownGracePeriod = parseOptionalDuration(values, KeyHTTPShutdownGrace, 15*time.Second, &errs)

	cfg.Log.Level = parseOptionalEnum(values, KeyLogLevel, "info", []string{"debug", "info", "warn", "error"}, &errs)
	cfg.Log.Format = parseOptionalEnum(values, KeyLogFormat, "text", []string{"json", "text"}, &errs)

	cfg.DB.Path = parseOptionalString(values, KeyDBPath, "./data/portal.db")
	busyTimeoutMS := parseOptionalInt(values, KeyDBBusyTimeoutMS, 5000, dbBusyTimeoutMSMin, dbBusyTimeoutMSMax, &errs)
	cfg.DB.BusyTimeout = time.Duration(busyTimeoutMS) * time.Millisecond
	cfg.DB.MaxOpenConns = parseOptionalInt(values, KeyDBMaxOpenConns, 8, dbMaxOpenConnsMin, dbMaxOpenConnsMax, &errs)

	cfg.Auth.LocalOrigin = strings.TrimRight(values[KeyLocalOrigin], "/")
	validateOrigin(&errs, KeyLocalOrigin, cfg.Auth.LocalOrigin, cfg.Env)
	cfg.Auth.AriaClientCallbacks = parseOptionalCallbackList(values, KeyAriaClientCallbacks, &errs)
	cfg.Auth.OwnerUsername = parseOptionalString(values, KeyOwnerUsername, "owner")
	validateOwnerUsername(&errs, KeyOwnerUsername, cfg.Auth.OwnerUsername)
	cfg.Auth.OwnerDisplayName = parseOptionalString(values, KeyOwnerDisplayName, "")

	cfg.Jobs.WorkerID = parseOptionalString(values, KeyJobsWorkerID, "")
	cfg.Jobs.PollInterval = parseOptionalDuration(values, KeyJobsPollInterval, time.Second, &errs)
	cfg.Jobs.ClaimBatchSize = parseOptionalInt(values, KeyJobsClaimBatchSize, 10, jobsClaimBatchSizeMin, jobsClaimBatchSizeMax, &errs)
	cfg.Jobs.LeaseDuration = parseOptionalDuration(values, KeyJobsLeaseDuration, 30*time.Second, &errs)
	cfg.Jobs.LeaseRenewMargin = parseOptionalDuration(values, KeyJobsLeaseRenewMargin, 10*time.Second, &errs)
	cfg.Jobs.MaxAttempts = parseOptionalInt(values, KeyJobsMaxAttempts, 8, jobsMaxAttemptsMin, jobsMaxAttemptsMax, &errs)
	cfg.Jobs.BackoffBase = parseOptionalDuration(values, KeyJobsBackoffBase, time.Second, &errs)
	cfg.Jobs.BackoffMax = parseOptionalDuration(values, KeyJobsBackoffMax, 10*time.Minute, &errs)
	cfg.Jobs.MaxConcurrentJobs = parseOptionalInt(values, KeyJobsMaxConcurrent, 4, jobsMaxConcurrentMin, jobsMaxConcurrentMax, &errs)
	cfg.Jobs.ShutdownGracePeriod = parseOptionalDuration(values, KeyJobsShutdownGrace, 15*time.Second, &errs)

	cfg.LLM.Enabled = parseOptionalBool(values, KeyLLMEnabled, false, &errs)
	cfg.LLM.BaseURL = strings.TrimRight(parseOptionalString(values, KeyLLMBaseURL, ""), "/")
	cfg.LLM.APIKey = parseOptionalString(values, KeyLLMAPIKey, "")
	cfg.LLM.Model = parseOptionalString(values, KeyLLMModel, "")
	cfg.LLM.Timeout = parseOptionalDuration(values, KeyLLMTimeout, 30*time.Second, &errs)
	cfg.LLM.MaxOutputTokens = parseOptionalInt(values, KeyLLMMaxOutputTokens, 1024, llmMaxOutputTokensMin, llmMaxOutputTokensMax, &errs)
	cfg.LLM.ThreadContextMaxMessages = parseOptionalInt(values, KeyLLMThreadContextMaxMessages, 20, llmThreadContextMaxMessagesMin, llmThreadContextMaxMessagesMax, &errs)
	cfg.LLM.ThreadContextMaxChars = parseOptionalInt(values, KeyLLMThreadContextMaxChars, 8000, llmThreadContextMaxCharsMin, llmThreadContextMaxCharsMax, &errs)

	cfg.LLM.ClassificationEnabled = parseOptionalBool(values, KeyLLMClassificationEnabled, false, &errs)
	cfg.LLM.ClassificationModel = parseOptionalString(values, KeyLLMClassificationModel, "")
	cfg.LLM.ClassificationMaxOutputTokens = parseOptionalInt(values, KeyLLMClassificationMaxOutputTokens, 1024, llmMaxOutputTokensMin, llmMaxOutputTokensMax, &errs)
	cfg.LLM.ClassificationThreadContextMaxMessages = parseOptionalInt(values, KeyLLMClassificationThreadContextMaxMessages, 20, llmThreadContextMaxMessagesMin, llmThreadContextMaxMessagesMax, &errs)
	cfg.LLM.ClassificationThreadContextMaxChars = parseOptionalInt(values, KeyLLMClassificationThreadContextMaxChars, 8000, llmThreadContextMaxCharsMin, llmThreadContextMaxCharsMax, &errs)

	cfg.RSS.Enabled = parseOptionalBool(values, KeyRSSEnabled, false, &errs)
	cfg.RSS.FeedURLs = splitOptionalURLList(values, KeyRSSFeedURLs)
	cfg.RSS.PollInterval = parseOptionalDuration(values, KeyRSSPollInterval, 15*time.Minute, &errs)
	cfg.RSS.FetchTimeout = parseOptionalDuration(values, KeyRSSFetchTimeout, 15*time.Second, &errs)
	cfg.RSS.MaxResponseBytes = parseOptionalInt64(values, KeyRSSMaxResponseBytes, 2_097_152, rssMaxResponseBytesMin, &errs)
	cfg.RSS.MaxRedirects = parseOptionalInt(values, KeyRSSMaxRedirects, 3, rssMaxRedirectsMin, rssMaxRedirectsMax, &errs)
	cfg.RSS.SummaryMaxChars = parseOptionalInt(values, KeyRSSSummaryMaxChars, 4000, rssSummaryMaxCharsMin, rssSummaryMaxCharsMax, &errs)
	cfg.RSS.AllowInsecureHTTP = parseOptionalBool(values, KeyRSSAllowInsecureHTTP, false, &errs)

	return cfg, errs
}

// Validate re-checks cross-field and environment-dependent rules that a
// single field's parser cannot express alone, such as production
// hardening, and re-checks the same per-field bounds parse enforces
// (positive timeouts, a 1-65535 port, MaxRequestBodyBytes >= 1) so a
// hand-built Config (tests, cmd/server defaults) gets the same safety
// guarantees a Load-produced one does. Load always calls it; a Config
// built by hand should call it too before use.
func (c Config) Validate() error {
	var errs []FieldError

	if !slices.Contains(allowedEnvironments, string(c.Env)) {
		errs = append(errs, FieldError{Key: KeyAppEnv, Reason: "must be one of " + strings.Join(allowedEnvironments, ", ")})
	}

	validateIntBounds(&errs, KeyHTTPPort, c.HTTP.Port, httpPortMin, httpPortMax)
	validatePositiveDuration(&errs, KeyHTTPReadTimeout, c.HTTP.ReadTimeout)
	validatePositiveDuration(&errs, KeyHTTPReadHeaderTimeout, c.HTTP.ReadHeaderTimeout)
	validatePositiveDuration(&errs, KeyHTTPWriteTimeout, c.HTTP.WriteTimeout)
	validatePositiveDuration(&errs, KeyHTTPIdleTimeout, c.HTTP.IdleTimeout)
	validateInt64Min(&errs, KeyHTTPMaxBodyBytes, c.HTTP.MaxRequestBodyBytes, httpMaxRequestBodyBytesMin)
	validatePositiveDuration(&errs, KeyHTTPShutdownGrace, c.HTTP.ShutdownGracePeriod)

	if c.DB.Path == "" {
		errs = append(errs, FieldError{Key: KeyDBPath, Reason: "must not be empty"})
	}
	if ms := c.DB.BusyTimeout.Milliseconds(); ms < dbBusyTimeoutMSMin || ms > dbBusyTimeoutMSMax {
		errs = append(errs, FieldError{Key: KeyDBBusyTimeoutMS, Reason: fmt.Sprintf("must be an integer between %d and %d", dbBusyTimeoutMSMin, dbBusyTimeoutMSMax)})
	}
	validateIntBounds(&errs, KeyDBMaxOpenConns, c.DB.MaxOpenConns, dbMaxOpenConnsMin, dbMaxOpenConnsMax)

	validateOrigin(&errs, KeyLocalOrigin, c.Auth.LocalOrigin, c.Env)
	validateCallbackEntries(&errs, KeyAriaClientCallbacks, c.Auth.AriaClientCallbacks)
	validateOwnerUsername(&errs, KeyOwnerUsername, c.Auth.OwnerUsername)

	validatePositiveDuration(&errs, KeyJobsPollInterval, c.Jobs.PollInterval)
	validateIntBounds(&errs, KeyJobsClaimBatchSize, c.Jobs.ClaimBatchSize, jobsClaimBatchSizeMin, jobsClaimBatchSizeMax)
	validatePositiveDuration(&errs, KeyJobsLeaseDuration, c.Jobs.LeaseDuration)
	validatePositiveDuration(&errs, KeyJobsLeaseRenewMargin, c.Jobs.LeaseRenewMargin)
	if c.Jobs.LeaseRenewMargin > 0 && c.Jobs.LeaseDuration > 0 && c.Jobs.LeaseRenewMargin >= c.Jobs.LeaseDuration {
		errs = append(errs, FieldError{Key: KeyJobsLeaseRenewMargin, Reason: "must be less than " + KeyJobsLeaseDuration})
	}
	validateIntBounds(&errs, KeyJobsMaxAttempts, c.Jobs.MaxAttempts, jobsMaxAttemptsMin, jobsMaxAttemptsMax)
	validatePositiveDuration(&errs, KeyJobsBackoffBase, c.Jobs.BackoffBase)
	validatePositiveDuration(&errs, KeyJobsBackoffMax, c.Jobs.BackoffMax)
	if c.Jobs.BackoffBase > 0 && c.Jobs.BackoffMax > 0 && c.Jobs.BackoffBase > c.Jobs.BackoffMax {
		errs = append(errs, FieldError{Key: KeyJobsBackoffBase, Reason: "must not exceed " + KeyJobsBackoffMax})
	}
	validateIntBounds(&errs, KeyJobsMaxConcurrent, c.Jobs.MaxConcurrentJobs, jobsMaxConcurrentMin, jobsMaxConcurrentMax)
	validatePositiveDuration(&errs, KeyJobsShutdownGrace, c.Jobs.ShutdownGracePeriod)

	// LLM fields are only required/bound-checked when the feature is
	// actually enabled: LLM_ENABLED defaults to false, and a disabled
	// deployment must not fail startup over an unset or zero-value LLM
	// setting it will never use. BaseURL/Timeout are shared connection
	// settings, so either Enabled or ClassificationEnabled requires them.
	if c.LLM.Enabled || c.LLM.ClassificationEnabled {
		validateLLMBaseURL(&errs, KeyLLMBaseURL, c.LLM.BaseURL, c.Env)
		validatePositiveDuration(&errs, KeyLLMTimeout, c.LLM.Timeout)
	}
	if c.LLM.Enabled {
		if c.LLM.Model == "" {
			errs = append(errs, FieldError{Key: KeyLLMModel, Reason: "required when " + KeyLLMEnabled + "=true"})
		}
		validateIntBounds(&errs, KeyLLMMaxOutputTokens, c.LLM.MaxOutputTokens, llmMaxOutputTokensMin, llmMaxOutputTokensMax)
		validateIntBounds(&errs, KeyLLMThreadContextMaxMessages, c.LLM.ThreadContextMaxMessages, llmThreadContextMaxMessagesMin, llmThreadContextMaxMessagesMax)
		validateIntBounds(&errs, KeyLLMThreadContextMaxChars, c.LLM.ThreadContextMaxChars, llmThreadContextMaxCharsMin, llmThreadContextMaxCharsMax)
	}
	if c.LLM.ClassificationEnabled {
		if c.LLM.ClassificationModelOrDefault() == "" {
			errs = append(errs, FieldError{Key: KeyLLMClassificationModel, Reason: "required (directly, or via " + KeyLLMModel + ") when " + KeyLLMClassificationEnabled + "=true"})
		}
		validateIntBounds(&errs, KeyLLMClassificationMaxOutputTokens, c.LLM.ClassificationMaxOutputTokens, llmMaxOutputTokensMin, llmMaxOutputTokensMax)
		validateIntBounds(&errs, KeyLLMClassificationThreadContextMaxMessages, c.LLM.ClassificationThreadContextMaxMessages, llmThreadContextMaxMessagesMin, llmThreadContextMaxMessagesMax)
		validateIntBounds(&errs, KeyLLMClassificationThreadContextMaxChars, c.LLM.ClassificationThreadContextMaxChars, llmThreadContextMaxCharsMin, llmThreadContextMaxCharsMax)
	}

	// RSS fields are only required/bound-checked when the feature is
	// actually enabled: RSS_ENABLED defaults to false, and a disabled
	// deployment must not fail startup over an unset RSS setting it will
	// never use.
	if c.RSS.Enabled {
		if len(c.RSS.FeedURLs) == 0 {
			errs = append(errs, FieldError{Key: KeyRSSFeedURLs, Reason: "required when " + KeyRSSEnabled + "=true"})
		}
		validateRSSFeedURLs(&errs, KeyRSSFeedURLs, c.RSS.FeedURLs, c.RSS.AllowInsecureHTTP)
		validatePositiveDuration(&errs, KeyRSSPollInterval, c.RSS.PollInterval)
		validatePositiveDuration(&errs, KeyRSSFetchTimeout, c.RSS.FetchTimeout)
		if c.RSS.FetchTimeout > 0 && c.RSS.PollInterval > 0 && c.RSS.FetchTimeout >= c.RSS.PollInterval {
			errs = append(errs, FieldError{Key: KeyRSSFetchTimeout, Reason: "must be less than " + KeyRSSPollInterval})
		}
		validateInt64Min(&errs, KeyRSSMaxResponseBytes, c.RSS.MaxResponseBytes, rssMaxResponseBytesMin)
		validateIntBounds(&errs, KeyRSSMaxRedirects, c.RSS.MaxRedirects, rssMaxRedirectsMin, rssMaxRedirectsMax)
		validateIntBounds(&errs, KeyRSSSummaryMaxChars, c.RSS.SummaryMaxChars, rssSummaryMaxCharsMin, rssSummaryMaxCharsMax)
	}

	if c.Env == EnvProduction {
		if c.Log.Format != "json" {
			errs = append(errs, FieldError{Key: KeyLogFormat, Reason: "must be json in production"})
		}
		if c.Log.Level == "debug" {
			errs = append(errs, FieldError{Key: KeyLogLevel, Reason: "must not be debug in production"})
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

// Redacted returns a snapshot of every config field as strings, safe to
// log or print. It is the one place that decides what is safe to show, so
// a future secret-bearing field only needs to be added here once rather
// than trusted at every call site that wants to log the config.
func (c Config) Redacted() map[string]string {
	return map[string]string{
		KeyAppEnv:                string(c.Env),
		KeyHTTPHost:              c.HTTP.Host,
		KeyHTTPPort:              strconv.Itoa(c.HTTP.Port),
		KeyHTTPReadTimeout:       c.HTTP.ReadTimeout.String(),
		KeyHTTPReadHeaderTimeout: c.HTTP.ReadHeaderTimeout.String(),
		KeyHTTPWriteTimeout:      c.HTTP.WriteTimeout.String(),
		KeyHTTPIdleTimeout:       c.HTTP.IdleTimeout.String(),
		KeyHTTPMaxBodyBytes:      strconv.FormatInt(c.HTTP.MaxRequestBodyBytes, 10),
		KeyHTTPShutdownGrace:     c.HTTP.ShutdownGracePeriod.String(),
		KeyLogLevel:              c.Log.Level,
		KeyLogFormat:             c.Log.Format,
		KeyDBPath:                c.DB.Path,
		KeyDBBusyTimeoutMS:       strconv.FormatInt(c.DB.BusyTimeout.Milliseconds(), 10),
		KeyDBMaxOpenConns:        strconv.Itoa(c.DB.MaxOpenConns),
		KeyLocalOrigin:           c.Auth.LocalOrigin,
		KeyAriaClientCallbacks:   strings.Join(c.Auth.AriaClientCallbacks, ","),
		KeyOwnerUsername:         c.Auth.OwnerUsername,
		KeyOwnerDisplayName:      c.Auth.OwnerDisplayName,
		KeyJobsWorkerID:          c.Jobs.WorkerID,
		KeyJobsPollInterval:      c.Jobs.PollInterval.String(),
		KeyJobsClaimBatchSize:    strconv.Itoa(c.Jobs.ClaimBatchSize),
		KeyJobsLeaseDuration:     c.Jobs.LeaseDuration.String(),
		KeyJobsLeaseRenewMargin:  c.Jobs.LeaseRenewMargin.String(),
		KeyJobsMaxAttempts:       strconv.Itoa(c.Jobs.MaxAttempts),
		KeyJobsBackoffBase:       c.Jobs.BackoffBase.String(),
		KeyJobsBackoffMax:        c.Jobs.BackoffMax.String(),
		KeyJobsMaxConcurrent:     strconv.Itoa(c.Jobs.MaxConcurrentJobs),
		KeyJobsShutdownGrace:     c.Jobs.ShutdownGracePeriod.String(),
		KeyLLMEnabled:            strconv.FormatBool(c.LLM.Enabled),
		KeyLLMBaseURL:            c.LLM.BaseURL,
		// LLM_API_KEY is a secret credential for a third-party endpoint:
		// only whether it is set is shown here.
		KeyLLMAPIKey:                                 redactedSetOrUnset(c.LLM.APIKey),
		KeyLLMModel:                                  c.LLM.Model,
		KeyLLMTimeout:                                c.LLM.Timeout.String(),
		KeyLLMMaxOutputTokens:                        strconv.Itoa(c.LLM.MaxOutputTokens),
		KeyLLMThreadContextMaxMessages:               strconv.Itoa(c.LLM.ThreadContextMaxMessages),
		KeyLLMThreadContextMaxChars:                  strconv.Itoa(c.LLM.ThreadContextMaxChars),
		KeyLLMClassificationEnabled:                  strconv.FormatBool(c.LLM.ClassificationEnabled),
		KeyLLMClassificationModel:                    c.LLM.ClassificationModel,
		KeyLLMClassificationMaxOutputTokens:          strconv.Itoa(c.LLM.ClassificationMaxOutputTokens),
		KeyLLMClassificationThreadContextMaxMessages: strconv.Itoa(c.LLM.ClassificationThreadContextMaxMessages),
		KeyLLMClassificationThreadContextMaxChars:    strconv.Itoa(c.LLM.ClassificationThreadContextMaxChars),
		KeyRSSEnabled:                                strconv.FormatBool(c.RSS.Enabled),
		KeyRSSFeedURLs:                               strings.Join(c.RSS.FeedURLs, ","),
		KeyRSSPollInterval:                           c.RSS.PollInterval.String(),
		KeyRSSFetchTimeout:                           c.RSS.FetchTimeout.String(),
		KeyRSSMaxResponseBytes:                       strconv.FormatInt(c.RSS.MaxResponseBytes, 10),
		KeyRSSMaxRedirects:                           strconv.Itoa(c.RSS.MaxRedirects),
		KeyRSSSummaryMaxChars:                        strconv.Itoa(c.RSS.SummaryMaxChars),
		KeyRSSAllowInsecureHTTP:                      strconv.FormatBool(c.RSS.AllowInsecureHTTP),
	}
}

func redactedSetOrUnset(value string) string {
	if value == "" {
		return "<unset>"
	}
	return "<set>"
}

func parseRequiredEnum(values map[string]string, key string, allowed []string, errs *[]FieldError) string {
	v, ok := values[key]
	if !ok || v == "" {
		*errs = append(*errs, FieldError{Key: key, Reason: "required"})
		return ""
	}
	if !slices.Contains(allowed, v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be one of " + strings.Join(allowed, ", ")})
		return ""
	}
	return v
}

func parseOptionalEnum(values map[string]string, key, def string, allowed []string, errs *[]FieldError) string {
	v, ok := values[key]
	if !ok || v == "" {
		return def
	}
	if !slices.Contains(allowed, v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be one of " + strings.Join(allowed, ", ")})
		return def
	}
	return v
}

func parseOptionalString(values map[string]string, key, def string) string {
	if v, ok := values[key]; ok && v != "" {
		return v
	}
	return def
}

func parseOptionalInt(values map[string]string, key string, def, min, max int, errs *[]FieldError) int {
	v, ok := values[key]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, FieldError{Key: key, Reason: fmt.Sprintf("must be an integer between %d and %d", min, max)})
		return def
	}
	if !validateIntBounds(errs, key, n, min, max) {
		return def
	}
	return n
}

func parseOptionalInt64(values map[string]string, key string, def, min int64, errs *[]FieldError) int64 {
	v, ok := values[key]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		*errs = append(*errs, FieldError{Key: key, Reason: fmt.Sprintf("must be an integer of at least %d", min)})
		return def
	}
	if !validateInt64Min(errs, key, n, min) {
		return def
	}
	return n
}

func parseOptionalBool(values map[string]string, key string, def bool, errs *[]FieldError) bool {
	v, ok := values[key]
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be a boolean (true/false)"})
		return def
	}
	return b
}

func parseOptionalDuration(values map[string]string, key string, def time.Duration, errs *[]FieldError) time.Duration {
	v, ok := values[key]
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be a positive duration (e.g. 5s)"})
		return def
	}
	if !validatePositiveDuration(errs, key, d) {
		return def
	}
	return d
}

// validateIntBounds, validateInt64Min, and validatePositiveDuration are the
// shared bound checks used both while parsing raw config-file/env-var
// strings (parseOptionalInt, ...) and by Validate when re-checking an
// already-typed, hand-built Config, so the two paths cannot drift apart.

func validateIntBounds(errs *[]FieldError, key string, n, min, max int) bool {
	if n < min || n > max {
		*errs = append(*errs, FieldError{Key: key, Reason: fmt.Sprintf("must be an integer between %d and %d", min, max)})
		return false
	}
	return true
}

func validateInt64Min(errs *[]FieldError, key string, n, min int64) bool {
	if n < min {
		*errs = append(*errs, FieldError{Key: key, Reason: fmt.Sprintf("must be an integer of at least %d", min)})
		return false
	}
	return true
}

func validatePositiveDuration(errs *[]FieldError, key string, d time.Duration) bool {
	if d <= 0 {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be a positive duration (e.g. 5s)"})
		return false
	}
	return true
}

// validateOrigin backs LOCAL_ORIGIN. It is required in every environment
// (there is no safe default redirect
// target), must be an absolute URL naming only a scheme and a host (no
// userinfo, path beyond "" or "/", query, or fragment — ADR-0002 fixes
// these as origins, never paths), and must be https in production.
func validateOrigin(errs *[]FieldError, key, v string, env Environment) bool {
	if v == "" {
		*errs = append(*errs, FieldError{Key: key, Reason: "required"})
		return false
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be an absolute http(s) origin URL"})
		return false
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		*errs = append(*errs, FieldError{Key: key, Reason: "must contain only a scheme and a host, no userinfo, path, query, or fragment"})
		return false
	}
	if env == EnvProduction && u.Scheme != "https" {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be https in production"})
		return false
	}
	return true
}

// validateLLMBaseURL checks LLM_BASE_URL when the LLM feature is enabled.
// Unlike validateOrigin (LOCAL_ORIGIN), a path is expected
// and allowed here: OpenAI-compatible base URLs commonly include one (for
// example "https://api.openai.com/v1"), so only the scheme and host are
// constrained, not the path/query.
func validateLLMBaseURL(errs *[]FieldError, key, v string, env Environment) bool {
	if v == "" {
		*errs = append(*errs, FieldError{Key: key, Reason: "required when " + KeyLLMEnabled + "=true"})
		return false
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be an absolute http(s) URL"})
		return false
	}
	if env == EnvProduction && u.Scheme != "https" {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be https in production"})
		return false
	}
	return true
}

// parseOptionalCallbackList splits ARIA_CLIENT_CALLBACKS at commas that
// introduce another absolute URL, while retaining commas inside a URL's
// path or query. It trims whitespace around each entry and validates the
// result. An unset or empty value yields nil: no client callback is
// accepted.
func parseOptionalCallbackList(values map[string]string, key string, errs *[]FieldError) []string {
	v, ok := values[key]
	if !ok || v == "" {
		return nil
	}
	parts := splitCallbackList(v)
	list := make([]string, len(parts))
	for i, p := range parts {
		list[i] = strings.TrimSpace(p)
	}
	validateCallbackEntries(errs, key, list)
	return list
}

var callbackSchemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func splitCallbackList(v string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(v); i++ {
		if v[i] != ',' {
			continue
		}
		remainder := strings.TrimSpace(v[i+1:])
		if remainder == "" || remainder[0] == ',' || callbackSchemePrefix.MatchString(remainder) {
			parts = append(parts, v[start:i])
			start = i + 1
		}
	}
	return append(parts, v[start:])
}

// validateCallbackEntries checks each ARIA_CLIENT_CALLBACKS entry is a
// URL with a non-empty scheme. Unlike validateOrigin, a non-HTTPS scheme
// (Aria's aria://aria/miauth deep link) is explicitly allowed here: these
// are exact-match client return destinations.
func validateCallbackEntries(errs *[]FieldError, key string, list []string) bool {
	ok := true
	for _, p := range list {
		if p == "" {
			*errs = append(*errs, FieldError{Key: key, Reason: "must not contain an empty entry"})
			ok = false
			continue
		}
		u, err := url.Parse(p)
		if err != nil || u.Scheme == "" {
			*errs = append(*errs, FieldError{Key: key, Reason: "each entry must be a URL with a non-empty scheme"})
			ok = false
		}
	}
	return ok
}

// splitOptionalURLList splits RSS_FEED_URLS the same way
// parseOptionalCallbackList splits ARIA_CLIENT_CALLBACKS (commas inside
// a URL's own path or query are retained; a separator is a comma
// followed by the next absolute URL scheme), but performs no format
// validation itself: unlike ARIA_CLIENT_CALLBACKS (always validated,
// independent of any feature flag), RSS_FEED_URLS is only required and
// checked when RSS_ENABLED=true, so validateRSSFeedURLs is called
// separately from Validate, gated by that flag — the same "parse now,
// validate only if enabled" split LLM_BASE_URL uses. An unset or empty
// value yields nil: no feed is polled.
func splitOptionalURLList(values map[string]string, key string) []string {
	v, ok := values[key]
	if !ok || v == "" {
		return nil
	}
	parts := splitCallbackList(v)
	list := make([]string, len(parts))
	for i, p := range parts {
		list[i] = strings.TrimSpace(p)
	}
	return list
}

// validateRSSFeedURLs checks each RSS_FEED_URLS entry is an absolute
// http(s) URL, and that an "http" entry is only present when
// allowInsecureHTTP (RSS_ALLOW_INSECURE_HTTP) is true.
func validateRSSFeedURLs(errs *[]FieldError, key string, list []string, allowInsecureHTTP bool) bool {
	ok := true
	for _, p := range list {
		u, err := url.Parse(p)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			*errs = append(*errs, FieldError{Key: key, Reason: "each entry must be an absolute http(s) URL"})
			ok = false
			continue
		}
		if u.Scheme == "http" && !allowInsecureHTTP {
			*errs = append(*errs, FieldError{Key: key, Reason: "http entries require " + KeyRSSAllowInsecureHTTP + "=true"})
			ok = false
		}
	}
	return ok
}

// ownerUsernamePattern mirrors Misskey's own username character set
// closely enough for this service's purposes: non-empty, ASCII letters,
// digits, and underscores only.
var ownerUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func validateOwnerUsername(errs *[]FieldError, key, v string) bool {
	if !ownerUsernamePattern.MatchString(v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be a non-empty string of ASCII letters, digits, and underscores"})
		return false
	}
	return true
}
