package domain

import "errors"

// ErrNotFound is returned by a repository lookup that finds no matching
// record. Callers compare with errors.Is; no storage-specific error type
// ever crosses the repository interface boundary.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a write violates a uniqueness or
// compare-and-set invariant the storage layer enforces (a duplicate
// idempotency key, a second owner-binding attempt, a reused token hash,
// ...).
var ErrConflict = errors.New("conflict")
