// Package health backs the liveness/readiness split required by Issue #3:
// liveness reports only that the process is running, while readiness
// tracks startup completion plus any registered dependency Checkers.
package health

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// checkTimeout bounds each individual Checker.Check call within Ready, so
// one slow or hung dependency cannot block the whole /readyz response
// indefinitely.
const checkTimeout = 2 * time.Second

// Checker reports whether one dependency (a database connection, an
// external adapter, ...) is currently able to serve traffic.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// Registry tracks process startup completion and any registered
// dependency Checkers. A future issue (such as #4's SQLite persistence)
// extends readiness by registering a Checker here; this package does not
// need to change.
type Registry struct {
	mu       sync.RWMutex
	checkers []Checker
	ready    atomic.Bool
}

// NewRegistry returns a Registry that is not ready until MarkReady is
// called.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a dependency Checker consulted by every future Ready call.
// It is not safe to call concurrently with Ready; register every Checker
// during startup, before serving traffic.
func (r *Registry) Register(c Checker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkers = append(r.checkers, c)
}

// MarkReady signals that process startup has finished. It does not itself
// run any Checker; Ready still evaluates them on every call.
func (r *Registry) MarkReady() {
	r.ready.Store(true)
}

// MarkNotReady signals that the process should stop receiving new traffic,
// typically at the start of a graceful shutdown so a load balancer can
// stop routing before in-flight requests finish draining.
func (r *Registry) MarkNotReady() {
	r.ready.Store(false)
}

// Live reports whether the process itself is running. It is intentionally
// unconditional on startup or dependency state: an orchestrator uses
// liveness to decide whether to restart the process, not whether to route
// traffic to it, so it must keep succeeding while the process drains.
func (r *Registry) Live(_ context.Context) error {
	return nil
}

// Ready reports whether startup has finished and every registered Checker
// currently succeeds.
func (r *Registry) Ready(ctx context.Context) error {
	if !r.ready.Load() {
		return errors.New("startup not complete")
	}

	r.mu.RLock()
	checkers := make([]Checker, len(r.checkers))
	copy(checkers, r.checkers)
	r.mu.RUnlock()

	for _, c := range checkers {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := c.Check(checkCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name(), err)
		}
	}
	return nil
}
