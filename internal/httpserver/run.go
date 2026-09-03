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
)

// Options configures the HTTP server. It intentionally contains only
// primitive and standard-library types so this package never depends on
// internal/config; cmd/server translates a config.Config into Options.
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

	server := NewServer(logger, reg)
	handler := withRecover(logger, withRequestID(withMaxBody(opts.MaxRequestBodyBytes, server.Handler())))
	srv := newHTTPServer(ctx, opts, handler, logger)

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
	case err := <-serveErr:
		return err
	}

	return shutdown(srv, reg, logger, opts.ShutdownGracePeriod, serveErr)
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

func shutdown(srv *http.Server, reg *health.Registry, logger *slog.Logger, grace time.Duration, serveErr <-chan error) error {
	reg.MarkNotReady()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown exceeded grace period, forcing close", "error", err.Error())
		_ = srv.Close()
	}

	return <-serveErr
}
