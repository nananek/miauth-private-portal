package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/drive"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/streamhub"
	"github.com/nananek/miauth-private-portal/internal/timeline"
	"github.com/nananek/miauth-private-portal/internal/userlist"
)

// Options configures the HTTP server. It intentionally contains only
// primitive and standard-library types (plus *miauth.Service, itself
// independent of internal/config and any storage driver type) so this
// package never depends on internal/config; cmd/server translates a
// config.Config into Options.
type Options struct {
	// Addr is the host:port to listen on. Ignored if Listener is set.
	Addr string
	// Listener, when non-nil, is used instead of binding Addr. It exists
	// primarily for tests, which bind an ephemeral port (":0") themselves
	// so they can learn the actual address before Run starts serving.
	Listener net.Listener

	ReadTimeout         time.Duration
	ReadHeaderTimeout   time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	MaxRequestBodyBytes int64
	ShutdownGracePeriod time.Duration

	// MiAuthService and LocalOrigin configure the local MiAuth routes; see
	// NewServer. A nil MiAuthService registers none of them.
	MiAuthService *miauth.Service
	LocalOrigin   string

	// TimelineService additionally configures Issue #7's minimal
	// Aria/Misskey-compatible note routes; see NewServer. A nil
	// TimelineService (or a nil MiAuthService) registers none of them.
	TimelineService *timeline.Service

	// UserListService additionally configures Issue #115's
	// Misskey-compatible users/lists/* CRUD and membership push/pull
	// routes; see NewServer. A nil UserListService (or a nil
	// MiAuthService/TimelineService) registers none of them —
	// handleUsersListsPush validates its userId through the same
	// searchCandidates enumeration users/search uses, which needs
	// TimelineService.
	UserListService *userlist.Service

	// LLMEnabled gates Issue #9's notes/create enqueue hook: when false
	// (LLM_ENABLED's safe default), handleNotesCreate never evaluates the
	// reply/follow-up policy and never enqueues an "llm_generation" job,
	// regardless of post content.
	LLMEnabled bool
	// LLMClassificationEnabled gates Issue #10's notes/create enqueue
	// hook: when false (LLM_CLASSIFICATION_ENABLED's safe default),
	// handleNotesCreate never enqueues an "llm_classification" job.
	// Independent of LLMEnabled: an operator can run one feature without
	// the other.
	LLMClassificationEnabled bool

	// StreamPingInterval configures GET /streaming's keepalive ping
	// interval (Issue #41). Zero (its safe default) means
	// defaultStreamPingInterval (streaming_handlers.go); tests shorten it
	// to observe a ping/pong cycle without waiting 30+ seconds.
	StreamPingInterval time.Duration
	// StreamHub is Issue #95 PR2's live push delivery source: when
	// non-nil, every GET /streaming connection subscribed to
	// "homeTimeline" receives a push for each new entry StreamHub
	// publishes (cmd/server/main.go wires the same *streamhub.Hub given
	// to timeline.Config.Broadcaster here, so the two sides of one
	// broadcast never drift apart). A nil value (the safe default —
	// matching every httpserver test predating this field) leaves
	// GET /streaming exactly as Issue #41 left it: handshake, acks, and
	// keepalive only, no push.
	StreamHub *streamhub.Hub

	// VirtualActors resolves an Open WebUI model actor to the
	// VirtualActor projection Aria sees (Issue #52). A nil value (the
	// safe default, OPENWEBUI_ENABLED off) means resolveUserLite's
	// existing fallback projection applies to every actor, unchanged
	// from before this field existed. It is the narrow
	// VirtualActorResolver interface, not internal/openwebui.Registry
	// itself, so this package still never imports that use-case package
	// directly — cmd/server passes its concrete *openwebui.Registry in,
	// which already satisfies the interface structurally.
	VirtualActors VirtualActorResolver

	// ExternalSources resolves an ActorExternalSource actor to its owning
	// domain.ExternalSource (Issue #77 PR4/ADR-0008's design-A host
	// display). A nil value means resolveUserLite's existing fallback
	// projection applies. It is the narrow ExternalSourceResolver
	// interface (satisfied structurally by domain.ExternalSourceRepository
	// itself, via its GetByActorID method) — cmd/server passes
	// db.Repos.ExternalSources directly, so this package still never
	// imports a storage driver type.
	ExternalSources ExternalSourceResolver

	// OpenWebUIBridge is Issue #53's notes/create enqueue hook: when
	// non-nil, handleNotesCreate creates every user_post through
	// timeline.Service's Create{Root,Reply}WithHook instead of
	// Create{Root,Reply}, so a branch claim, turn record, and durable
	// "openwebui_turn" job are all committed atomically alongside the
	// post whenever the hook decides to enqueue one. A nil value (the
	// safe default — OPENWEBUI_ENABLED or its generation gate off) keeps
	// handleNotesCreate on the plain Create{Root,Reply} path, unchanged
	// from before this field existed. cmd/server passes an
	// *openwebui.Bridge's EnqueueTurn method value in, which matches
	// timeline.EntryHook's signature structurally without this package
	// importing internal/openwebui.
	OpenWebUIBridge timeline.EntryHook

	// OpenWebUITurnLinks backs Issues #81/#84's wire-projection
	// enrichment (projectNote): looking up an Open WebUI-generated
	// reply's turn (by its assistant entry) to attach citation
	// footnotes, a generated chat title, and an owner-facing viewer
	// link. A nil value (the safe default, OPENWEBUI_ENABLED off) leaves
	// every EntryLLMReply projected exactly as wireText alone already
	// produces it, unchanged from before this field existed. This is
	// domain.OpenWebUITurnLinkRepository, the same domain-level
	// interface internal/storage/sqlite already implements — not a
	// storage package type — so this package's "no storage driver
	// dependency" rule (see the package doc comment) is unaffected.
	OpenWebUITurnLinks domain.OpenWebUITurnLinkRepository
	// OpenWebUIViewerBaseURL mirrors OPENWEBUI_VIEWER_BASE_URL (Issue
	// #84, ADR-0005 D23): when non-empty, projectNote inserts
	// "<OpenWebUIViewerBaseURL>/c/<remote_chat_id>" on its own line
	// directly under the title/marker line of a generated reply's text,
	// whenever its turn has a known remote chat id (2026-09-08: moved
	// there from the text's end, for visibility without scrolling past
	// the body). Empty (the default, unset) never inserts anything —
	// D23's "leaving it unset reproduces today's behavior exactly"
	// guarantee.
	OpenWebUIViewerBaseURL string

	// Drive backs Issue #77 PR3's Misskey-compatible Drive API and the
	// anonymous GET /files/{id} serving route; see NewServer. A nil
	// value (the safe default) registers neither.
	Drive *drive.Service
	// DriveMaxFileBytes bounds a single drive/files/create upload's file
	// part; see Server.driveMaxFileBytes's doc comment for why it is
	// enforced independently of drive.Service's own Config.MaxFileBytes.
	// Meaningless while Drive is nil.
	DriveMaxFileBytes int64
}

// Run builds the HTTP server from opts, serves it, marks reg ready once
// serving has started, and blocks until ctx is cancelled (including by
// SIGINT/SIGTERM) or the server fails. It always performs a graceful
// shutdown bounded by opts.ShutdownGracePeriod before returning, falling
// back to a forced close if in-flight requests do not finish in time.
func Run(ctx context.Context, opts Options, logger *slog.Logger, reg *health.Registry) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln := opts.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", opts.Addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", opts.Addr, err)
		}
	}

	server := NewServer(logger, reg, opts)
	// withRequestID must wrap withRecover (not the other way around):
	// withRequestID's r.WithContext call produces a new *http.Request, so
	// if it sat inside withRecover, withRecover's deferred closure would
	// keep referring to the original, pre-request-ID request and never
	// see the request ID on panic.
	handler := withRequestID(withRecover(logger, withMaxBody(opts.MaxRequestBodyBytes, server.Handler())))

	// baseCtx roots every in-flight request's context. It is deliberately
	// independent of the SIGINT/SIGTERM-derived ctx above: srv.Shutdown
	// already gives in-flight requests up to opts.ShutdownGracePeriod to
	// finish on their own, so cancelling every request context the
	// instant a shutdown signal arrives would defeat that grace period.
	// It is only cancelled if the grace period is exceeded and we fall
	// back to a forced close.
	baseCtx, cancelBaseCtx := context.WithCancel(context.Background())
	defer cancelBaseCtx()
	srv := newHTTPServer(baseCtx, opts, handler, logger)

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server starting", "addr", ln.Addr().String())
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	reg.MarkReady()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		// Stop intercepting SIGINT/SIGTERM now, before waiting out the
		// grace period: signal.NotifyContext suppresses the OS's default
		// terminate-on-signal behavior for as long as it stays registered,
		// so a second Ctrl-C during a slow drain must revert to that
		// default (immediate termination) instead of being silently
		// swallowed.
		stop()
		gracefulShutdown(srv, reg, logger, opts.ShutdownGracePeriod, cancelBaseCtx)
		return <-serveErr
	case err := <-serveErr:
		logger.Error("http server failed", "error", err.Error())
		stop()
		gracefulShutdown(srv, reg, logger, opts.ShutdownGracePeriod, cancelBaseCtx)
		return err
	}
}

// newHTTPServer builds the *http.Server used by Run. It is factored out so
// tests can assert timeout wiring directly against the returned struct
// without going through a real listen/serve/shutdown cycle.
func newHTTPServer(ctx context.Context, opts Options, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadTimeout:       opts.ReadTimeout,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
		WriteTimeout:      opts.WriteTimeout,
		IdleTimeout:       opts.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

// gracefulShutdown marks reg not-ready and gives in-flight requests up to
// grace to finish via srv.Shutdown, falling back to a forced srv.Close
// (and cancelling every request's base context) if they do not.
func gracefulShutdown(srv *http.Server, reg *health.Registry, logger *slog.Logger, grace time.Duration, cancelBaseCtx context.CancelFunc) {
	reg.MarkNotReady()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown exceeded grace period, forcing close", "error", err.Error())
		cancelBaseCtx()
		_ = srv.Close()
	}
}
