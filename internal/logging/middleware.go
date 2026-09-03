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
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// AccessLog wraps h with a request-completion log line recording the
// method, pattern, status, duration, and request ID. It deliberately logs
// pattern (the route string a handler was registered under, e.g.
// "/miauth/{session}") rather than the raw request path, because a future
// route can embed a bearer secret in its path (see ADR-0001's Aria MiAuth
// {session} route); the raw path, query string, headers, and body are
// never logged here.
//
// If h panics, AccessLog still logs the completion line (with a 500
// status) before re-raising the panic, so a caller-level recover (e.g.
// httpserver's withRecover) never loses the access-log entry for that
// request.
func AccessLog(logger *slog.Logger, pattern string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if p := recover(); p != nil {
				logAccess(logger, r, pattern, http.StatusInternalServerError, start)
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
