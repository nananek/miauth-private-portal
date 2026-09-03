package httpserver

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

func TestNewHTTPServer_AppliesTimeoutsFromOptions(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	opts := Options{
		ReadTimeout:       1 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       4 * time.Second,
	}

	srv := newHTTPServer(context.Background(), opts, http.NewServeMux(), logger)

	if srv.ReadTimeout != opts.ReadTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, opts.ReadTimeout)
	}
	if srv.ReadHeaderTimeout != opts.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, opts.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != opts.WriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, opts.WriteTimeout)
	}
	if srv.IdleTimeout != opts.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, opts.IdleTimeout)
	}
}

func TestRun_ListenErrorReturnsImmediately(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := Run(ctx, Options{Addr: "not-a-valid-address"}, logger, reg); err == nil {
		t.Fatal("expected error for an invalid listen address")
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func waitForServing(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server at %s never started accepting connections", addr)
}

func testOptions(ln net.Listener) Options {
	return Options{
		Listener:            ln,
		ReadTimeout:         2 * time.Second,
		ReadHeaderTimeout:   2 * time.Second,
		WriteTimeout:        2 * time.Second,
		IdleTimeout:         2 * time.Second,
		MaxRequestBodyBytes: 1024,
		ShutdownGracePeriod: 2 * time.Second,
	}
}

func TestRun_ServesHealthzAndReadyzAndMarksReady(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()

	ln := mustListen(t)
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, testOptions(ln), logger, reg) }()

	waitForServing(t, addr)

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	resp, err = http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz status = %d, want %d (Run should MarkReady on startup)", resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func TestRun_CtxCancelMarksRegistryNotReady(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()

	ln := mustListen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, testOptions(ln), logger, reg) }()

	waitForServing(t, ln.Addr().String())
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if err := reg.Ready(context.Background()); err == nil {
		t.Error("expected registry to be marked not-ready after shutdown")
	}
}

type blockingChecker struct {
	release chan struct{}
}

func (b *blockingChecker) Name() string { return "blocking" }

func (b *blockingChecker) Check(_ context.Context) error {
	<-b.release
	return nil
}

func TestRun_GracefulShutdownWaitsForInFlightRequest(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	release := make(chan struct{})
	reg.Register(&blockingChecker{release: release})

	ln := mustListen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, testOptions(ln), logger, reg) }()

	waitForServing(t, addr)

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	// Give the in-flight request time to reach the blocking checker before
	// triggering shutdown.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Shutdown must wait for the in-flight request instead of cutting it
	// off; release it shortly after triggering shutdown to prove that.
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case resp := <-respCh:
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("in-flight /readyz status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
	case err := <-errCh:
		t.Fatalf("in-flight request failed instead of completing gracefully: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after graceful shutdown")
	}
}

type ctxCapturingChecker struct {
	release  chan struct{}
	sawErrCh chan error
}

func (c *ctxCapturingChecker) Name() string { return "ctx-capturing" }

func (c *ctxCapturingChecker) Check(ctx context.Context) error {
	<-c.release
	c.sawErrCh <- ctx.Err()
	return nil
}

func TestRun_InFlightRequestContextNotCancelledDuringGracePeriod(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	release := make(chan struct{})
	sawErrCh := make(chan error, 1)
	reg.Register(&ctxCapturingChecker{release: release, sawErrCh: sawErrCh})

	ln := mustListen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, testOptions(ln), logger, reg) }()

	waitForServing(t, addr)

	go func() {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Give the in-flight request time to reach the blocking checker
	// before triggering shutdown.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Release the checker while shutdown is in progress but well within
	// the grace period, and assert its request context was not already
	// cancelled merely because the shutdown signal fired.
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case err := <-sawErrCh:
		if err != nil {
			t.Errorf("in-flight request context.Err() = %v, want nil during the shutdown grace period", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checker never observed release")
	}

	<-done
}

func TestRun_ForcedCloseAfterGracePeriodExceeded(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	release := make(chan struct{})
	defer close(release)
	reg.Register(&blockingChecker{release: release})

	ln := mustListen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	opts := testOptions(ln)
	opts.ShutdownGracePeriod = 100 * time.Millisecond
	go func() { done <- Run(ctx, opts, logger, reg) }()

	waitForServing(t, addr)

	go func() {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err == nil {
			resp.Body.Close()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Run must return within a bounded time even though the blocking
		// checker never releases on its own; the forced Close fallback
		// bounds shutdown time to roughly the grace period.
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within a bounded time after the grace period was exceeded")
	}

	if !strings.Contains(buf.String(), "forcing close") {
		t.Errorf("expected a forced-close log line, got: %s", buf.String())
	}
}

// erroringListener always fails Accept, simulating srv.Serve returning a
// non-ErrServerClosed error asynchronously after Run has already started
// serving.
type erroringListener struct {
	addr net.Addr
}

func (l *erroringListener) Accept() (net.Conn, error) { return nil, errors.New("accept boom") }
func (l *erroringListener) Close() error              { return nil }
func (l *erroringListener) Addr() net.Addr            { return l.addr }

func TestRun_ServeErrorStillMarksRegistryNotReady(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()

	realLn := mustListen(t)
	addr := realLn.Addr()
	realLn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := Run(ctx, testOptions(&erroringListener{addr: addr}), logger, reg)
	if err == nil {
		t.Fatal("expected Run to return the Serve error")
	}

	// Run's doc comment promises a graceful shutdown always runs before
	// it returns, even when the failure is the server itself rather than
	// ctx cancellation; that shutdown includes marking the registry
	// not-ready.
	if readyErr := reg.Ready(context.Background()); readyErr == nil {
		t.Error("expected registry to be marked not-ready after a Serve failure")
	}
}

type panickingChecker struct{}

func (panickingChecker) Name() string                  { return "panicking" }
func (panickingChecker) Check(_ context.Context) error { panic("boom") }

func TestRun_PanicInHandlerIsLoggedWithRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	reg.Register(panickingChecker{})

	ln := mustListen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, testOptions(ln), logger, reg) }()

	waitForServing(t, addr)

	// This exercises the real middleware composition wired up by Run:
	// AccessLog -> withRequestID -> withRecover -> the panicking /readyz
	// handler. Both the access log and the panic-recovery log must carry
	// a non-empty request_id, and the client must still see a clean 500
	// rather than a dropped connection.
	resp, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}

	out := buf.String()
	if !strings.Contains(out, "panic recovered") {
		t.Fatalf("expected a panic recovered log line, got: %s", out)
	}
	if !strings.Contains(out, "http_request") {
		t.Errorf("expected AccessLog's completion line even though the handler panicked, got: %s", out)
	}
	if strings.Contains(out, `"request_id":""`) {
		t.Errorf("expected every request_id in the log to be non-empty, got: %s", out)
	}
}
