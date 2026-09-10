package webadmin

import "time"

// Clock exists so tests can inject a fixed time source instead of
// depending on wall-clock time for TTL/expiry behavior — the exact
// internal/miauth.Clock pattern, duplicated rather than imported: this
// is a one-line interface with no shared state, and ADR-0001's "keep
// distinct records for distinct credentials" extends naturally to
// "keep the new credential type's own package self-contained" (the
// precedent internal/httpserver/noteapi_wire.go's optionalDisplayName
// already set for a similarly tiny, self-contained duplication rather
// than a cross-package export).
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
