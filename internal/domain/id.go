// Package domain holds this service's entities and repository interfaces.
// It depends on nothing but the standard library and never imports
// database/sql, an HTTP package, or any specific LLM provider client, so
// storage and transport adapters can change without changing this
// package. internal/storage/sqlite implements the repository interfaces
// declared here; a future internal/storage/postgres could implement the
// same interfaces without this package changing.
package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a new opaque identifier suitable for any entity this
// service persists (entries, threads, actors, sessions, tokens, jobs,
// ...). Callers must treat it as opaque: never parse it, sort it, or
// infer creation order from it.
func NewID() string {
	buf := make([]byte, 16)
	// crypto/rand.Read never returns an error on Go 1.24+: it crashes the
	// process irrecoverably instead of returning one (go.dev/issue/66821),
	// so there is no error path here to check or recover from.
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
