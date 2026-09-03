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

	for _, key := range knownKeyOrder {
		if v, ok := opts.Getenv(key); ok {
			values[key] = v
		}
	}

	cfg, errs := parse(values)
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

	for k := range raw {
		if !isKnownKey(k) {
			return nil, &ValidationError{Fields: []FieldError{{
				Key:    k,
				Reason: fmt.Sprintf("unknown config key in %s", path),
			}}}
		}
	}
	return raw, nil
}

func parse(values map[string]string) (Config, []FieldError) {
	var errs []FieldError
	var cfg Config

	cfg.Env = Environment(parseRequiredEnum(values, KeyAppEnv, []string{
		string(EnvDevelopment), string(EnvStaging), string(EnvProduction),
	}, &errs))

	cfg.HTTP.Host = parseOptionalString(values, KeyHTTPHost, "0.0.0.0")
	cfg.HTTP.Port = parseOptionalInt(values, KeyHTTPPort, 8080, 1, 65535, &errs)
	cfg.HTTP.ReadTimeout = parseOptionalDuration(values, KeyHTTPReadTimeout, 5*time.Second, &errs)
	cfg.HTTP.ReadHeaderTimeout = parseOptionalDuration(values, KeyHTTPReadHeaderTimeout, 5*time.Second, &errs)
	cfg.HTTP.WriteTimeout = parseOptionalDuration(values, KeyHTTPWriteTimeout, 10*time.Second, &errs)
	cfg.HTTP.IdleTimeout = parseOptionalDuration(values, KeyHTTPIdleTimeout, 60*time.Second, &errs)
	cfg.HTTP.MaxRequestBodyBytes = parseOptionalInt64(values, KeyHTTPMaxBodyBytes, 1<<20, 1, &errs)
	cfg.HTTP.ShutdownGracePeriod = parseOptionalDuration(values, KeyHTTPShutdownGrace, 15*time.Second, &errs)

	cfg.Log.Level = parseOptionalEnum(values, KeyLogLevel, "info", []string{"debug", "info", "warn", "error"}, &errs)
	cfg.Log.Format = parseOptionalEnum(values, KeyLogFormat, "text", []string{"json", "text"}, &errs)

	return cfg, errs
}

// Validate re-checks cross-field and environment-dependent rules that a
// single field's parser cannot express alone, such as production
// hardening. Load always calls it; a Config built by hand (tests,
// cmd/server defaults) should call it too before use.
func (c Config) Validate() error {
	var errs []FieldError

	switch c.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		errs = append(errs, FieldError{Key: KeyAppEnv, Reason: "must be one of development, staging, production"})
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
	}
}

func parseRequiredEnum(values map[string]string, key string, allowed []string, errs *[]FieldError) string {
	v, ok := values[key]
	if !ok || v == "" {
		*errs = append(*errs, FieldError{Key: key, Reason: "required"})
		return ""
	}
	if !containsString(allowed, v) {
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
	if !containsString(allowed, v) {
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
	if err != nil || n < min || n > max {
		*errs = append(*errs, FieldError{Key: key, Reason: fmt.Sprintf("must be an integer between %d and %d", min, max)})
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
	if err != nil || n < min {
		*errs = append(*errs, FieldError{Key: key, Reason: fmt.Sprintf("must be an integer of at least %d", min)})
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
	if err != nil || d <= 0 {
		*errs = append(*errs, FieldError{Key: key, Reason: "must be a positive duration (e.g. 5s)"})
		return def
	}
	return d
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
