package userlist

import "time"

// Clock returns the current time. Tests supply a fixed clock so
// created_at/updated_at/added_at timestamps can be asserted without
// depending on wall-clock timing. Mirrors internal/timeline.Clock: each
// narrow use-case package defines its own copy rather than sharing one
// through internal/domain (AGENTS.md's layer-boundary rule applies to
// use-case packages themselves, not just to domain/storage).
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
