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
	CreatedAt   time.Time
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
