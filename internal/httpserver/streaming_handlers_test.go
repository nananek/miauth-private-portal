package httpserver

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// newStreamingTestServer starts a real Server through Run on an
// ephemeral TCP listener — not httptest.NewRecorder() (its
// ResponseWriter does not implement http.Hijacker at all) and not
// httptest.NewServer() (it builds its own bare *http.Server with no
// ReadTimeout/WriteTimeout, which would skip over exactly the
// interaction between this service's configured HTTP timeouts and a
// hijacked long-lived connection that streaming_handlers.go's deadline
// handling exists to get right). It mirrors run_test.go's mustListen /
// waitForServing pattern.
//
// It returns the server's address and a valid API token with the given
// scope, issued through a throwaway Server sharing the same
// miauth.Service/database Run's internal Server will authenticate
// against.
func newStreamingTestServer(t *testing.T, opts Options, tokenScope string) (addr, token string) {
	t.Helper()
	if tokenScope == "" {
		tokenScope = miauth.ScopeReadAccount
	}

	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second, MaxOpenConns: 4,
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}

	miauthSvc := miauth.NewService(db, db.Repos, defaultMiAuthTestConfig())
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()

	setupSrv := NewServer(logger, reg, Options{MiAuthService: miauthSvc})
	token, _ = mustIssueToken(t, setupSrv, "streaming-setup", tokenScope)

	ln := mustListen(t)
	addr = ln.Addr().String()

	opts.Listener = ln
	opts.MiAuthService = miauthSvc
	if opts.ShutdownGracePeriod == 0 {
		opts.ShutdownGracePeriod = 2 * time.Second
	}
	if opts.ReadHeaderTimeout == 0 {
		opts.ReadHeaderTimeout = 2 * time.Second
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 2 * time.Second
	}
	if opts.MaxRequestBodyBytes == 0 {
		opts.MaxRequestBodyBytes = 1024
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts, logger, reg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned error during shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server did not shut down")
		}
	})

	waitForServing(t, addr)
	return addr, token
}

func dialStreaming(url string) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.Dial(url, nil)
}

func respStatus(resp *http.Response) int {
	if resp == nil {
		return -1
	}
	return resp.StatusCode
}

func isTimeoutErr(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func TestHandleStreaming_ValidTokenUpgradesAndAcksConnect(t *testing.T) {
	addr, token := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type": "connect",
		"body": map[string]any{"channel": "homeTimeline", "id": "abc123", "params": map[string]any{}},
	}); err != nil {
		t.Fatalf("write connect: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack struct {
		Type string `json:"type"`
		Body struct {
			ID *string `json:"id"`
		} `json:"body"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read connected ack: %v", err)
	}
	if ack.Type != "connected" {
		t.Errorf("ack type = %q, want %q", ack.Type, "connected")
	}
	if ack.Body.ID == nil || *ack.Body.ID != "abc123" {
		t.Errorf("ack body.id = %v, want %q", ack.Body.ID, "abc123")
	}
}

func TestHandleStreaming_RejectsMissingOrInvalidTokenBeforeUpgrade(t *testing.T) {
	addr, _ := newStreamingTestServer(t, Options{}, "")

	cases := []struct {
		name string
		url  string
	}{
		{"missing token", "ws://" + addr + "/streaming"},
		{"garbage token", "ws://" + addr + "/streaming?i=not-a-real-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, err := dialStreaming(tc.url)
			if err == nil {
				conn.Close()
				t.Fatal("expected dial to fail for an unauthenticated /streaming request")
			}
			if respStatus(resp) != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", respStatus(resp), http.StatusUnauthorized)
			}
		})
	}
}

func TestHandleStreaming_RejectsTokenMissingRequiredScope(t *testing.T) {
	// write:notes only, deliberately without read:account.
	addr, token := newStreamingTestServer(t, Options{}, miauth.ScopeWriteNotes)

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err == nil {
		conn.Close()
		t.Fatal("expected dial to fail for a token lacking read:account scope")
	}
	if respStatus(resp) != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", respStatus(resp), http.StatusUnauthorized)
	}
}

func TestHandleStreaming_SendsPeriodicPingAndSurvivesConfiguredWriteTimeout(t *testing.T) {
	const pingInterval = 200 * time.Millisecond
	addr, token := newStreamingTestServer(t, Options{
		StreamPingInterval: pingInterval,
		// Deliberately shorter than this test's runtime: proves
		// handleStreaming's own deadline management (streaming_handlers.go)
		// supersedes net/http's pre-hijack deadline instead of inheriting it.
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
	}, "")

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	defer conn.Close()

	pinged := make(chan struct{}, 1)
	conn.SetPingHandler(func(appData string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		// WriteControl, not WriteMessage: a ping handler runs on the
		// background reader goroutine below, concurrently with this
		// test's own WriteJSON call on the main goroutine. gorilla/
		// websocket documents WriteControl (unlike WriteMessage) as safe
		// to call concurrently with other writes for exactly this
		// pattern; using WriteMessage here raced under `go test -race`.
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(streamWriteWait))
	})

	// gorilla/websocket only dispatches control frames (ping) to the
	// registered handler while a read is in flight, so a background
	// reader is required for the ping handler above to ever fire.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-pinged:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive a ping within the expected interval")
	}

	// Outlive the deliberately-short ReadTimeout/WriteTimeout above by a
	// wide margin.
	time.Sleep(500 * time.Millisecond)

	if err := conn.WriteJSON(map[string]any{"type": "disconnect", "body": map[string]any{"id": "x"}}); err != nil {
		t.Fatalf("connection died before an explicit close: %v", err)
	}
}

func TestHandleStreaming_SubscriptionMessagesProduceNoReply(t *testing.T) {
	addr, token := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")
	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	defer conn.Close()

	messages := []map[string]any{
		{"type": "subNote", "body": map[string]any{"id": "note1", "params": map[string]any{}}},
		{"type": "unsubNote", "body": map[string]any{"id": "note1", "params": map[string]any{}}},
		{"type": "disconnect", "body": map[string]any{"id": "abc123"}},
	}
	for _, msg := range messages {
		if err := conn.WriteJSON(msg); err != nil {
			t.Fatalf("write %v: %v", msg, err)
		}
	}

	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected no reply for subNote/unsubNote/disconnect, but got a message")
	} else if !isTimeoutErr(err) {
		t.Fatalf("expected a read-deadline timeout, got: %v", err)
	}
}

func TestHandleStreaming_UnknownMessageTypeIsIgnoredNotErrored(t *testing.T) {
	addr, token := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")
	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{"type": "somethingFutureAriaSends", "body": map[string]any{}}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The connection must survive an unrecognized frame (this issue's
	// entire point). Prove it is still alive by round-tripping a connect
	// afterward.
	if err := conn.WriteJSON(map[string]any{
		"type": "connect",
		"body": map[string]any{"channel": "homeTimeline", "id": "still-alive", "params": map[string]any{}},
	}); err != nil {
		t.Fatalf("write connect after unknown message: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read connected ack after unknown message: %v", err)
	}
	if ack.Type != "connected" {
		t.Errorf("ack type = %q, want %q", ack.Type, "connected")
	}
}

func TestHandleStreaming_RejectsConnectionsBeyondConcurrencyLimit(t *testing.T) {
	addr, token := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

	var conns []*websocket.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < maxConcurrentStreamConnections; i++ {
		conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
		if err != nil {
			t.Fatalf("dial %d: %v (status=%d)", i, err, respStatus(resp))
		}
		conns = append(conns, conn)
	}

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err == nil {
		conn.Close()
		t.Fatal("expected the connection beyond the concurrency limit to be rejected")
	}
	if respStatus(resp) != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", respStatus(resp), http.StatusServiceUnavailable)
	}
}

func TestHandleStreaming_ClosingConnectionsReleasesGoroutinesAndSemaphoreSlots(t *testing.T) {
	addr, token := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

	before := runtime.NumGoroutine()

	for i := 0; i < maxConcurrentStreamConnections; i++ {
		conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
		if err != nil {
			t.Fatalf("dial %d: %v (status=%d)", i, err, respStatus(resp))
		}
		conn.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= before+2 { // small tolerance for scheduler/runtime noise
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not settle: before=%d now=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Prove the semaphore slots were actually released (not merely that
	// goroutine count happened to settle) by successfully opening a full
	// new batch now that the earlier connections are gone.
	var conns []*websocket.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < maxConcurrentStreamConnections; i++ {
		conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
		if err != nil {
			t.Fatalf("dial after cleanup %d: %v (status=%d)", i, err, respStatus(resp))
		}
		conns = append(conns, conn)
	}
}
