// Package config loads and validates this service's startup configuration
// from defaults, an optional dotenv-style config file, and environment
// variables, in that increasing priority order. It never depends on any
// other internal package, and it never lets an invalid or unknown value's
// raw text reach an error message or log line.
package config

import (
	"fmt"
	"net"
	"os"
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
	httpPortMin, httpPortMax               = 1, 65535
	httpMaxRequestBodyBytesMin             = 1
	dbBusyTimeoutMSMin, dbBusyTimeoutMSMax = 1, 600_000
	dbMaxOpenConnsMin, dbMaxOpenConnsMax   = 1, 100
)

func parse(values map[string]string) (Config, []FieldError) {
	var errs []FieldError
	var cfg Config

	cfg.Env = Environment(parseRequiredEnum(values, KeyAppEnv, allowedEnvironments, &errs))

	cfg.HTTP.Host = parseOptionalString(values, KeyHTTPHost, "0.0.0.0")
	cfg.HTTP.Port = parseOptionalInt(values, KeyHTTPPort, 8080, httpPortMin, httpPortMax, &errs)
	cfg.HTTP.ReadTimeout = parseOptionalDuration(values, KeyHTTPReadTimeout, 5*time.Second, &errs)
	cfg.HTTP.ReadHeaderTimeout = parseOptionalDuration(values, KeyHTTPReadHeaderTimeout, 5*time.Second, &errs)
	cfg.HTTP.WriteTimeout = parseOptionalDuration(values, KeyHTTPWriteTimeout, 10*time.Second, &errs)
	cfg.HTTP.IdleTimeout = parseOptionalDuration(values, KeyHTTPIdleTimeout, 60*time.Second, &errs)
	cfg.HTTP.MaxRequestBodyBytes = parseOptionalInt64(values, KeyHTTPMaxBodyBytes, 1<<20, httpMaxRequestBodyBytesMin, &errs)
	cfg.HTTP.ShutdownGracePeriod = parseOptionalDuration(values, KeyHTTPShutdownGrace, 15*time.Second, &errs)

	cfg.Log.Level = parseOptionalEnum(values, KeyLogLevel, "info", []string{"debug", "info", "warn", "error"}, &errs)
	cfg.Log.Format = parseOptionalEnum(values, KeyLogFormat, "text", []string{"json", "text"}, &errs)

	cfg.DB.Path = parseOptionalString(values, KeyDBPath, "./data/portal.db")
	busyTimeoutMS := parseOptionalInt(values, KeyDBBusyTimeoutMS, 5000, dbBusyTimeoutMSMin, dbBusyTimeoutMSMax, &errs)
	cfg.DB.BusyTimeout = time.Duration(busyTimeoutMS) * time.Millisecond
	cfg.DB.MaxOpenConns = parseOptionalInt(values, KeyDBMaxOpenConns, 8, dbMaxOpenConnsMin, dbMaxOpenConnsMax, &errs)

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
	}
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
