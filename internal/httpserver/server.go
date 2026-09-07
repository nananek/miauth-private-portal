// Package httpserver wires this service's HTTP handlers, middleware, and
// graceful-shutdown lifecycle. It never depends on internal/config or
// internal/storage/sqlite: the caller (cmd/server) translates a
// config.Config into the primitive-typed Options this package accepts
// and constructs internal/miauth.Service itself, so httpserver stays
// testable in isolation and a storage-adapter change never needs to
// touch it. Since Issue #5 it also depends on internal/miauth and
// internal/domain for the MiAuth wire boundary (miauth_handlers.go,
// miauth_wire.go, scope_middleware.go); it still
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
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/streamhub"
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
	llmEnabled               bool
	llmClassificationEnabled bool
	// virtualActors resolves an Open WebUI model actor to the
	// VirtualActor Aria sees (Issue #52). A nil value (the default,
	// OPENWEBUI_ENABLED off) leaves resolveUserLite's existing fallback
	// projection untouched for every actor.
	virtualActors VirtualActorResolver
	// openWebUIBridge is Issue #53's notes/create enqueue hook; see
	// Options.OpenWebUIBridge. A nil value leaves handleNotesCreate on
	// its plain Create{Root,Reply} path.
	openWebUIBridge timeline.EntryHook
	// openWebUITurnLinks and openWebUIViewerBaseURL back Issues #81/#84's
	// wire-projection enrichment; see Options.OpenWebUITurnLinks and
	// Options.OpenWebUIViewerBaseURL.
	openWebUITurnLinks     domain.OpenWebUITurnLinkRepository
	openWebUIViewerBaseURL string

	// streamSem bounds concurrent GET /streaming connections; see
	// maxConcurrentStreamConnections (streaming_handlers.go).
	streamSem          chan struct{}
	streamPingInterval time.Duration
	// streamHub is Issue #95 PR2's live push source; see Options.StreamHub.
	// nil disables push delivery entirely, leaving GET /streaming as
	// Issue #41's handshake/ack/keepalive-only stub.
	streamHub *streamhub.Hub
}

// NewServer builds a Server with liveness ("GET /healthz") and readiness
// ("GET /readyz") routes backed by reg always registered.
//
// opts.MiAuthService configures the local MiAuth routes (GET
// /miauth/{session}, POST /api/miauth/{session}/check). A nil
// MiAuthService registers none of them, leaving a Server with only the
// health routes — the shape every httpserver test predating Issue #5
// still expects.
//
// opts.TimelineService additionally configures Issue #7's minimal
// Aria/Misskey-compatible note routes (POST /api/meta, /api/i,
// /api/endpoints, /api/notes/create, /api/notes/timeline,
// /api/notes/show, /api/notes/conversation, /api/notes/children), plus
// Issue #23 PR1's POST /api/i/update, PR2's anonymous POST /api/stats,
// PR3's POST /api/notes/delete, PR4's POST
// /api/notes/reactions/create, /api/notes/reactions/delete, and POST
// /api/notes/reactions, PR5's POST /api/notes/mentions, PR6's POST
// /api/i/notifications, and Issue #65's POST /api/users/search and
// /api/users/search-by-username-and-host. They register only when both
// opts.MiAuthService and opts.TimelineService are non-nil: every
// protected note route authenticates through RequireScope (which needs
// the MiAuth service), and there is no meaningful note API without a
// timeline to back it.
func NewServer(logger *slog.Logger, reg *health.Registry, opts Options) *Server {
	pingInterval := opts.StreamPingInterval
	if pingInterval <= 0 {
		pingInterval = defaultStreamPingInterval
	}
	s := &Server{
		mux:                      http.NewServeMux(),
		logger:                   logger,
		miauth:                   opts.MiAuthService,
		timeline:                 opts.TimelineService,
		localOrigin:              opts.LocalOrigin,
		llmEnabled:               opts.LLMEnabled,
		llmClassificationEnabled: opts.LLMClassificationEnabled,
		virtualActors:            opts.VirtualActors,
		openWebUIBridge:          opts.OpenWebUIBridge,
		openWebUITurnLinks:       opts.OpenWebUITurnLinks,
		openWebUIViewerBaseURL:   opts.OpenWebUIViewerBaseURL,
		streamSem:                make(chan struct{}, maxConcurrentStreamConnections),
		streamPingInterval:       pingInterval,
		streamHub:                opts.StreamHub,
	}

	s.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealthResult(w, logger, reg.Live(r.Context()))
	}))
	s.Handle("GET /readyz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealthResult(w, logger, reg.Ready(r.Context()))
	}))
	// Issue #77 PR2: default favicon/OGP/PWA icons (staticicons.go),
	// always registered like the health routes above — a fresh install
	// must show a real icon with no configuration.
	s.registerStaticIcons()

	if opts.MiAuthService != nil {
		s.Handle("GET /miauth/{session}", http.HandlerFunc(s.handleMiAuthStart))
		s.Handle("POST /api/miauth/{session}/check", http.HandlerFunc(s.handleMiAuthCheck))
		// GET /streaming's route registration only needs read:account
		// authentication (Issue #41), not a timeline (opts.TimelineService
		// may be nil here), so it belongs in this MiAuthService-only group
		// rather than mixed into the note-API group below, which exists
		// because every route there needs both scoped auth and a timeline
		// to read. Since Issue #95 PR2, a connection subscribed to
		// "homeTimeline" does receive a live note-create push, but only
		// once opts.TimelineService and opts.StreamHub are also configured
		// (serveStreamConn's nil checks) — cmd/server/main.go always wires
		// all three together.
		s.Handle("GET /streaming", http.HandlerFunc(s.handleStreaming))
	}

	if opts.MiAuthService != nil && opts.TimelineService != nil {
		s.Handle("POST /api/meta", http.HandlerFunc(s.handleMeta))
		s.Handle("POST /api/endpoints", http.HandlerFunc(s.handleEndpoints))
		s.Handle("POST /api/stats", http.HandlerFunc(s.handleStats))
		s.Handle("POST /api/i", RequireScope(logger, s.miauth, miauth.ScopeReadAccount)(http.HandlerFunc(s.handleAPII)))
		s.Handle("POST /api/i/update", RequireScope(logger, s.miauth, miauth.ScopeWriteAccount)(http.HandlerFunc(s.handleAPIIUpdate)))
		s.Handle("POST /api/notes/create", RequireScope(logger, s.miauth, miauth.ScopeWriteNotes)(http.HandlerFunc(s.handleNotesCreate)))
		s.Handle("POST /api/notes/timeline", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesTimeline)))
		s.Handle("POST /api/notes/show", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesShow)))
		s.Handle("POST /api/notes/conversation", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesConversation)))
		s.Handle("POST /api/notes/children", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesChildren)))
		s.Handle("POST /api/notes/delete", RequireScope(logger, s.miauth, miauth.ScopeWriteNotes)(http.HandlerFunc(s.handleNotesDelete)))
		s.Handle("POST /api/notes/reactions/create", RequireScope(logger, s.miauth, miauth.ScopeWriteReactions)(http.HandlerFunc(s.handleNotesReactionsCreate)))
		s.Handle("POST /api/notes/reactions/delete", RequireScope(logger, s.miauth, miauth.ScopeWriteReactions)(http.HandlerFunc(s.handleNotesReactionsDelete)))
		s.Handle("POST /api/notes/reactions", RequireScope(logger, s.miauth, miauth.ScopeReadReactions)(http.HandlerFunc(s.handleNotesReactions)))
		s.Handle("POST /api/notes/mentions", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesMentions)))
		s.Handle("POST /api/i/notifications", RequireScope(logger, s.miauth, miauth.ScopeReadNotifications)(http.HandlerFunc(s.handleAPINotifications)))
		s.Handle("POST /api/users/search", RequireScope(logger, s.miauth, miauth.ScopeReadAccount)(http.HandlerFunc(s.handleUsersSearch)))
		s.Handle("POST /api/users/search-by-username-and-host", RequireScope(logger, s.miauth, miauth.ScopeReadAccount)(http.HandlerFunc(s.handleUsersSearchByUsernameAndHost)))
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
