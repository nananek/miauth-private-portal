package domain

import (
	"context"
	"time"
)

// ExternalSource is one configured RSS/Atom feed or IMAP mailbox this
// service ingests from. Kind is an open string ("rss", "imap", ...): the
// set of supported adapters lives in Go, not in a schema-level enum.
type ExternalSource struct {
	ID          string
	Kind        string
	URI         string
	DisplayName *string
	// Cursor is an adapter-opaque resume token (for internal/ingest/rss,
	// a JSON string carrying the last response's ETag/Last-Modified). It
	// only ever advances after every item in a fetch batch has been
	// durably processed (see RecordFetchSuccess), so a crash mid-batch
	// re-fetches the same batch rather than skipping unprocessed items.
	Cursor              *string
	LastFetchedAt       *time.Time
	LastError           *string
	ConsecutiveFailures int
	CreatedAt           time.Time
}

// ExternalItem is one fetched item from an ExternalSource, deduplicated
// before being promoted into the timeline as an Entry. EntryID is nil
// until promoted.
type ExternalItem struct {
	ID            string
	SourceID      string
	ExternalID    string // the source's own item ID (guid, Message-ID, ...)
	ProvenanceURL *string
	PublishedAt   *time.Time
	FetchedAt     time.Time
	// DedupeKey is a content-hash fallback for sources whose ExternalID
	// is unstable or absent.
	DedupeKey string
	EntryID   *string
	CreatedAt time.Time
}

// ExternalSourceRepository persists configured external sources.
type ExternalSourceRepository interface {
	Create(ctx context.Context, s ExternalSource) error
	Get(ctx context.Context, id string) (ExternalSource, error)
	List(ctx context.Context) ([]ExternalSource, error)
	// RecordFetchSuccess updates last_fetched_at, clears last_error, and
	// resets consecutive_failures to 0. When cursor is non-nil it also
	// advances the stored fetch cursor; a nil cursor leaves the existing
	// one unchanged (an unmodified-since-last-fetch outcome has no new
	// cursor to record but is still a success). Callers must not call
	// this until every item in the corresponding fetch batch has been
	// durably processed, so cursor never advances past unprocessed items.
	RecordFetchSuccess(ctx context.Context, id string, cursor *string, at time.Time) error
	// RecordFetchFailure records a fetch attempt's failure (last_error,
	// incremented consecutive_failures) for operator observability,
	// without touching cursor, so a failed batch is retried from the
	// same position. It is independent of internal/jobs' own
	// retry/dead-job bookkeeping, which this never influences.
	RecordFetchFailure(ctx context.Context, id string, errMsg string, at time.Time) error
	// EnsureFromConfig idempotently creates every source in sources that
	// does not already exist by (kind, uri): a UNIQUE(kind, uri)
	// conflict on an individual source is ignored, so operator-edited
	// configuration never needs a separate existence check before
	// startup seeding. It never modifies an already-existing source row.
	EnsureFromConfig(ctx context.Context, sources []ExternalSource) error
}

// ExternalItemRepository persists fetched external items and their
// promotion into the timeline.
type ExternalItemRepository interface {
	// Create inserts a new item. It returns ErrConflict if the
	// (source, external ID) pair or the dedupe key already exists.
	Create(ctx context.Context, i ExternalItem) error
	GetByDedupeKey(ctx context.Context, dedupeKey string) (ExternalItem, error)
	// Promote links item to its produced timeline Entry. Call together
	// with EntryRepository.Create for entryID inside the same UnitOfWork
	// transaction.
	Promote(ctx context.Context, id, entryID string) error
}
