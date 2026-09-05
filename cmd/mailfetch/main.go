// Command mailfetch is the untrusted IMAP/MIME processing sidecar
// docs/decisions/0003-imap-mailfetch-isolation.md (ADR-0003) requires
// Issue #12's IMAP ingestion to run outside cmd/server's process. It has
// no configuration of its own beyond where to listen: IMAP host, port,
// TLS mode, credentials, and mailbox all travel per-request from
// cmd/server over the Unix domain socket (see internal/mailfetch/rpc),
// never as this process's own environment or command-line arguments, so
// this binary genuinely holds no long-lived secret of its own.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/mailfetch"
	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
)

// defaultSocketPath matches internal/config.IMAPConfig.MailfetchSocket's
// own default, so a deployment that overrides neither side still lines
// up without any extra configuration.
const defaultSocketPath = "/run/mailfetch/mailfetch.sock"

// maxConcurrentFetches bounds how many IMAP fetches this process runs at
// once. A fixed internal constant, not a config key: Issue #12 only ever
// wires up one IMAP source, so there is no realistic deployment that
// needs to tune this.
const maxConcurrentFetches = 4

// requestReadTimeout bounds how long handleConn waits to receive a
// complete request frame after accepting a connection, independent of
// the request's own FetchTimeoutMs (which only starts once the request
// has actually been read) — a defense against a connected-but-silent
// peer holding a slot open indefinitely.
const requestReadTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	socketPath := getenvDefault("MAILFETCH_SOCKET_PATH", defaultSocketPath)
	logger := logging.New(os.Stdout, logging.Config{
		Level:  getenvDefault("MAILFETCH_LOG_LEVEL", "info"),
		Format: getenvDefault("MAILFETCH_LOG_FORMAT", "text"),
	})
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l, err := listen(socketPath)
	if err != nil {
		return fmt.Errorf("mailfetch: listen on %s: %w", socketPath, err)
	}
	defer l.Close()
	logger.Info("mailfetch listening", "socket", socketPath)

	return serve(ctx, l, logger)
}

// listen creates socketPath's parent directory if missing, removes a
// stale socket file left by a previous, uncleanly-terminated run
// (cmd/mailfetch is a single-instance service: there is never a second,
// legitimate listener to protect by leaving a pre-existing file alone),
// and restricts the socket's permissions to this process's own user —
// the filesystem-permission access-control boundary ADR-0003 relies on.
func listen(socketPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return l, nil
}

// serve accepts connections until ctx is cancelled, running up to
// maxConcurrentFetches of them at once, and waits for every already-
// accepted connection to finish before returning.
func serve(ctx context.Context, l net.Listener, logger *slog.Logger) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	sem := make(chan struct{}, maxConcurrentFetches)
	var wg sync.WaitGroup
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			logger.Warn("mailfetch: accept failed", "error_category", "accept_error")
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			handleConn(ctx, conn, logger)
		}()
	}
	wg.Wait()
	return nil
}

// handleConn serves exactly one request/response pair per
// docs/decisions/0003-imap-mailfetch-isolation.md's protocol shape, then
// closes conn.
func handleConn(ctx context.Context, conn net.Conn, logger *slog.Logger) {
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	var req rpc.Request
	if err := rpc.ReadFrame(conn, &req); err != nil {
		logger.Warn("mailfetch: read request failed", "error_category", "framing_error")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	resp := mailfetch.Fetch(ctx, req)
	if resp.Error != nil {
		logger.Warn("mailfetch: fetch failed", "source_id", req.SourceID, "error_category", resp.Error.Category)
	} else {
		logger.Info("mailfetch: fetch completed", "source_id", req.SourceID, "items_fetched", len(resp.Items))
	}

	if err := rpc.WriteFrame(conn, resp); err != nil {
		logger.Warn("mailfetch: write response failed", "error_category", "framing_error")
	}
}

func getenvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
