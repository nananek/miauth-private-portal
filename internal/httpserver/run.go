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

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/miauth"
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

	// MiAuthService, LocalOrigin, and IdentityOrigin configure Issue #5's
	// MiAuth routes; see NewServer. A nil MiAuthService registers none of
	// them.
	MiAuthService  *miauth.Service
	LocalOrigin    string
	IdentityOrigin string
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
