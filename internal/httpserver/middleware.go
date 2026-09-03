package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/logging"
)

// withRequestID assigns a random per-request correlation ID to the request
// context before the handler runs, so logs can be tied together without
// ever needing the client-visible URL or headers.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithRequestID(r.Context(), newRequestID())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	buf := make([]byte, 16)
	// crypto/rand.Read never returns an error on Go 1.24+: it crashes the
	// process irrecoverably instead of returning one (go.dev/issue/66821),
	// so there is no error path here to check or recover from.
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// withMaxBody bounds the request body to maxBytes using
// http.MaxBytesReader, so a handler reading the body gets a clear error
// instead of the server buffering an unbounded payload.
func withMaxBody(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// withRecover turns a handler panic into a 500 response and a logged
// error, instead of crashing the process or leaking a Go stack trace to
// the client.
func withRecover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"request_id", logging.RequestIDFromContext(r.Context()),
					"panic", fmt.Sprintf("%v", rec),
				)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
