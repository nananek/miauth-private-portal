// Package drive implements Issue #77's Drive-backed media storage
// foundation (ADR-0007): a Storage abstraction over the object bytes
// backing avatars, RSS/mail source favicons, app icons, and post
// attachments, plus the raster-image validation every upload must pass
// before it is stored.
//
// This package knows nothing about HTTP, the Misskey wire format, or the
// files table's metadata — it is deliberately narrow, matching
// AGENTS.md's "Put persistence and provider boundaries behind narrow
// interfaces." A deployment selects exactly one Storage implementation
// for its whole lifetime (internal/config's DriveConfig); nothing here
// switches backend per file or per request, and there is no migration
// path between backends.
package drive

import (
	"context"
	"errors"
	"io"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// Storage puts, gets, and deletes opaque byte blobs by key. Both
// implementations in this package (Local, S3) satisfy it identically
// from a caller's point of view: neither leaks backend-specific errors
// or behavior beyond what this interface documents.
//
// key is caller-assigned and opaque to Storage — it corresponds to the
// files table's storage_key column, but this package does not know
// about that table. Callers must choose keys that cannot collide across
// unrelated files (for example a UUID-derived path); Storage does not
// deduplicate or namespace keys itself.
type Storage interface {
	// Put stores size bytes read from r under key, failing if key
	// already has an object (Put is create-only: overwriting an
	// existing key is a caller bug, since keys are meant to be assigned
	// once per uploaded file and never reused). r is read to completion
	// or to the first error; Put does not retry a failed read.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens key for reading. The caller must Close the returned
	// ReadCloser. Returns an error satisfying errors.Is(err,
	// domain.ErrNotFound) if key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes key. Deleting a key that does not exist is not an
	// error: callers (orphan GC, a failed upload's cleanup) must be able
	// to delete idempotently without first checking existence.
	Delete(ctx context.Context, key string) error
	// List returns every key this backend currently holds, in no
	// particular order. Issue #77 PR7's orphan GC (Service.RunOrphanGC)
	// is its only caller: it cross-references this against every
	// files.storage_key in the database to find an object no row
	// references — the storeValidatedImage/DeleteFile failure paths
	// documented in their own comments are what can leave one behind.
	List(ctx context.Context) ([]string, error)
}

// ErrKeyExists reports that Put was called with a key that already has
// an object. Wraps no domain.Err* sentinel because it is a caller
// programming error (key reuse), not an expected runtime condition a
// caller branches on.
var ErrKeyExists = errors.New("drive: key already exists")

// notFound wraps domain.ErrNotFound so every Storage implementation
// reports a missing key identically, regardless of how its own backend
// (a filesystem ENOENT, an S3 404) spells "not found."
func notFound(key string) error {
	return &keyNotFoundError{key: key}
}

type keyNotFoundError struct{ key string }

func (e *keyNotFoundError) Error() string { return "drive: key " + e.key + " not found" }
func (e *keyNotFoundError) Unwrap() error { return domain.ErrNotFound }
