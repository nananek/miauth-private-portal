// Package httpserver wires this service's HTTP handlers, middleware, and
// graceful-shutdown lifecycle. It deliberately depends only on
// internal/health and internal/logging, never on internal/config: the
// caller (cmd/server) translates a config.Config into the primitive-typed
// Options this package accepts, so httpserver stays testable in isolation
// and future internal/domain or SQLite-adapter code never needs to import
// it.
//
// Routing uses the standard library's net/http.ServeMux with its Go
// 1.22+ method+path patterns (e.g. "GET /healthz"). This service's
// Misskey-compatible surface (Issue #7) is a small, mostly-static set of
// routes with at most one path parameter per route (Aria's MiAuth
// "/miauth/{session}"), which ServeMux already expresses directly, so no
// third-party router dependency is justified yet; see
// docs/operations/configuration.md for the fuller rationale.
package httpserver

import (
	"log/slog"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

// Server wraps an http.ServeMux, applying access-log middleware to every
// route as it is registered.
type Server struct {
	mux    *http.ServeMux
	logger *slog.Logger
}

// NewServer builds a Server with liveness ("GET /healthz") and readiness
// ("GET /readyz") routes backed by reg already registered.
func NewServer(logger *slog.Logger, reg *health.Registry) *Server {
	s := &Server{mux: http.NewServeMux(), logger: logger}

	s.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealthResult(w, logger, reg.Live(r.Context()))
	}))
	s.Handle("GET /readyz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealthResult(w, logger, reg.Ready(r.Context()))
	}))

	return s
}

// Handle registers h under pattern, wrapped with access logging keyed by
// pattern rather than the request's raw path (see the package doc for
// why).
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, logging.AccessLog(s.logger, pattern, h))
}

// Handler returns the composed http.Handler for all registered routes.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func writeHealthResult(w http.ResponseWriter, logger *slog.Logger, err error) {
	if err != nil {
		// err is already wrapped with the failing Checker's name (see
		// health.Registry.Ready), so this is the one place that
		// diagnostic reaches an operator; discarding it silently would
		// make a 503 indistinguishable from "not ready yet" vs. "a
		// specific dependency is down".
		logger.Warn("readiness check failed", "error", err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
