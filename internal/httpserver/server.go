// Package httpserver wires this service's HTTP handlers, middleware, and
// graceful-shutdown lifecycle. It never depends on internal/config or
// internal/storage/sqlite: the caller (cmd/server) translates a
// config.Config into the primitive-typed Options this package accepts
// and constructs internal/miauth.Service itself, so httpserver stays
// testable in isolation and a storage-adapter change never needs to
// touch it. Since Issue #5 it also depends on internal/miauth and
// internal/domain for the MiAuth wire boundary (miauth_handlers.go,
// miauth_wire.go, bootstrap_handlers.go, scope_middleware.go); it still
// never imports net/http-unaware use-case code the other direction, nor
// a storage driver type.
//
// Routing uses the standard library's net/http.ServeMux with its Go
// 1.22+ method+path patterns (e.g. "GET /healthz"). This service's
// Misskey-compatible surface is a small, mostly-static set of routes
// with at most one path parameter per route (Aria's MiAuth
// "/miauth/{session}"), which ServeMux already expresses directly, so no
// third-party router dependency is justified yet; see
// docs/operations/configuration.md for the fuller rationale.
package httpserver

import (
	"log/slog"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// Server wraps an http.ServeMux, applying access-log middleware to every
// route as it is registered.
type Server struct {
	mux    *http.ServeMux
	logger *slog.Logger

	miauth                   *miauth.Service
	timeline                 *timeline.Service
	localOrigin              string
	identityOrigin           string
	llmEnabled               bool
	llmClassificationEnabled bool
}

// NewServer builds a Server with liveness ("GET /healthz") and readiness
// ("GET /readyz") routes backed by reg always registered.
//
// opts.MiAuthService and its named origin fields configure Issue #5's
// MiAuth routes (GET /miauth/{session}, GET /miauth/callback, GET
// /miauth/bootstrap/{gate}, POST /api/miauth/{session}/check). A nil
// MiAuthService registers none of them, leaving a Server with only the
// health routes — the shape every httpserver test predating Issue #5
// still expects.
//
// opts.TimelineService additionally configures Issue #7's minimal
// Aria/Misskey-compatible note routes (POST /api/meta, /api/i,
// /api/endpoints, /api/notes/create, /api/notes/timeline,
// /api/notes/show, /api/notes/conversation, /api/notes/children). They
// register only when both opts.MiAuthService and opts.TimelineService
// are non-nil: every protected note route authenticates through
// RequireScope (which needs the MiAuth service), and there is no
// meaningful note API without a timeline to back it.
func NewServer(logger *slog.Logger, reg *health.Registry, opts Options) *Server {
	s := &Server{
		mux:                      http.NewServeMux(),
		logger:                   logger,
		miauth:                   opts.MiAuthService,
		timeline:                 opts.TimelineService,
		localOrigin:              opts.LocalOrigin,
		identityOrigin:           opts.IdentityOrigin,
		llmEnabled:               opts.LLMEnabled,
		llmClassificationEnabled: opts.LLMClassificationEnabled,
	}

	s.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealthResult(w, logger, reg.Live(r.Context()))
	}))
	s.Handle("GET /readyz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealthResult(w, logger, reg.Ready(r.Context()))
	}))

	if opts.MiAuthService != nil {
		s.Handle("GET /miauth/{session}", http.HandlerFunc(s.handleMiAuthStart))
		s.Handle("GET /miauth/callback", http.HandlerFunc(s.handleMiAuthCallback))
		s.Handle("GET /miauth/bootstrap/{gate}", http.HandlerFunc(s.handleMiAuthBootstrapStart))
		s.Handle("POST /api/miauth/{session}/check", http.HandlerFunc(s.handleMiAuthCheck))
	}

	if opts.MiAuthService != nil && opts.TimelineService != nil {
		s.Handle("POST /api/meta", http.HandlerFunc(s.handleMeta))
		s.Handle("POST /api/endpoints", http.HandlerFunc(s.handleEndpoints))
		s.Handle("POST /api/i", RequireScope(logger, s.miauth, miauth.ScopeReadAccount)(http.HandlerFunc(s.handleAPII)))
		s.Handle("POST /api/notes/create", RequireScope(logger, s.miauth, miauth.ScopeWriteNotes)(http.HandlerFunc(s.handleNotesCreate)))
		s.Handle("POST /api/notes/timeline", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesTimeline)))
		s.Handle("POST /api/notes/show", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesShow)))
		s.Handle("POST /api/notes/conversation", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesConversation)))
		s.Handle("POST /api/notes/children", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesChildren)))
	}

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
