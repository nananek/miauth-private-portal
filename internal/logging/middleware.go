package logging

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder captures the response status code so AccessLog can report
// it without buffering or altering the response body.
type statusRecorder struct {
	http.ResponseWriter
	status int
	// wrote is true once the response has actually been written to
	// (explicitly via WriteHeader or implicitly via the first Write), so
	// AccessLog's panic recovery can tell whether status already reflects
	// something sent to the client.
	wrote bool
	// headerWritten guards status: net/http only honors the first
	// WriteHeader call (later calls are a no-op on the wire, logged by
	// net/http as "superfluous"), so status must not be overwritten by a
	// second call either.
	headerWritten bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.headerWritten {
		r.status = status
		r.headerWritten = true
	}
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// AccessLog wraps h with a request-completion log line recording the
// method, pattern, status, duration, and request ID. It deliberately logs
// pattern (the route string a handler was registered under, e.g.
// "/miauth/{session}") rather than the raw request path, because a future
// route can embed a bearer secret in its path (see ADR-0001's Aria MiAuth
// {session} route); the raw path, query string, headers, and body are
// never logged here.
//
// If h panics, AccessLog still logs the completion line before re-raising
// the panic, so a caller-level recover (e.g. httpserver's withRecover)
// never loses the access-log entry for that request. The logged status is
// whatever was already written to the client (rec.status) if anything was,
// since that is what actually went out on the wire; only when the handler
// panicked before writing anything does it fall back to 500, matching what
// the outer recover-and-respond middleware sends in that case.
func AccessLog(logger *slog.Logger, pattern string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if p := recover(); p != nil {
				status := http.StatusInternalServerError
				if rec.wrote {
					status = rec.status
				}
				logAccess(logger, r, pattern, status, start)
				panic(p)
			}
		}()

		h.ServeHTTP(rec, r)
		logAccess(logger, r, pattern, rec.status, start)
	})
}

func logAccess(logger *slog.Logger, r *http.Request, pattern string, status int, start time.Time) {
	logger.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
		slog.String("method", r.Method),
		slog.String("route", pattern),
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
		slog.String("request_id", RequestIDFromContext(r.Context())),
	)
}
