// Package logging builds this service's structured logger and its
// access-log HTTP middleware. Every attribute key AGENTS.md forbids from
// appearing in logs is redacted centrally here, so callers elsewhere in
// the codebase do not each need to remember the rule.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// sensitiveKeys lists structured-log attribute keys whose value is always
// replaced, regardless of nesting depth or case, matching AGENTS.md:
// "never log access tokens, API keys, cookies, authorization headers,
// MiAuth state, message bodies, mail bodies, or full LLM prompts".
var sensitiveKeys = map[string]struct{}{
	"authorization": {},
	"cookie":        {},
	"set-cookie":    {},
	"i":             {},
	"token":         {},
	"access_token":  {},
	"api_key":       {},
	"apikey":        {},
	"state":         {},
	"miauth_state":  {},
	"password":      {},
	"secret":        {},
	"body":          {},
	"prompt":        {},
	"mail_body":     {},
}

const redactedValue = "REDACTED"

// Config selects the logger's minimum level and output encoding.
type Config struct {
	// Level is one of debug, info, warn, error. An unrecognized value
	// falls back to info.
	Level string
	// Format is "json" or anything else for slog's text encoding.
	Format string
}

// New builds a slog.Logger writing to w. Any attribute whose key matches
// sensitiveKeys (case-insensitively, including inside a nested slog.Group)
// has its value replaced before the handler ever encodes it.
func New(w io.Writer, cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(cfg.Level),
		ReplaceAttr: redactingReplaceAttr,
	}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func redactingReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	if _, sensitive := sensitiveKeys[strings.ToLower(a.Key)]; sensitive {
		return slog.String(a.Key, redactedValue)
	}
	return a
}

type contextKey int

const requestIDKey contextKey = iota

// WithRequestID returns a context carrying id for later log correlation.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request ID stored by WithRequestID, or
// "" if none is set.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}
