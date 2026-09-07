// Package streamhub is a small, neutral in-process publish/subscribe
// broker that fans a newly committed domain.Entry out to whatever is
// currently listening. It exists so internal/timeline and
// internal/httpserver can both depend on it instead of one depending on
// the other (AGENTS.md: "Domain/use-case code must not depend on HTTP
// handlers") without a construction-order cycle in cmd/server/main.go
// (httpserver.NewServer needs timeline.Service, and timeline.NewService
// would need a broadcaster satisfied by something httpserver owns).
// This package depends only on internal/domain.
//
// There is no persistence and no external transport: this deployment is
// a single process (AGENTS.md: single-owner, no clustering), so an
// external pub/sub broker (Redis or similar) would add operational cost
// with no corresponding benefit. A dropped or missed push is never a
// correctness problem — see Hub.Publish's doc comment — only a UX
// miss the caller's own HTTP timeline poll/reload always corrects
// (docs/compat/aria-v1.5.11.md's "Streaming decision").
package streamhub

import (
	"sync"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// subscriberBufferSize bounds how many not-yet-delivered domain.Entry
// values one Subscribe call's channel holds before Publish starts
// dropping the oldest to make room for the newest (AGENTS.md: "Bound
// request sizes, timeouts, concurrency"). A slow consumer (a stalled
// WebSocket write, a client that stopped reading) can never block
// Publish or grow memory unboundedly; it only misses older pushes.
const subscriberBufferSize = 32

// Hub fans out every Published domain.Entry to every current
// subscriber. The zero value is not usable; construct one with NewHub.
// A Hub is safe for concurrent use by multiple goroutines.
type Hub struct {
	mu   sync.Mutex
	subs map[int]chan domain.Entry
	next int
}

// NewHub returns a ready-to-use Hub with no subscribers.
func NewHub() *Hub {
	return &Hub{subs: make(map[int]chan domain.Entry)}
}

// Publish fans entry out to every current subscriber's channel. It must
// never block the caller — internal/timeline calls this only after its
// own transaction has already committed, as a best-effort side effect
// (see internal/timeline's EntryBroadcaster doc comment), not something
// a slow or stuck subscriber may delay. Every send is therefore
// non-blocking: a subscriber whose buffered channel is already full has
// its oldest entry dropped to make room for this new one instead, so
// Publish always keeps making progress and a subscriber never falls
// permanently behind the very latest event, only some in between.
func (h *Hub) Publish(entry domain.Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- entry:
		default:
			// Full: drop the oldest buffered entry to make room, then
			// retry once. Both steps stay non-blocking (default cases),
			// so a pathological interleaving simply drops this delivery
			// rather than ever blocking Publish.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- entry:
			default:
			}
		}
	}
}

// Subscribe registers a new listener and returns a receive-only channel
// of every entry Published from now on, plus a cancel func the caller
// must call exactly once (typically via defer) to unregister and
// release it. cancel closes the channel after removing it from h, so a
// caller ranging over the channel sees a clean close, never a
// send-on-closed-channel panic racing a concurrent Publish (both
// Publish and cancel hold h's lock for their whole operation).
func (h *Hub) Subscribe() (ch <-chan domain.Entry, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	sub := make(chan domain.Entry, subscriberBufferSize)
	h.subs[id] = sub
	cancelled := false
	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cancelled {
			return
		}
		cancelled = true
		delete(h.subs, id)
		close(sub)
	}
	return sub, cancel
}
