package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// Issue #41 adds a minimal GET /streaming WebSocket stub so Aria stops
// surfacing a connection error every time it opens a timeline tab (Issue
// #1, tracked as a real-world symptom of the deliberate "Streaming is
// not an MVP requirement" decision in docs/compat/aria-v1.5.11.md). It
// intentionally never pushes a real note/notification event — that
// remains future work — it only makes the handshake succeed and answers
// the small set of client control messages Aria's pinned misskey_dart
// commit (docs/compat/aria-v1.5.11.md) actually sends, matching the
// nananek/sakurasato precedent AGENTS.md names as a behavioral reference
// for this exact problem (its Issue #170: "Aria UI が『接続中…』で
// hang しないため").
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
const maxConcurrentStreamConnections = 8

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

	serveStreamConn(conn, s.streamPingInterval)
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
// "params"}. params is read by nothing here (no real event delivery
// exists yet to filter by it) but is accepted and ignored rather than
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
// no separate package, no persistence, no channel-name validation. This
// handler never pushes a real note/notification event (this file's
// Non-goals), so nothing reads these maps back today; they exist only so
// a future real-event-push feature has an established place to look up
// "which connect id(s) asked for this channel" / "is this note
// subscribed" against, rather than needing its own per-socket state from
// scratch.
type streamConnState struct {
	channels map[string]string   // connect body.id -> channel name
	notes    map[string]struct{} // subscribed note IDs (opaque strings — AGENTS.md: "treat ... Misskey IDs as opaque strings")
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
			c.channels[body.ID] = body.Channel
		}
		return connectedAck{Type: "connected", Body: connectedAckBody{ID: nonEmptyStringPtr(body.ID)}}, true
	case "disconnect":
		var body streamIDBody
		_ = json.Unmarshal(env.Body, &body)
		delete(c.channels, body.ID)
		return nil, false
	case "subNote", "sn":
		var body streamIDBody
		_ = json.Unmarshal(env.Body, &body)
		if body.ID != "" {
			c.notes[body.ID] = struct{}{}
		}
		return nil, false
	case "unsubNote", "un":
		var body streamIDBody
		_ = json.Unmarshal(env.Body, &body)
		delete(c.notes, body.ID)
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
func serveStreamConn(conn *websocket.Conn, pingInterval time.Duration) {
	pongWait := pingInterval * streamPongGraceMultiplier
	conn.SetReadLimit(streamReadLimit)

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

	state := &streamConnState{channels: map[string]string{}, notes: map[string]struct{}{}}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return // client disconnect, read-deadline expiry, or protocol error
		}
		reply, ok := state.handleMessage(raw)
		if !ok {
			continue
		}
		if writeJSON(reply) != nil {
			return
		}
	}
}
