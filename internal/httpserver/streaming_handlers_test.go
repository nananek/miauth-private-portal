package httpserver

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/streamhub"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// syncBuffer is a concurrency-safe io.Writer/String() pair for capturing
// the Server-under-test's log output (see newStreamingTestServer): the
// real Server runs its logger calls on background connection goroutines
// while a test's own goroutine reads the buffer, so a plain bytes.Buffer
// would race under `go test -race` (mirrors internal/integration's
// harness_test.go helper of the same name/shape for the same reason).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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
// It returns the server's address, a valid API token with the given
// scope (issued through a throwaway Server sharing the same
// miauth.Service/database Run's internal Server will authenticate
// against), and the Server-under-test's own log output.
func newStreamingTestServer(t *testing.T, opts Options, tokenScope string) (addr, token string, logs *syncBuffer) {
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

	logs = &syncBuffer{}
	miauthSvc := miauth.NewService(db, db.Repos, defaultMiAuthTestConfig())
	logger := logging.New(logs, logging.Config{Format: "json", Level: "info"})
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
	return addr, token, logs
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
	addr, token, _ := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

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
	addr, _, _ := newStreamingTestServer(t, Options{}, "")

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
	addr, token, _ := newStreamingTestServer(t, Options{}, miauth.ScopeWriteNotes)

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
	addr, token, _ := newStreamingTestServer(t, Options{
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
	addr, token, _ := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")
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
	addr, token, _ := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")
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
	addr, token, logs := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

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

	// Issue #95 PR1 observability: a rejection due to the concurrency
	// limit must be logged so operators can correlate it with 503s.
	if !strings.Contains(logs.String(), "streaming connection rejected: concurrency limit reached") {
		t.Errorf("expected a concurrency-limit warning log, got: %s", logs.String())
	}
}

func TestHandleStreaming_ClosingConnectionsReleasesGoroutinesAndSemaphoreSlots(t *testing.T) {
	addr, token, _ := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

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

// waitForLogLine polls logs until it contains want or the deadline
// passes, returning the final buffer contents either way — a plain
// assertion right after closing/dialing would race the server's
// background connection goroutine, which logs asynchronously.
func waitForLogLine(t *testing.T, logs *syncBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s := logs.String(); strings.Contains(s, want) {
			return s
		}
		if time.Now().After(deadline) {
			return logs.String()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHandleStreaming_LogsAbnormalDisconnect(t *testing.T) {
	addr, token, logs := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	// A bare TCP close, not a WebSocket close handshake — this is the
	// zombie-connection shape the pong-timeout hypothesis
	// (maxConcurrentStreamConnections' doc comment) cares about: the
	// server never sees a clean disconnect signal from the peer.
	conn.Close()

	got := waitForLogLine(t, logs, "streaming connection ended abnormally")
	if !strings.Contains(got, "streaming connection ended abnormally") {
		t.Errorf("expected an abnormal-disconnect warning log, got: %s", got)
	}
}

func TestHandleStreaming_DoesNotLogCleanClientClose(t *testing.T) {
	addr, token, logs := newStreamingTestServer(t, Options{StreamPingInterval: time.Hour}, "")

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	if err := conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(streamWriteWait)); err != nil {
		t.Fatalf("write close: %v", err)
	}
	conn.Close()

	// Give the server ample time to process the close and (if it were
	// buggy) log it, then assert it did not: a routine client-initiated
	// close must not add noise to a log operators rely on for spotting
	// abnormal disconnects.
	time.Sleep(300 * time.Millisecond)
	if got := logs.String(); strings.Contains(got, "streaming connection ended abnormally") ||
		strings.Contains(got, "streaming connection timed out") {
		t.Errorf("expected no disconnect warning for a clean close, got: %s", got)
	}
}

// newStreamingPushTestServer is newStreamingTestServer's Issue #95 PR2
// counterpart: it additionally builds a timeline.Service and
// streamhub.Hub sharing the same database as the running Server (wired
// into Options.TimelineService/StreamHub, and into
// timeline.Config.Broadcaster, exactly as cmd/server/main.go wires
// them), and returns both so a test can create an entry directly and
// observe the resulting push frame over a real WebSocket connection.
func newStreamingPushTestServer(t *testing.T, tokenScope string) (addr, token string, timelineSvc *timeline.Service, hub *streamhub.Hub) {
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

	hub = streamhub.NewHub()
	timelineSvc = timeline.NewService(db, db.Repos, timeline.Config{Broadcaster: hub})

	miauthSvc := miauth.NewService(db, db.Repos, defaultMiAuthTestConfig())
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()

	setupSrv := NewServer(logger, reg, Options{MiAuthService: miauthSvc})
	token, _ = mustIssueToken(t, setupSrv, "streaming-push-setup", tokenScope)

	ln := mustListen(t)
	addr = ln.Addr().String()

	opts := Options{
		Listener:            ln,
		MiAuthService:       miauthSvc,
		TimelineService:     timelineSvc,
		StreamHub:           hub,
		StreamPingInterval:  time.Hour,
		ShutdownGracePeriod: 2 * time.Second,
		ReadHeaderTimeout:   2 * time.Second,
		IdleTimeout:         2 * time.Second,
		MaxRequestBodyBytes: 1024,
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
	return addr, token, timelineSvc, hub
}

func TestHandleStreaming_PushesNewHomeTimelineNoteToSubscribedConnection(t *testing.T) {
	addr, token, timelineSvc, _ := newStreamingPushTestServer(t, "")

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type": "connect",
		"body": map[string]any{"channel": "homeTimeline", "id": "home1", "params": map[string]any{}},
	}); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read connected ack: %v", err)
	}
	if ack.Type != "connected" {
		t.Fatalf("ack type = %q, want %q", ack.Type, "connected")
	}

	entry, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "hello push", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var frame struct {
		Type string `json:"type"`
		Body struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Body struct {
				ID   string  `json:"id"`
				Text *string `json:"text"`
			} `json:"body"`
		} `json:"body"`
	}
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("read push frame: %v", err)
	}
	if frame.Type != "channel" {
		t.Errorf("frame.Type = %q, want %q", frame.Type, "channel")
	}
	if frame.Body.ID != "home1" {
		t.Errorf("frame.Body.ID = %q, want %q (the connect id)", frame.Body.ID, "home1")
	}
	if frame.Body.Type != "note" {
		t.Errorf("frame.Body.Type = %q, want %q", frame.Body.Type, "note")
	}
	if frame.Body.Body.ID != entry.ID {
		t.Errorf("pushed note id = %q, want %q", frame.Body.Body.ID, entry.ID)
	}
	if frame.Body.Body.Text == nil || *frame.Body.Body.Text != "hello push" {
		t.Errorf("pushed note text = %v, want %q", frame.Body.Body.Text, "hello push")
	}
}

func TestHandleStreaming_DoesNotPushToConnectionNotSubscribedToHomeTimeline(t *testing.T) {
	addr, token, timelineSvc, _ := newStreamingPushTestServer(t, "")

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	defer conn.Close()

	// Subscribed to a different channel, not homeTimeline: Issue #95's
	// scope is homeTimeline note-create push only (plan-issue-95 §2).
	if err := conn.WriteJSON(map[string]any{
		"type": "connect",
		"body": map[string]any{"channel": "localTimeline", "id": "local1", "params": map[string]any{}},
	}); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read connected ack: %v", err)
	}

	if _, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "hello", nil); err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected no push frame for a connection not subscribed to homeTimeline")
	} else if !isTimeoutErr(err) {
		t.Fatalf("expected a read-deadline timeout, got: %v", err)
	}
}

// TestHandleStreaming_ReactionDoesNotTriggerPush pins Issue #95's own
// non-goal (a continuation of Issue #41's — this document's WebSocket
// "/streaming timeline channel" row): only a note's own creation is
// pushed. timeline.Service.SetReaction never calls broadcastCreated at
// all, so this is structurally guaranteed rather than merely untested,
// but it is exactly the acceptance criterion plan-issue-95 §5 calls for
// a test on, so it is pinned here against a real subscribed connection.
func TestHandleStreaming_ReactionDoesNotTriggerPush(t *testing.T) {
	addr, token, timelineSvc, _ := newStreamingPushTestServer(t, "")

	entry, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "reactable", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	owner, err := timelineSvc.GetActorByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatalf("GetActorByType: %v", err)
	}

	conn, resp, err := dialStreaming("ws://" + addr + "/streaming?i=" + token)
	if err != nil {
		t.Fatalf("dial: %v (status=%d)", err, respStatus(resp))
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type": "connect",
		"body": map[string]any{"channel": "homeTimeline", "id": "home1", "params": map[string]any{}},
	}); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read connected ack: %v", err)
	}

	if err := timelineSvc.SetReaction(t.Context(), entry.ID, owner.ID, "👍"); err != nil {
		t.Fatalf("SetReaction: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected no push frame for a reaction, only note creation pushes")
	} else if !isTimeoutErr(err) {
		t.Fatalf("expected a read-deadline timeout, got: %v", err)
	}
}
