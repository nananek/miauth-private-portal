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
	IMAP IMAPConfig
	// OpenWebUI configures Issue #52's Open WebUI registry and identity
	// projection. Like LLM/RSS/IMAP it is off by default.
	OpenWebUI OpenWebUIConfig
	// Drive configures Issue #77 PR1's Drive storage foundation. Unlike
	// LLM/RSS/IMAP/OpenWebUI it has no Enabled flag: Backend's own value
	// ("localdisk" or "s3compat") is always meaningful, so there is no
	// third "off" state to represent.
	Drive DriveConfig
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
	// reports for the local owner actor. Unlike OwnerDisplayName, this
	// remains the permanent source of truth: Issue #23 PR1's source
	// trace (docs/compat/aria-v1.5.11.md's "POST /api/i/update" section)
	// found that neither Aria nor the pinned misskey_dart client has any
	// way to send a username field, so self-service username editing was
	// never added.
	OwnerUsername string
	// OwnerDisplayName is the optional display name reported as the
	// owner's UserDetailedNotMe.name. Empty means null (unset), matching
	// Misskey's own nullable name field. Since Issue #23 PR1, this is
	// only the initial value copied into the actors.display_name column
	// the first time the owner actor is created; POST /api/i/update
	// changes the DB value from then on, and this config value is never
	// consulted again.
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

// IMAPConfig configures Issue #12's read-only IMAP mail ingestion. Enabled
// defaults to false: no source is ever seeded, no adapter/scheduler is
// constructed, and cmd/mailfetch's socket is never dialed until an
// operator explicitly turns this on — the same safe-default shape as
// RSSConfig.Enabled. Unlike RSS, actually fetching a message requires
// cmd/mailfetch (see docs/decisions/0003-imap-mailfetch-isolation.md) to
// be running and reachable at MailfetchSocket; internal/ingest/imap never
// imports an IMAP or MIME library itself.
type IMAPConfig struct {
	// Enabled gates the ingestion scheduler, adapter, and job handler
	// entirely. False is the safe default.
	Enabled bool
	// Host and Port name the IMAP server. Seeded into the single
	// domain.ExternalSource this config produces (kind "imap").
	Host string
	Port int
	// TLSMode is "implicit" (TLS from the first byte, conventionally port
	// 993) or "starttls" (plaintext CAPABILITY/STARTTLS negotiation before
	// LOGIN, conventionally port 143). There is deliberately no plaintext
	// option: AGENTS.md requires IMAP credentials never cross the network
	// unencrypted.
	TLSMode string
	// Username and Password authenticate to the IMAP server. Sent to
	// cmd/mailfetch only in the per-request RPC payload over
	// MailfetchSocket, never as a command-line argument or logged value;
	// see Redacted.
	Username string
	Password string
	// Mailbox is EXAMINE'd (never SELECT'd: this service never marks,
	// moves, or deletes mail). Defaults to "INBOX".
	Mailbox string
	// PollInterval is how often the mailbox is re-fetched.
	PollInterval time.Duration
	// FetchTimeout bounds a single fetch's IMAP round trip (connect
	// through LOGOUT); must be less than PollInterval.
	FetchTimeout time.Duration
	// MaxMessageBytes bounds how much of a single message's body
	// cmd/mailfetch reads (the BODY.PEEK<0,N> upper bound N); a larger
	// body is truncated at this limit before sanitization.
	MaxMessageBytes int64
	// SnippetMaxChars bounds the plain-text snippet stored per message
	// after HTML sanitization, mirroring RSSConfig.SummaryMaxChars.
	SnippetMaxChars int
	// StoreFullBody additionally stores a longer plain-text body (bounded
	// by FullBodyMaxChars) instead of only SnippetMaxChars. False is the
	// safe, storage-minimizing default.
	StoreFullBody    bool
	FullBodyMaxChars int
	// MailfetchSocket is the Unix domain socket path cmd/mailfetch
	// listens on and internal/ingest/imap dials.
	MailfetchSocket string
}

// OpenWebUIConfig configures Issue #52's Open WebUI registry and
// VirtualActor projection. Enabled defaults to false, the same safe
// default LLMConfig/RSSConfig/IMAPConfig use: with it off, no registry
// row is seeded, no VirtualActor is projected, and every Note this
// service returns is byte-for-byte what it was before the feature
// existed.
//
// This is the whole configuration surface for the feature. There is no
// HTTP endpoint and no CLI for changing a workspace or a model: the
// values below are the owner-only path, in exactly the sense ADR-0002
// means it — only someone with host access can edit them (see
// docs/operations/configuration.md's Open WebUI section).
type OpenWebUIConfig struct {
	// Enabled gates registry seeding and the VirtualActor projection
	// entirely. False is the safe default.
	Enabled bool
	// BaseURL is the Open WebUI instance's HTTPS origin (scheme and host
	// only). It must appear verbatim in AllowedOrigins: the allowlist is
	// the boundary, and the configured target is checked against it
	// rather than being trusted for being configured (ADR-0005 D11).
	//
	// Unlike LOCAL_ORIGIN and LLM_BASE_URL, https is required in every
	// environment, not only production. A development deployment
	// pointing at a plaintext instance would send the API key in the
	// clear over whatever network sits between them.
	BaseURL string
	// AllowedOrigins is the fixed HTTPS origin allowlist. A tailnet
	// origin belongs here only if an operator lists it explicitly;
	// nothing is inferred.
	AllowedOrigins []string
	// APIKey authenticates against BaseURL (ADR-0005 D10). Never logged
	// or returned to a client; see Redacted. What the database stores is
	// the *name* of this key, never its value.
	APIKey string
	// WorkspaceName is the workspace's display name.
	WorkspaceName string
	// DefaultModelID is the provider's own opaque model id. It is used
	// verbatim apart from trimming surrounding whitespace: ADR-0005 D9
	// makes it opaque, so nothing here lowercases, splits, or otherwise
	// reshapes it.
	DefaultModelID string
	// PresentationHost is the host half of a VirtualActor's
	// @<slug>@<presentation host> handle. It is a fixed
	// deployment-provisioned value, never inferred from BaseURL, and it
	// must differ from LOCAL_ORIGIN's host: a UserLite whose host is
	// null means "local to this service", so reusing the local host for
	// a remote-presented actor would make the two indistinguishable.
	PresentationHost string
	// CatalogSyncInterval mirrors OPENWEBUI_CATALOG_SYNC_INTERVAL (Issue
	// #75): how often Registry.SyncCatalog re-lists GET /api/models and
	// reconciles the registry, independent of GenerationEnabled — catalog
	// sync keeps the VirtualActor projection and search results in step
	// with the provider's own model list even on a deployment that never
	// turns outbound generation on.
	CatalogSyncInterval time.Duration

	// The fields below are Issue #53's (OWUI-B) client-side bounds and
	// generation gate. They are parsed and validated from this PR
	// (Issue #53 PR1) on, but nothing in this service reads them yet: no
	// bridge, job, or provider adapter exists until Issue #53's later
	// PRs build one.

	// GenerationEnabled gates outbound generation specifically,
	// independent of Enabled, the same "sub-flag" shape
	// LLMConfig.ClassificationEnabled uses relative to LLMConfig.Enabled.
	// It is meaningless while Enabled is false and is never validated or
	// required in that case: a disabled deployment must not fail startup
	// over a generation setting it will never read.
	GenerationEnabled bool
	// Timeout bounds a single HTTP call Issue #53's adapter makes to
	// BaseURL. Buffered (non-streaming) generation can run considerably
	// longer than LLMConfig.Timeout's default, hence the larger default
	// here.
	Timeout time.Duration
	// MaxResponseBytes bounds how much of a single response the adapter
	// reads into memory. GET /api/v1/chats/{id} returns the whole chat,
	// not just one message, so this is deliberately larger than
	// LLMConfig's analogous bound.
	MaxResponseBytes int64
	// MaxRequestBytes bounds the outbound request body size. Exceeding
	// it fails the turn closed rather than truncating the conversation
	// context silently sent to the model.
	MaxRequestBytes int64
	// MaxContextMessages bounds how many prior-turn messages (including
	// the new one) a single request may carry, independent of
	// MaxRequestBytes: a byte bound alone would let a thread of many
	// short messages slip through uncapped.
	MaxContextMessages int
	// WebSearchEnabled mirrors OPENWEBUI_WEB_SEARCH_ENABLED. Tri-state
	// since ADR-0005 D21 (Issue #75 AC#11): nil means unset — a turn's
	// web_search feature then follows the selected model's own synced
	// defaultFeatureIds instead — while a non-nil value overrides every
	// model uniformly, on or off. Independent of GenerationEnabled at the
	// type level, but meaningless (never read) while GenerationEnabled is
	// false, the same relationship Enabled/GenerationEnabled already have.
	WebSearchEnabled *bool
	// ViewerBaseURL mirrors OPENWEBUI_VIEWER_BASE_URL (Issues #81+#84,
	// ADR-0005 D23): a browser-reachable origin for the same instance,
	// used only to render an owner-facing "view in Open WebUI" link into
	// a generated reply's own text. Empty (the default) disables the
	// link and, independently, gates whether this deployment ever asks
	// the provider for title generation at all (see
	// internal/openwebui.TurnJobConfig.ViewerBaseURL) — leaving it unset
	// reproduces pre-#84 behavior exactly. Unlike BaseURL, it is never
	// required to appear in AllowedOrigins: this server never dials it
	// (D11's SSRF allowlist policy governs connections this server
	// makes, and this value is display-only), so validation checks only
	// its shape.
	ViewerBaseURL string
}

// DriveConfig configures Issue #77 PR1's Drive storage foundation
// (ADR-0006): a single object-storage backend, selected for this
// deployment's whole lifetime, and the raster-image validation bounds
// every upload must satisfy. Nothing reads through it yet — it exists so
// PR3/PR4/PR5/PR6 have a validated configuration surface to build
// against, the same "config before its first reader" precedent Issue
// #53's OpenWebUIConfig fields set.
type DriveConfig struct {
	// Backend selects the internal/drive.Storage implementation:
	// "localdisk" (the default) or "s3compat". A deployment picks
	// exactly one; there is no per-file or per-request backend switch
	// and no migration path between them.
	Backend string
	// DataDir is the localdisk backend's root directory, defaulting to
	// "./data/drive" (matching DBConfig.Path's own "./data/..." default).
	// It must not be empty when Backend is "localdisk"; whether it
	// exists on disk is internal/drive.Local's concern at first use, not
	// this package's — the same "config validates shape, the consumer
	// validates reachability" split DBConfig.Path already has.
	DataDir string
	// S3Endpoint, S3Bucket, S3AccessKeyID, and S3SecretAccessKey
	// configure the s3compat backend (AWS S3 or a self-hosted
	// S3-compatible store such as MinIO). Required only when Backend is
	// "s3compat". S3SecretAccessKey is a credential: never logged or
	// returned to a client, see Redacted.
	//
	// These arrive as plain configured values, not a secret_ref
	// indirection — internal/openwebui/registry.go's secret_ref pattern
	// (the database stores only a configuration key's *name*) applies to
	// a value some other row in this database points at; nothing in this
	// PR persists Drive configuration to a database row for a secret_ref
	// to name. If a future PR adds a database-stored Drive setting that
	// needs a credential, it should reuse the same secret_ref
	// indirection rather than storing S3SecretAccessKey's value again.
	S3Endpoint        string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	// S3UseSSL selects https (true, the default) or http against
	// S3Endpoint.
	S3UseSSL bool
	// S3Region is passed to the S3 client when non-empty; most
	// S3-compatible servers (MinIO included) do not require it.
	S3Region string
	// MaxFileBytes bounds any single uploaded file, image or not.
	MaxFileBytes int64
	// MaxImageWidth and MaxImageHeight bound a raster image's decoded
	// pixel dimensions (internal/drive.ValidateImage), independent of
	// MaxFileBytes: a small but pathologically large-dimension image
	// (a "decompression bomb") is rejected by this check even when it
	// fits comfortably under the byte-size bound.
	MaxImageWidth  int
	MaxImageHeight int
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

	imapPortMin, imapPortMax                         = 1, 65535
	imapMaxMessageBytesMin                           = 1
	imapSnippetMaxCharsMin, imapSnippetMaxCharsMax   = 1, 100_000
	imapFullBodyMaxCharsMin, imapFullBodyMaxCharsMax = 1, 1_000_000

	// openWebUIMaxResponseBytesMin is higher than RSS/IMAP's analogous
	// floors: GET /api/v1/chats/{id} returns the whole chat, so a bound
	// too small to hold even a short conversation is not a usable
	// setting to allow at all.
	openWebUIMaxResponseBytesMin                                   = 65_536
	openWebUIMaxRequestBytesMin                                    = 1
	openWebUIMaxContextMessagesMin, openWebUIMaxContextMessagesMax = 1, 1000

	driveMaxFileBytesMin                                 = 1
	driveMaxImageDimensionMin, driveMaxImageDimensionMax = 1, 100_000
)

// imapAllowedTLSModes is the single source of truth for IMAP_TLS_MODE:
// deliberately just these two values, never a plaintext option (AGENTS.md:
// "IMAP is read-only by default and must not mark, move, or delete mail"
// sits alongside the broader rule that credentials never cross the network
// unencrypted).
var imapAllowedTLSModes = []string{"implicit", "starttls"}

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

	cfg.Drive.Backend = parseOptionalEnum(values, KeyDriveBackend, "localdisk", []string{"localdisk", "s3compat"}, &errs)
	cfg.Drive.DataDir = parseOptionalString(values, KeyDriveDataDir, "./data/drive")
	cfg.Drive.S3Endpoint = parseOptionalString(values, KeyDriveS3Endpoint, "")
	cfg.Drive.S3Bucket = parseOptionalString(values, KeyDriveS3Bucket, "")
	cfg.Drive.S3AccessKeyID = parseOptionalString(values, KeyDriveS3AccessKeyID, "")
	cfg.Drive.S3SecretAccessKey = parseOptionalString(values, KeyDriveS3SecretAccessKey, "")
	cfg.Drive.S3UseSSL = parseOptionalBool(values, KeyDriveS3UseSSL, true, &errs)
	cfg.Drive.S3Region = parseOptionalString(values, KeyDriveS3Region, "")
	cfg.Drive.MaxFileBytes = parseOptionalInt64(values, KeyDriveMaxFileBytes, 10_485_760, driveMaxFileBytesMin, &errs)
	cfg.Drive.MaxImageWidth = parseOptionalInt(values, KeyDriveMaxImageWidth, 8000, driveMaxImageDimensionMin, driveMaxImageDimensionMax, &errs)
	cfg.Drive.MaxImageHeight = parseOptionalInt(values, KeyDriveMaxImageHeight, 8000, driveMaxImageDimensionMin, driveMaxImageDimensionMax, &errs)

	cfg.IMAP.Enabled = parseOptionalBool(values, KeyIMAPEnabled, false, &errs)
	cfg.IMAP.Host = parseOptionalString(values, KeyIMAPHost, "")
	cfg.IMAP.Port = parseOptionalInt(values, KeyIMAPPort, 993, imapPortMin, imapPortMax, &errs)
	cfg.IMAP.TLSMode = parseOptionalString(values, KeyIMAPTLSMode, "implicit")
	cfg.IMAP.Username = parseOptionalString(values, KeyIMAPUsername, "")
	cfg.IMAP.Password = parseOptionalString(values, KeyIMAPPassword, "")
	cfg.IMAP.Mailbox = parseOptionalString(values, KeyIMAPMailbox, "INBOX")
	cfg.IMAP.PollInterval = parseOptionalDuration(values, KeyIMAPPollInterval, 5*time.Minute, &errs)
	cfg.IMAP.FetchTimeout = parseOptionalDuration(values, KeyIMAPFetchTimeout, 30*time.Second, &errs)
	cfg.IMAP.MaxMessageBytes = parseOptionalInt64(values, KeyIMAPMaxMessageBytes, 1_048_576, imapMaxMessageBytesMin, &errs)
	cfg.IMAP.SnippetMaxChars = parseOptionalInt(values, KeyIMAPSnippetMaxChars, 2000, imapSnippetMaxCharsMin, imapSnippetMaxCharsMax, &errs)
	cfg.IMAP.StoreFullBody = parseOptionalBool(values, KeyIMAPStoreFullBody, false, &errs)
	cfg.IMAP.FullBodyMaxChars = parseOptionalInt(values, KeyIMAPFullBodyMaxChars, 20_000, imapFullBodyMaxCharsMin, imapFullBodyMaxCharsMax, &errs)
	cfg.IMAP.MailfetchSocket = parseOptionalString(values, KeyIMAPMailfetchSocket, "/run/mailfetch/mailfetch.sock")

	cfg.OpenWebUI.Enabled = parseOptionalBool(values, KeyOpenWebUIEnabled, false, &errs)
	cfg.OpenWebUI.BaseURL = strings.TrimRight(parseOptionalString(values, KeyOpenWebUIBaseURL, ""), "/")
	// Each entry is right-trimmed the same way BaseURL is, so
	// "https://x.example.net/" and "https://x.example.net" name the same
	// allowlist entry: validateOpenWebUIBaseURL's exact-match membership
	// check would otherwise reject a base URL and an origin that a human
	// would read as identical.
	cfg.OpenWebUI.AllowedOrigins = trimRightEach(splitOptionalURLList(values, KeyOpenWebUIAllowedOrigins), "/")
	cfg.OpenWebUI.APIKey = parseOptionalString(values, KeyOpenWebUIAPIKey, "")
	cfg.OpenWebUI.WorkspaceName = parseOptionalString(values, KeyOpenWebUIWorkspaceName, "Open WebUI")
	// Trimmed but otherwise untouched: the provider's model id is opaque
	// (ADR-0005 D9), so this must not case-fold or otherwise reshape it.
	cfg.OpenWebUI.DefaultModelID = strings.TrimSpace(parseOptionalString(values, KeyOpenWebUIDefaultModelID, ""))
	cfg.OpenWebUI.PresentationHost = parseOptionalString(values, KeyOpenWebUIPresentationHost, "")
	cfg.OpenWebUI.CatalogSyncInterval = parseOptionalDuration(values, KeyOpenWebUICatalogSyncInterval, 10*time.Minute, &errs)
	cfg.OpenWebUI.GenerationEnabled = parseOptionalBool(values, KeyOpenWebUIGenerationEnabled, false, &errs)
	cfg.OpenWebUI.Timeout = parseOptionalDuration(values, KeyOpenWebUITimeout, 120*time.Second, &errs)
	cfg.OpenWebUI.MaxResponseBytes = parseOptionalInt64(values, KeyOpenWebUIMaxResponseBytes, 4_194_304, openWebUIMaxResponseBytesMin, &errs)
	cfg.OpenWebUI.MaxRequestBytes = parseOptionalInt64(values, KeyOpenWebUIMaxRequestBytes, 1_048_576, openWebUIMaxRequestBytesMin, &errs)
	cfg.OpenWebUI.MaxContextMessages = parseOptionalInt(values, KeyOpenWebUIMaxContextMessages, 100, openWebUIMaxContextMessagesMin, openWebUIMaxContextMessagesMax, &errs)
	cfg.OpenWebUI.WebSearchEnabled = parseOptionalBoolPtr(values, KeyOpenWebUIWebSearchEnabled, &errs)
	cfg.OpenWebUI.ViewerBaseURL = strings.TrimRight(parseOptionalString(values, KeyOpenWebUIViewerBaseURL, ""), "/")

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

	// Drive has no Enabled flag (DriveConfig's doc comment) — Backend
	// always selects one of the two branches below, so exactly one of
	// them is always validated, unlike RSS/IMAP/OpenWebUI's "skip
	// everything while disabled" shape.
	switch c.Drive.Backend {
	case "localdisk":
		if c.Drive.DataDir == "" {
			errs = append(errs, FieldError{Key: KeyDriveDataDir, Reason: "required when " + KeyDriveBackend + "=localdisk"})
		}
	case "s3compat":
		if c.Drive.S3Endpoint == "" {
			errs = append(errs, FieldError{Key: KeyDriveS3Endpoint, Reason: "required when " + KeyDriveBackend + "=s3compat"})
		}
		if c.Drive.S3Bucket == "" {
			errs = append(errs, FieldError{Key: KeyDriveS3Bucket, Reason: "required when " + KeyDriveBackend + "=s3compat"})
		}
		if c.Drive.S3AccessKeyID == "" {
			errs = append(errs, FieldError{Key: KeyDriveS3AccessKeyID, Reason: "required when " + KeyDriveBackend + "=s3compat"})
		}
		if c.Drive.S3SecretAccessKey == "" {
			errs = append(errs, FieldError{Key: KeyDriveS3SecretAccessKey, Reason: "required when " + KeyDriveBackend + "=s3compat"})
		}
	}
	validateInt64Min(&errs, KeyDriveMaxFileBytes, c.Drive.MaxFileBytes, driveMaxFileBytesMin)
	validateIntBounds(&errs, KeyDriveMaxImageWidth, c.Drive.MaxImageWidth, driveMaxImageDimensionMin, driveMaxImageDimensionMax)
	validateIntBounds(&errs, KeyDriveMaxImageHeight, c.Drive.MaxImageHeight, driveMaxImageDimensionMin, driveMaxImageDimensionMax)

	// IMAP fields are only required/bound-checked when the feature is
	// actually enabled: IMAP_ENABLED defaults to false, and a disabled
	// deployment must not fail startup over an unset IMAP setting it will
	// never use.
	if c.IMAP.Enabled {
		if c.IMAP.Host == "" {
			errs = append(errs, FieldError{Key: KeyIMAPHost, Reason: "required when " + KeyIMAPEnabled + "=true"})
		}
		if c.IMAP.Username == "" {
			errs = append(errs, FieldError{Key: KeyIMAPUsername, Reason: "required when " + KeyIMAPEnabled + "=true"})
		}
		if c.IMAP.Password == "" {
			errs = append(errs, FieldError{Key: KeyIMAPPassword, Reason: "required when " + KeyIMAPEnabled + "=true"})
		}
		if c.IMAP.Mailbox == "" {
			errs = append(errs, FieldError{Key: KeyIMAPMailbox, Reason: "must not be empty"})
		}
		if !slices.Contains(imapAllowedTLSModes, c.IMAP.TLSMode) {
			errs = append(errs, FieldError{Key: KeyIMAPTLSMode, Reason: "must be one of " + strings.Join(imapAllowedTLSModes, ", ")})
		}
		if c.IMAP.MailfetchSocket == "" {
			errs = append(errs, FieldError{Key: KeyIMAPMailfetchSocket, Reason: "must not be empty"})
		}
		validateIntBounds(&errs, KeyIMAPPort, c.IMAP.Port, imapPortMin, imapPortMax)
		validatePositiveDuration(&errs, KeyIMAPPollInterval, c.IMAP.PollInterval)
		validatePositiveDuration(&errs, KeyIMAPFetchTimeout, c.IMAP.FetchTimeout)
		if c.IMAP.FetchTimeout > 0 && c.IMAP.PollInterval > 0 && c.IMAP.FetchTimeout >= c.IMAP.PollInterval {
			errs = append(errs, FieldError{Key: KeyIMAPFetchTimeout, Reason: "must be less than " + KeyIMAPPollInterval})
		}
		validateInt64Min(&errs, KeyIMAPMaxMessageBytes, c.IMAP.MaxMessageBytes, imapMaxMessageBytesMin)
		validateIntBounds(&errs, KeyIMAPSnippetMaxChars, c.IMAP.SnippetMaxChars, imapSnippetMaxCharsMin, imapSnippetMaxCharsMax)
		validateIntBounds(&errs, KeyIMAPFullBodyMaxChars, c.IMAP.FullBodyMaxChars, imapFullBodyMaxCharsMin, imapFullBodyMaxCharsMax)
	}

	// Open WebUI fields are only required/checked when the feature is
	// actually enabled, the same shape LLM/RSS/IMAP use: OPENWEBUI_ENABLED
	// defaults to false, and a disabled deployment must not fail startup
	// over a setting it will never read.
	if c.OpenWebUI.Enabled {
		validateOpenWebUIOrigins(&errs, KeyOpenWebUIAllowedOrigins, c.OpenWebUI.AllowedOrigins)
		validateOpenWebUIBaseURL(&errs, KeyOpenWebUIBaseURL, c.OpenWebUI.BaseURL, c.OpenWebUI.AllowedOrigins)
		if c.OpenWebUI.APIKey == "" {
			errs = append(errs, FieldError{Key: KeyOpenWebUIAPIKey, Reason: "required when " + KeyOpenWebUIEnabled + "=true"})
		}
		if c.OpenWebUI.WorkspaceName == "" {
			errs = append(errs, FieldError{Key: KeyOpenWebUIWorkspaceName, Reason: "must not be empty"})
		}
		if c.OpenWebUI.DefaultModelID == "" {
			errs = append(errs, FieldError{Key: KeyOpenWebUIDefaultModelID, Reason: "required when " + KeyOpenWebUIEnabled + "=true"})
		}
		validateOpenWebUIPresentationHost(&errs, KeyOpenWebUIPresentationHost, c.OpenWebUI.PresentationHost, c.Auth.LocalOrigin)
		validatePositiveDuration(&errs, KeyOpenWebUICatalogSyncInterval, c.OpenWebUI.CatalogSyncInterval)
		validatePositiveDuration(&errs, KeyOpenWebUITimeout, c.OpenWebUI.Timeout)
		validateInt64Min(&errs, KeyOpenWebUIMaxResponseBytes, c.OpenWebUI.MaxResponseBytes, openWebUIMaxResponseBytesMin)
		validateInt64Min(&errs, KeyOpenWebUIMaxRequestBytes, c.OpenWebUI.MaxRequestBytes, openWebUIMaxRequestBytesMin)
		validateIntBounds(&errs, KeyOpenWebUIMaxContextMessages, c.OpenWebUI.MaxContextMessages, openWebUIMaxContextMessagesMin, openWebUIMaxContextMessagesMax)
		validateOpenWebUIViewerBaseURL(&errs, KeyOpenWebUIViewerBaseURL, c.OpenWebUI.ViewerBaseURL)
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
		KeyDriveBackend:                              c.Drive.Backend,
		KeyDriveDataDir:                              c.Drive.DataDir,
		KeyDriveS3Endpoint:                           c.Drive.S3Endpoint,
		KeyDriveS3Bucket:                             c.Drive.S3Bucket,
		// DRIVE_S3_ACCESS_KEY_ID/DRIVE_S3_SECRET_ACCESS_KEY are S3
		// credentials: only whether each is set is shown here, matching
		// LLM_API_KEY's treatment.
		KeyDriveS3AccessKeyID:     redactedSetOrUnset(c.Drive.S3AccessKeyID),
		KeyDriveS3SecretAccessKey: redactedSetOrUnset(c.Drive.S3SecretAccessKey),
		KeyDriveS3UseSSL:          strconv.FormatBool(c.Drive.S3UseSSL),
		KeyDriveS3Region:          c.Drive.S3Region,
		KeyDriveMaxFileBytes:      strconv.FormatInt(c.Drive.MaxFileBytes, 10),
		KeyDriveMaxImageWidth:     strconv.Itoa(c.Drive.MaxImageWidth),
		KeyDriveMaxImageHeight:    strconv.Itoa(c.Drive.MaxImageHeight),
		KeyIMAPEnabled:            strconv.FormatBool(c.IMAP.Enabled),
		KeyIMAPHost:               c.IMAP.Host,
		KeyIMAPPort:               strconv.Itoa(c.IMAP.Port),
		KeyIMAPTLSMode:            c.IMAP.TLSMode,
		// IMAP_USERNAME can be a personal email address; only whether it
		// is set is shown here, matching LLM_API_KEY's treatment.
		KeyIMAPUsername:         redactedSetOrUnset(c.IMAP.Username),
		KeyIMAPPassword:         redactedSetOrUnset(c.IMAP.Password),
		KeyIMAPMailbox:          c.IMAP.Mailbox,
		KeyIMAPPollInterval:     c.IMAP.PollInterval.String(),
		KeyIMAPFetchTimeout:     c.IMAP.FetchTimeout.String(),
		KeyIMAPMaxMessageBytes:  strconv.FormatInt(c.IMAP.MaxMessageBytes, 10),
		KeyIMAPSnippetMaxChars:  strconv.Itoa(c.IMAP.SnippetMaxChars),
		KeyIMAPStoreFullBody:    strconv.FormatBool(c.IMAP.StoreFullBody),
		KeyIMAPFullBodyMaxChars: strconv.Itoa(c.IMAP.FullBodyMaxChars),
		KeyIMAPMailfetchSocket:  c.IMAP.MailfetchSocket,

		KeyOpenWebUIEnabled:        strconv.FormatBool(c.OpenWebUI.Enabled),
		KeyOpenWebUIBaseURL:        c.OpenWebUI.BaseURL,
		KeyOpenWebUIAllowedOrigins: strings.Join(c.OpenWebUI.AllowedOrigins, ","),
		// OPENWEBUI_API_KEY is a secret credential for a third-party
		// endpoint, treated exactly like LLM_API_KEY: only whether it is
		// set is shown. The database stores this key's *name* as a
		// workspace's secret_ref (ADR-0005 D10), never the value shown
		// here as <set>.
		KeyOpenWebUIAPIKey:              redactedSetOrUnset(c.OpenWebUI.APIKey),
		KeyOpenWebUIWorkspaceName:       c.OpenWebUI.WorkspaceName,
		KeyOpenWebUIDefaultModelID:      c.OpenWebUI.DefaultModelID,
		KeyOpenWebUIPresentationHost:    c.OpenWebUI.PresentationHost,
		KeyOpenWebUICatalogSyncInterval: c.OpenWebUI.CatalogSyncInterval.String(),

		KeyOpenWebUIGenerationEnabled:  strconv.FormatBool(c.OpenWebUI.GenerationEnabled),
		KeyOpenWebUITimeout:            c.OpenWebUI.Timeout.String(),
		KeyOpenWebUIMaxResponseBytes:   strconv.FormatInt(c.OpenWebUI.MaxResponseBytes, 10),
		KeyOpenWebUIMaxRequestBytes:    strconv.FormatInt(c.OpenWebUI.MaxRequestBytes, 10),
		KeyOpenWebUIMaxContextMessages: strconv.Itoa(c.OpenWebUI.MaxContextMessages),
		KeyOpenWebUIWebSearchEnabled:   optionalBoolString(c.OpenWebUI.WebSearchEnabled),
		KeyOpenWebUIViewerBaseURL:      c.OpenWebUI.ViewerBaseURL,
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

// parseOptionalBoolPtr is parseOptionalBool's tri-state counterpart: an
// absent or empty key returns nil (distinct from an explicit "false"),
// for a field whose "unset" state means something different from either
// boolean value (OpenWebUIConfig.WebSearchEnabled, ADR-0005 D21) rather
// than merely picking a default.
func parseOptionalBoolPtr(values map[string]string, key string, errs *[]FieldError) *bool {
	v, ok := values[key]
	if !ok || v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be a boolean (true/false)"})
		return nil
	}
	return &b
}

// optionalBoolString renders a *bool for Config.Redacted(): "unset" for
// nil, otherwise the same strconv.FormatBool text every other boolean
// field already uses.
func optionalBoolString(v *bool) string {
	if v == nil {
		return "unset"
	}
	return strconv.FormatBool(*v)
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

// trimRightEach returns a new slice with cutset right-trimmed from every
// entry, leaving a nil list nil. Used by OPENWEBUI_ALLOWED_ORIGINS so a
// trailing slash there does not make an otherwise-identical origin fail
// OPENWEBUI_BASE_URL's exact-match membership check.
func trimRightEach(list []string, cutset string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = strings.TrimRight(v, cutset)
	}
	return out
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

// openWebUIPresentationHostPattern bounds OPENWEBUI_PRESENTATION_HOST to
// a lowercase DNS hostname: labels of letters, digits and hyphens
// separated by dots, with no scheme, port, path, or trailing dot. It is
// a presentation value that appears verbatim in a UserLite's host field,
// so anything a client might try to resolve or parse as a URL is
// rejected here rather than surfacing in a wire payload.
var openWebUIPresentationHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// validateOpenWebUIOrigins checks OPENWEBUI_ALLOWED_ORIGINS is a
// non-empty list of HTTPS origins. Unlike validateOrigin (LOCAL_ORIGIN)
// http is never accepted, in any environment: this list is what bounds
// where an API key may be sent (ADR-0005 D11), so a plaintext entry
// would defeat the point of having it.
func validateOpenWebUIOrigins(errs *[]FieldError, key string, list []string) bool {
	if len(list) == 0 {
		*errs = append(*errs, FieldError{Key: key, Reason: "required when " + KeyOpenWebUIEnabled + "=true"})
		return false
	}
	ok := true
	for _, origin := range list {
		if !isHTTPSOrigin(origin) {
			*errs = append(*errs, FieldError{Key: key, Reason: "each entry must be an https origin URL with no userinfo, path, query, or fragment"})
			ok = false
		}
	}
	return ok
}

// validateOpenWebUIBaseURL checks OPENWEBUI_BASE_URL is an HTTPS origin
// that the allowlist actually permits. The exact-match membership test
// is the point: an allowlist the configured target is not required to
// satisfy would be documentation rather than a control.
func validateOpenWebUIBaseURL(errs *[]FieldError, key, v string, allowed []string) bool {
	if v == "" {
		*errs = append(*errs, FieldError{Key: key, Reason: "required when " + KeyOpenWebUIEnabled + "=true"})
		return false
	}
	if !isHTTPSOrigin(v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be an https origin URL with no userinfo, path, query, or fragment"})
		return false
	}
	if !slices.Contains(allowed, v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must appear verbatim in " + KeyOpenWebUIAllowedOrigins})
		return false
	}
	return true
}

// validateOpenWebUIViewerBaseURL checks OPENWEBUI_VIEWER_BASE_URL's
// shape only when it is set at all: unlike BaseURL, it is optional
// (empty disables Issue #84's viewer link and title-generation request
// entirely — ADR-0005 D23), and unlike BaseURL it is never checked
// against AllowedOrigins, since this server never dials it — D11's SSRF
// allowlist governs outbound connections this server makes, and this
// value only ever appears in rendered Note text.
func validateOpenWebUIViewerBaseURL(errs *[]FieldError, key, v string) bool {
	if v == "" {
		return true
	}
	if !isHTTPSOrigin(v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be an https origin URL with no userinfo, path, query, or fragment"})
		return false
	}
	return true
}

// isHTTPSOrigin reports whether v is an absolute https URL naming only a
// scheme and a host, the same origin shape validateOrigin enforces for
// LOCAL_ORIGIN minus the http option.
func isHTTPSOrigin(v string) bool {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	return u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

// validateOpenWebUIPresentationHost checks OPENWEBUI_PRESENTATION_HOST
// is a bare lowercase hostname and is not this service's own host.
//
// The second check is what keeps the projection honest: a UserLite with
// a null host means "local to this service", and every actor but a
// VirtualActor projects that way. If the VirtualActor's presentation
// host were this service's own host, a client would have two different
// spellings for the same place and no way to tell a local actor from a
// presented one.
func validateOpenWebUIPresentationHost(errs *[]FieldError, key, v, localOrigin string) bool {
	if v == "" {
		*errs = append(*errs, FieldError{Key: key, Reason: "required when " + KeyOpenWebUIEnabled + "=true"})
		return false
	}
	if !openWebUIPresentationHostPattern.MatchString(v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be a lowercase DNS hostname with no scheme, port, path, or trailing dot"})
		return false
	}
	if u, err := url.Parse(localOrigin); err == nil && u.Hostname() != "" && strings.EqualFold(u.Hostname(), v) {
		*errs = append(*errs, FieldError{Key: key, Reason: "must differ from " + KeyLocalOrigin + "'s host"})
		return false
	}
	return true
}
