package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// Issue #41 adds a minimal GET /streaming WebSocket stub so Aria stops
// surfacing a connection error every time it opens a timeline tab (Issue
// #1, tracked as a real-world symptom of the deliberate "Streaming is
// not an MVP requirement" decision in docs/compat/aria-v1.5.11.md). At
// the time, it intentionally never pushed a real note/notification
// event; it only made the handshake succeed and answered the small set
// of client control messages Aria's pinned misskey_dart commit
// (docs/compat/aria-v1.5.11.md) actually sends, matching the
// nananek/sakurasato precedent AGENTS.md names as a behavioral reference
// for this exact problem (its Issue #170: "Aria UI が『接続中…』で
// hang しないため"). Since Issue #95 PR2, a homeTimeline-subscribed
// connection does receive a real "note" create push (see
// pushHomeTimelineEvents below) — every other event type (reaction,
// notification, mention, renote, ...) remains permanently unpushed, a
// deliberate non-goal rather than deferred work.
//
// Message shapes below (connect/disconnect/subNote/unsubNote, the
// "connected" ack) were confirmed against misskey_dart's pinned commit
// (lib/src/services/streaming_service_impl.dart,
// lib/src/enums/streaming_request_type.dart) and Aria's pinned commit
// (lib/provider/streaming/timeline_stream_provider.dart), not guessed
// from sakurasato alone.

// defaultStreamPingInterval is how often handleStreaming sends a
// WebSocket ping once a connection is established, matching real
// Misskey server behavior and the sakurasato precedent's stated reason
// (keeping the connection alive through an idle-timeout intermediary
// such as Tailscale or cloudflared).
const defaultStreamPingInterval = 30 * time.Second

// streamPongGraceMultiplier sets how long handleStreaming waits for a
// pong before treating a connection as dead, as a multiple of the ping
// interval: pongWait = pingInterval * streamPongGraceMultiplier. Two
// intervals tolerates one lost/delayed ping without a false-positive
// disconnect, while still bounding a truly dead connection's lifetime to
// a small, fixed multiple of the ping interval rather than leaving it
// unbounded (AGENTS.md: "Bound request sizes, timeouts, concurrency").
// sakurasato's own pong timeout value is not documented, so this is a
// new decision made for this Go implementation rather than a ported one.
const streamPongGraceMultiplier = 2

// streamWriteWait bounds how long a single WebSocket control-frame or
// JSON write may block. It is independent of, and much shorter than,
// this service's configured HTTP WriteTimeout: after Hijack (see
// handleStreaming), net/http no longer enforces any deadline on this
// connection at all, so handleStreaming must set its own before every
// write or a slow/stalled client could block a write goroutine forever.
const streamWriteWait = 10 * time.Second

// maxConcurrentStreamConnections bounds how many simultaneous
// GET /streaming connections this process accepts. This deployment has
// exactly one owner (AGENTS.md: single-owner), so legitimate concurrent
// connections are a handful of devices/tabs, not many; the bound exists
// to satisfy AGENTS.md's general concurrency-bounding rule against a
// buggy or misbehaving client opening connections in a loop, not because
// real usage is expected to approach it.
//
// Raised from 8 to 32 (Issue #95 PR1): the leading hypothesis for
// observed 503s with a single real client is that Aria's reconnect
// interval (5s after a failed connection) outpaces this server's dead-
// connection detection (pongWait = pingInterval * 2, default 60s), so a
// flaky network period can pile up several zombie connections from one
// device before the oldest ones are noticed as dead. 32 gives that a lot
// more headroom while still bounding a misbehaving client's connection
// count, per the same rule this const exists to satisfy.
const maxConcurrentStreamConnections = 32

// streamReadLimit bounds a single incoming WebSocket message. Every
// message this handler understands (connect/disconnect/subNote/
// unsubNote) is a small JSON control frame; this generously covers any
// of them while still giving a misbehaving client's message an
// unsurprising, small, fixed ceiling (AGENTS.md: "Bound request sizes").
const streamReadLimit = 8 * 1024

// streamUpgrader is safe for concurrent, repeated use across requests
// (gorilla/websocket's documented usage pattern), so it is a package
// value rather than built fresh per request. CheckOrigin always allows:
// Aria is a native client, not a browser page this service serves, so
// there is no browser-origin CSRF-style concern to check, and this
// endpoint requires a valid bearer-equivalent token (see handleStreaming)
// before upgrading regardless of Origin.
var streamUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// handleStreaming serves GET /streaming: Misskey-compatible WebSocket
// streaming. See this file's package-level doc comment for scope and
// non-goals.
func (s *Server) handleStreaming(w http.ResponseWriter, r *http.Request) {
	if _, err := verifyTokenFromQuery(r.Context(), s.miauth, r, miauth.ScopeReadAccount); err != nil {
		if errors.Is(err, miauth.ErrTokenInvalid) {
			http.Error(w, "authentication failed", http.StatusUnauthorized)
			return
		}
		s.logger.Error("streaming token verification failed",
			"request_id", logging.RequestIDFromContext(r.Context()),
			"error", err.Error(),
		)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	select {
	case s.streamSem <- struct{}{}:
	default:
		s.logger.Warn("streaming connection rejected: concurrency limit reached",
			"request_id", logging.RequestIDFromContext(r.Context()),
			"limit", maxConcurrentStreamConnections,
		)
		http.Error(w, "too many concurrent streaming connections", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-s.streamSem }()

	conn, err := streamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote its own error response to w on failure
		// (gorilla/websocket's documented behavior); nothing else to do.
		return
	}
	defer conn.Close()

	// Upgrade just wrote "101 Switching Protocols" directly to the
	// hijacked connection, bypassing statusRecorder entirely (see
	// logging.HijackStatusSetter's doc comment) — without this, the
	// eventual AccessLog completion line for this connection would
	// misreport the default 200.
	if setter, ok := w.(logging.HijackStatusSetter); ok {
		setter.SetHijackedStatus(http.StatusSwitchingProtocols)
	}

	s.serveStreamConn(conn, s.streamPingInterval, logging.RequestIDFromContext(r.Context()))
}

// streamEnvelope is the generic Misskey streaming wire envelope every
// client-to-server frame this handler reads uses: {"type": ..., "body":
// ...}. See streaming_service_impl.dart's sendRequest and
// timeline_stream_provider.dart's raw JSON sends (both pinned commits,
// docs/compat/aria-v1.5.11.md) for the traced source.
type streamEnvelope struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// streamConnectBody is "connect"'s body shape: {"channel", "id",
// "params"}. params is read by nothing here — Issue #95 PR2's push
// delivery sends every homeTimeline note to every id subscribed to that
// channel, unfiltered — but is accepted and ignored rather than
// rejected, matching this endpoint's overall "never fail on a frame
// shape it wasn't specifically built for" stance.
type streamConnectBody struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
}

// streamIDBody covers "disconnect"/"subNote"/"unsubNote", all of which
// carry only {"id": ...} (subNote/unsubNote's "params" is likewise
// accepted-and-ignored; see streamConnectBody).
type streamIDBody struct {
	ID string `json:"id"`
}

// connectedAck is the reply "connect" gets, per the Misskey streaming
// protocol and the sakurasato precedent: an ack is returned even for a
// channel name this stub does not recognize. Body.ID is a pointer so a
// connect frame with no id round-trips as JSON null rather than "",
// matching sakurasato's `id.map_or(JsonValue::Null, ...)`.
type connectedAck struct {
	Type string           `json:"type"`
	Body connectedAckBody `json:"body"`
}

type connectedAckBody struct {
	ID *string `json:"id"`
}

// streamConnState tracks one /streaming connection's subscriptions in
// memory. Deliberately minimal (AGENTS.md: no premature abstraction) —
// no separate package, no persistence, no channel-name validation.
// channels/notes were originally written and read only by
// serveStreamConn's single read-loop goroutine; since Issue #95 PR2,
// homeTimelineIDs also reads channels from the concurrent push-delivery
// goroutine (pushHomeTimelineEvents), so every access now goes through
// mu.
type streamConnState struct {
	mu       sync.Mutex
	channels map[string]string   // connect body.id -> channel name
	notes    map[string]struct{} // subscribed note IDs (opaque strings — AGENTS.md: "treat ... Misskey IDs as opaque strings")
}

// homeTimelineIDs returns every connect id currently subscribed to the
// "homeTimeline" channel on this connection — the only channel Issue
// #95 PR2 ever pushes a note to (docs/compat/aria-v1.5.11.md's traced
// push shape). Safe for concurrent use with handleMessage.
func (c *streamConnState) homeTimelineIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for id, channel := range c.channels {
		if channel == "homeTimeline" {
			ids = append(ids, id)
		}
	}
	return ids
}

// handleMessage applies one client-to-server frame to c and returns a
// reply to send, if any. Unrecognized or malformed frames are silently
// ignored rather than erroring the connection: Issue #41 exists
// specifically to stop /streaming from surfacing errors to Aria, so
// failing on a message shape this stub does not yet know about would
// reintroduce exactly that failure mode.
func (c *streamConnState) handleMessage(raw []byte) (reply any, ok bool) {
	var env streamEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}
	switch env.Type {
	case "connect":
		var body streamConnectBody
		_ = json.Unmarshal(env.Body, &body)
		if body.ID != "" {
			c.mu.Lock()
			c.channels[body.ID] = body.Channel
			c.mu.Unlock()
		}
		return connectedAck{Type: "connected", Body: connectedAckBody{ID: nonEmptyStringPtr(body.ID)}}, true
	case "disconnect":
		var body streamIDBody
		_ = json.Unmarshal(env.Body, &body)
		c.mu.Lock()
		delete(c.channels, body.ID)
		c.mu.Unlock()
		return nil, false
	case "subNote", "sn":
		var body streamIDBody
		_ = json.Unmarshal(env.Body, &body)
		if body.ID != "" {
			c.mu.Lock()
			c.notes[body.ID] = struct{}{}
			c.mu.Unlock()
		}
		return nil, false
	case "unsubNote", "un":
		var body streamIDBody
		_ = json.Unmarshal(env.Body, &body)
		c.mu.Lock()
		delete(c.notes, body.ID)
		c.mu.Unlock()
		return nil, false
	default:
		// Includes "readNotification" and "channel"/"ch", which
		// misskey_dart's enum defines but this stub has no real feature
		// behind yet, plus anything a future client version adds.
		return nil, false
	}
}

func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// serveStreamConn drives one upgraded WebSocket connection until the
// client disconnects or a read/write fails: a read loop that answers
// client control messages, and a ping loop that keeps the connection
// alive. It owns conn's read/write deadlines outright from here on: Go's
// net/http clears any deadline it had set (opts.ReadTimeout/WriteTimeout,
// see run.go's newHTTPServer) the moment a handler hijacks the
// connection (net/http.conn.hijackLocked calls rwc.SetDeadline(time.Time{})),
// so a hijacked connection starts with *no* deadline at all rather than
// inheriting a stale per-request one. Without this function installing
// its own, a genuinely dead peer (network partition, crashed client)
// would never be noticed — the read loop and its ping goroutine would
// block/tick forever, leaking one goroutine pair and one
// maxConcurrentStreamConnections slot per such connection (AGENTS.md:
// "Bound request sizes, timeouts, concurrency").
//
// requestID is used only to record how the connection ended (see
// logStreamDisconnect) — Issue #95 PR1 observability for the
// pong-timeout zombie-connection hypothesis (maxConcurrentStreamConnections'
// doc comment). Since Issue #95 PR2, this is also where a homeTimeline
// subscription starts receiving live note pushes (see
// pushHomeTimelineEvents) whenever s.streamHub is configured.
func (s *Server) serveStreamConn(conn *websocket.Conn, pingInterval time.Duration, requestID string) {
	pongWait := pingInterval * streamPongGraceMultiplier
	conn.SetReadLimit(streamReadLimit)
	connectedAt := time.Now()

	var writeMu sync.Mutex // gorilla/websocket: at most one concurrent writer per connection
	writePing := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(streamWriteWait))
		return conn.WriteMessage(websocket.PingMessage, nil)
	}
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(streamWriteWait))
		return conn.WriteJSON(v)
	}

	if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if writePing() != nil {
					return
				}
			}
		}
	}()

	var endErr error
	defer func() { logStreamDisconnect(s.logger, requestID, endErr, time.Since(connectedAt)) }()

	state := &streamConnState{channels: map[string]string{}, notes: map[string]struct{}{}}

	// s.timeline is nil-checked too, not just s.streamHub: GET /streaming
	// registers whenever opts.MiAuthService is set, independent of
	// opts.TimelineService (see NewServer's route registration), so a
	// theoretical Options{StreamHub: ..., TimelineService: nil}
	// misconfiguration must not panic pushHomeTimelineEvents' use of
	// s.timeline. cmd/server/main.go always wires the two together.
	if s.streamHub != nil && s.timeline != nil {
		sub, unsubscribe := s.streamHub.Subscribe()
		defer unsubscribe()
		go s.pushHomeTimelineEvents(sub, state, writeJSON)
	}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			endErr = err
			return // client disconnect, read-deadline expiry, or protocol error
		}
		reply, ok := state.handleMessage(raw)
		if !ok {
			continue
		}
		if err := writeJSON(reply); err != nil {
			endErr = err
			return
		}
	}
}

// logStreamDisconnect records why one /streaming connection ended, for
// Issue #95 PR1's zombie-connection observability goal
// (maxConcurrentStreamConnections' doc comment). A clean client-initiated
// close (WebSocket close code 1000/1001) is expected, routine behavior —
// not logged, to avoid drowning genuinely interesting lines in noise
// every time Aria backgrounds or a tab closes. Anything else, including a
// read-deadline timeout (the leading zombie-connection hypothesis: a
// missed pong), is logged at Warn so operators can correlate this with
// 503s from the concurrency limit above.
func logStreamDisconnect(logger *slog.Logger, requestID string, err error, connected time.Duration) {
	if err == nil || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return
	}
	msg := "streaming connection ended abnormally"
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		msg = "streaming connection timed out waiting for a pong"
	}
	logger.Warn(msg,
		"request_id", requestID,
		"error", err.Error(),
		"connected_ms", connected.Milliseconds(),
	)
}

// streamPushProjectTimeout bounds one push event's owner-resolution and
// note-projection work (projectPushNote): this runs from a background
// goroutine with no request deadline of its own to inherit, so it needs
// its own bound (AGENTS.md: "Bound request sizes, timeouts, concurrency.
// Propagate context cancellation.") rather than being able to hang
// indefinitely on a stuck database call.
const streamPushProjectTimeout = 5 * time.Second

// channelNoteFrame is the traced server→client push shape for a new
// home-timeline note (docs/compat/aria-v1.5.11.md's "Traced server→client
// channel/note push event shape" section): misskey_dart's
// StreamingResponse "channel" case wrapping its own ChannelStreamEvent
// "note" case.
type channelNoteFrame struct {
	Type string               `json:"type"`
	Body channelNoteFrameBody `json:"body"`
}

type channelNoteFrameBody struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Body note   `json:"body"`
}

// newChannelNoteFrame builds the push frame for n, addressed to the
// connect id that subscribed to homeTimeline.
func newChannelNoteFrame(id string, n note) channelNoteFrame {
	return channelNoteFrame{Type: "channel", Body: channelNoteFrameBody{ID: id, Type: "note", Body: n}}
}

// pushHomeTimelineEvents delivers Issue #95 PR2's live note-create push
// to one /streaming connection: for every domain.Entry sub receives, it
// projects the entry once and sends a channelNoteFrame to every connect
// id state currently has subscribed to "homeTimeline". It returns when
// sub is closed — serveStreamConn's deferred unsubscribe, itself tied to
// the connection's own lifetime via s.streamHub.Subscribe — or as soon
// as a write fails (the read loop will independently notice the same
// dead connection and clean everything up).
//
// Any failure here (no homeTimeline subscriber on this connection, note
// projection failing) is silently dropped rather than logged or
// surfaced: a failed push must never affect this or any other
// connection, this file's non-goal since Issue #41, and matches
// AGENTS.md's "local post success must not depend on provider success"
// applied to this best-effort delivery instead.
func (s *Server) pushHomeTimelineEvents(sub <-chan domain.Entry, state *streamConnState, writeJSON func(any) error) {
	for entry := range sub {
		ids := state.homeTimelineIDs()
		if len(ids) == 0 {
			continue
		}
		n, err := s.projectPushNote(entry)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if writeJSON(newChannelNoteFrame(id, n)) != nil {
				return
			}
		}
	}
}

// projectPushNote builds the wire Note for a just-created entry, for
// pushHomeTimelineEvents. It resolves the owner itself via s.timeline/
// s.miauth rather than accepting a caller-supplied owner profile, since
// it runs from a background goroutine with no HTTP request/context to
// reuse (plan-issue-95 §3.4: owner resolution "リクエストコンテキストが
// 無いbackgroundゴルーチンからでも呼べる、既存メソッドの組み合わせ").
// viewerActorID is always the owner: a just-created entry can have no
// reaction yet, so projectNote's MyReaction is always nil regardless of
// who the real eventual viewer is.
func (s *Server) projectPushNote(entry domain.Entry) (note, error) {
	ctx, cancel := context.WithTimeout(context.Background(), streamPushProjectTimeout)
	defer cancel()

	ownerActor, err := s.timeline.GetActorByType(ctx, domain.ActorOwner)
	if err != nil {
		return note{}, err
	}
	owner, err := s.miauth.DescribeOwner(ctx, ownerActor.ID)
	if err != nil {
		return note{}, err
	}
	user := s.resolveUserLite(ctx, entry.AuthorActorID, owner)
	return s.projectNote(ctx, entry, user, owner.ActorID)
}
