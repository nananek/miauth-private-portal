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
	"github.com/nananek/miauth-private-portal/internal/drive"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/streamhub"
	"github.com/nananek/miauth-private-portal/internal/timeline"
	"github.com/nananek/miauth-private-portal/internal/userlist"
	"github.com/nananek/miauth-private-portal/internal/webadmin"
)

// Server wraps an http.ServeMux, applying access-log middleware to every
// route as it is registered.
type Server struct {
	mux    *http.ServeMux
	logger *slog.Logger

	miauth                   *miauth.Service
	timeline                 *timeline.Service
	userLists                *userlist.Service
	localOrigin              string
	llmEnabled               bool
	llmClassificationEnabled bool
	// virtualActors resolves an Open WebUI model actor to the
	// VirtualActor Aria sees (Issue #52). A nil value (the default,
	// OPENWEBUI_ENABLED off) leaves resolveUserLite's existing fallback
	// projection untouched for every actor.
	virtualActors VirtualActorResolver
	// externalSources resolves an ActorExternalSource actor to its owning
	// domain.ExternalSource (Issue #77 PR4/ADR-0008). A nil value leaves
	// resolveUserLite's existing fallback projection untouched — never
	// expected in production (cmd/server always passes db.Repos.
	// ExternalSources), but every httpserver test predating PR4 still
	// builds a Server without it.
	externalSources ExternalSourceResolver
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

	// drive backs Issue #77 PR3's Misskey-compatible Drive API and the
	// anonymous GET /files/{id} serving route. A nil value (the safe
	// default) registers neither.
	drive *drive.Service
	// driveMaxFileBytes bounds a single POST /api/drive/files/create
	// multipart upload's file part — drive_handlers.go enforces it
	// directly while reading that part (before any byte reaches
	// drive.Service), independent of drive.Service's own Config.
	// MaxFileBytes (the same bound, kept in sync by cmd/server) that
	// UploadFromURL and CreateFile's other callers rely on instead.
	driveMaxFileBytes int64

	// webadmin backs Issue #136's admin bootstrap/registration routes
	// (Phase 1: GET /admin/setup, POST /admin/setup/begin,
	// POST /admin/setup/finish) and login/session routes (Phase 2:
	// GET /admin/login, POST /admin/login/{begin,finish}, GET /admin/,
	// POST /admin/logout). A nil value (the default) registers none of
	// them — every httpserver test predating this field, and any
	// deployment that hasn't wired it yet, is unaffected.
	webadmin *webadmin.Service
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
		userLists:                opts.UserListService,
		localOrigin:              opts.LocalOrigin,
		llmEnabled:               opts.LLMEnabled,
		llmClassificationEnabled: opts.LLMClassificationEnabled,
		virtualActors:            opts.VirtualActors,
		externalSources:          opts.ExternalSources,
		openWebUIBridge:          opts.OpenWebUIBridge,
		openWebUITurnLinks:       opts.OpenWebUITurnLinks,
		openWebUIViewerBaseURL:   opts.OpenWebUIViewerBaseURL,
		streamSem:                make(chan struct{}, maxConcurrentStreamConnections),
		streamPingInterval:       pingInterval,
		streamHub:                opts.StreamHub,
		drive:                    opts.Drive,
		driveMaxFileBytes:        opts.DriveMaxFileBytes,
		webadmin:                 opts.WebAdmin,
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
		// Issue #114: users/show returns a profile (read:account, same
		// scope as users/search above); users/notes returns that actor's
		// own notes (read:notes, matching every other note-returning
		// endpoint in this group).
		s.Handle("POST /api/users/show", RequireScope(logger, s.miauth, miauth.ScopeReadAccount)(http.HandlerFunc(s.handleUsersShow)))
		s.Handle("POST /api/users/notes", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleUsersNotes)))

		// Issue #115 PR2: users/lists/* CRUD and membership push/pull.
		// Nested inside the TimelineService-gated group (rather than its
		// own opts.MiAuthService-only group, the way Drive's routes below
		// are independent of it) because handleUsersListsPush validates
		// its userId against searchCandidates, which itself calls
		// s.timeline.GetActorByType/ListActorsByType — see
		// isKnownActor's doc comment. cmd/server always wires
		// TimelineService and UserListService together, so this
		// additional nil check only matters to httpserver's own tests.
		if opts.UserListService != nil {
			s.Handle("POST /api/users/lists/create", RequireScope(logger, s.miauth, miauth.ScopeWriteAccount)(http.HandlerFunc(s.handleUsersListsCreate)))
			s.Handle("POST /api/users/lists/list", RequireScope(logger, s.miauth, miauth.ScopeReadAccount)(http.HandlerFunc(s.handleUsersListsList)))
			s.Handle("POST /api/users/lists/show", RequireScope(logger, s.miauth, miauth.ScopeReadAccount)(http.HandlerFunc(s.handleUsersListsShow)))
			s.Handle("POST /api/users/lists/update", RequireScope(logger, s.miauth, miauth.ScopeWriteAccount)(http.HandlerFunc(s.handleUsersListsUpdate)))
			s.Handle("POST /api/users/lists/delete", RequireScope(logger, s.miauth, miauth.ScopeWriteAccount)(http.HandlerFunc(s.handleUsersListsDelete)))
			s.Handle("POST /api/users/lists/push", RequireScope(logger, s.miauth, miauth.ScopeWriteAccount)(http.HandlerFunc(s.handleUsersListsPush)))
			s.Handle("POST /api/users/lists/pull", RequireScope(logger, s.miauth, miauth.ScopeWriteAccount)(http.HandlerFunc(s.handleUsersListsPull)))
			// Issue #115 PR3: the list's own filtered timeline. Scoped
			// read:notes, matching notes/timeline's own scope (plan-115
			// §2.1) rather than read:account like the CRUD routes above —
			// it reads notes, not list metadata.
			s.Handle("POST /api/notes/user-list-timeline", RequireScope(logger, s.miauth, miauth.ScopeReadNotes)(http.HandlerFunc(s.handleNotesUserListTimeline)))
		}
	}

	// Issue #77 PR3: the Misskey-compatible Drive API. Independent of
	// opts.TimelineService — Drive has never depended on the note
	// timeline, only on opts.MiAuthService for RequireScope/manual token
	// verification. drive/files/create authenticates itself (see
	// handleDriveFilesCreate's doc comment) rather than being wrapped in
	// RequireScope, since its multipart body carries "i" as a form
	// field, not the JSON RequireScope reads from.
	if opts.MiAuthService != nil && opts.Drive != nil {
		s.Handle("POST /api/drive", RequireScope(logger, s.miauth, miauth.ScopeReadDrive)(http.HandlerFunc(s.handleDrive)))
		s.Handle("POST /api/drive/files", RequireScope(logger, s.miauth, miauth.ScopeReadDrive)(http.HandlerFunc(s.handleDriveFiles)))
		s.Handle("POST /api/drive/files/create", http.HandlerFunc(s.handleDriveFilesCreate))
		s.Handle("POST /api/drive/files/show", RequireScope(logger, s.miauth, miauth.ScopeReadDrive)(http.HandlerFunc(s.handleDriveFilesShow)))
		s.Handle("POST /api/drive/files/update", RequireScope(logger, s.miauth, miauth.ScopeWriteDrive)(http.HandlerFunc(s.handleDriveFilesUpdate)))
		s.Handle("POST /api/drive/files/delete", RequireScope(logger, s.miauth, miauth.ScopeWriteDrive)(http.HandlerFunc(s.handleDriveFilesDelete)))
		s.Handle("POST /api/drive/files/upload-from-url", RequireScope(logger, s.miauth, miauth.ScopeWriteDrive)(http.HandlerFunc(s.handleDriveFilesUploadFromUrl)))
		s.Handle("POST /api/drive/files/attached-notes", RequireScope(logger, s.miauth, miauth.ScopeReadDrive)(http.HandlerFunc(s.handleDriveFilesAttachedNotes)))
		s.Handle("POST /api/drive/folders", RequireScope(logger, s.miauth, miauth.ScopeReadDrive)(http.HandlerFunc(s.handleDriveFolders)))
		s.Handle("POST /api/drive/folders/create", RequireScope(logger, s.miauth, miauth.ScopeWriteDrive)(http.HandlerFunc(s.handleDriveFoldersCreate)))
		s.Handle("POST /api/drive/folders/show", RequireScope(logger, s.miauth, miauth.ScopeReadDrive)(http.HandlerFunc(s.handleDriveFoldersShow)))
		s.Handle("POST /api/drive/folders/update", RequireScope(logger, s.miauth, miauth.ScopeWriteDrive)(http.HandlerFunc(s.handleDriveFoldersUpdate)))
		s.Handle("POST /api/drive/folders/delete", RequireScope(logger, s.miauth, miauth.ScopeWriteDrive)(http.HandlerFunc(s.handleDriveFoldersDelete)))
	}
	// GET /files/{id}: anonymous byte serving, independent of
	// opts.MiAuthService (see handleFilesShow's doc comment on why no
	// auth check belongs here).
	if opts.Drive != nil {
		s.Handle("GET /files/{id}", http.HandlerFunc(s.handleFilesShow))
	}

	// Issue #136 Phase 1 (ADR-0010): the admin bootstrap/registration
	// ceremony. Independent of opts.MiAuthService — these routes
	// authenticate a browser via a bearer bootstrap token, not Aria via
	// a local API token, and issue no session or cookie in this phase
	// (that is Phase 2). No feature flag gates this off; cmd/server
	// always constructs a webadmin.Service, the same way it always
	// constructs miauth.Service.
	if opts.WebAdmin != nil {
		s.Handle("GET /admin/setup", http.HandlerFunc(s.handleAdminSetup))
		s.Handle("POST /admin/setup/begin", http.HandlerFunc(s.handleAdminSetupBegin))
		s.Handle("POST /admin/setup/finish", http.HandlerFunc(s.handleAdminSetupFinish))

		// Issue #136 Phase 2 (ADR-0010): the WebAuthn login ceremony, the
		// session it produces, and the routes it protects.
		s.Handle("GET /admin/login", http.HandlerFunc(s.handleAdminLogin))
		s.Handle("POST /admin/login/begin", http.HandlerFunc(s.handleAdminLoginBegin))
		s.Handle("POST /admin/login/finish", http.HandlerFunc(s.handleAdminLoginFinish))

		adminAuth := RequireAdminSession(logger, opts.WebAdmin)
		s.Handle("GET /admin/", adminAuth(http.HandlerFunc(s.handleAdminIndex)))
		s.Handle("POST /admin/logout", adminAuth(RequireAdminCSRF(http.HandlerFunc(s.handleAdminLogout))))
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
