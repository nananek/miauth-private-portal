package openwebui

import (
	"context"
	"sync"
)

// threadLocks is ADR-0005 D5's per-thread single-flight: an in-process,
// keyed mutex that bounds a thread's concurrently-running remote turns
// to one. This deployment runs one worker process, so an in-process lock
// is sufficient — the roadmap's cross-process case is a future issue's
// concern, not this one's.
//
// Entries are never removed: a long-running deployment accumulates one
// small channel per thread that has ever enqueued a turn. That is an
// accepted, bounded-by-thread-count tradeoff for the MVP rather than an
// oversight — the alternative (reference-counted cleanup) is exactly the
// kind of complexity a single-worker deployment does not need yet.
type threadLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func newThreadLocks() *threadLocks {
	return &threadLocks{locks: make(map[string]chan struct{})}
}

// Lock blocks until threadID's lock is free or ctx is cancelled. On
// success it returns a func that releases the lock; the caller must call
// it exactly once, typically via defer.
func (l *threadLocks) Lock(ctx context.Context, threadID string) (func(), error) {
	l.mu.Lock()
	ch, ok := l.locks[threadID]
	if !ok {
		ch = make(chan struct{}, 1)
		l.locks[threadID] = ch
	}
	l.mu.Unlock()

	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
