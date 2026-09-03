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
