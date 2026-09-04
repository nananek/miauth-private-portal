package domain

import (
	"context"
	"time"
)

// ClassificationStatus tracks one classification attempt.
type ClassificationStatus string

const (
	ClassificationPending  ClassificationStatus = "pending"
	ClassificationComplete ClassificationStatus = "complete"
	ClassificationFailed   ClassificationStatus = "failed"
)

// LLMClassification is one versioned, LLM-authored organization result for
// an Entry (subject, field, keywords, open questions, priority, ...),
// stored separately from the entry's user-authored Body and never
// overwriting it. StructuredOutput carries the fields beyond Summary as
// opaque JSON; this package's callers, not SQLite, validate its shape.
type LLMClassification struct {
	// ID is an internal surrogate key; it is never exposed on the wire.
	ID               int64
	EntryID          string
	Version          int
	IsActive         bool
	Provider         string
	Model            string
	PromptVersion    string
	Status           ClassificationStatus
	ErrorCategory    *string
	Summary          *string
	StructuredOutput *string
	PromptTokens     *int
	CompletionTokens *int
	GeneratedAt      *time.Time
	CreatedAt        time.Time
	Tags             []string
	RelatedEntryIDs  []string
}

// LLMClassificationRepository persists versioned LLM classification
// results, kept separate from EntryRepository (user-authored data) and
// from UserTagRepository (user-authored tags).
type LLMClassificationRepository interface {
	// Create inserts the next version for c.EntryID and returns its
	// generated internal ID (needed by AddTag/AddRelatedEntry). It does
	// not change any other version's IsActive flag; call Activate in the
	// same UnitOfWork transaction to make the new version current
	// atomically (the schema's partial unique index allows only one
	// active version per entry at a time).
	Create(ctx context.Context, c LLMClassification) (int64, error)
	// Activate marks entryID's classification at version as the active
	// one and deactivates every other version for that entry. Callers
	// that need this to happen atomically must call it inside
	// UnitOfWork.WithinTx; called standalone, a failure partway through
	// can leave entryID with no active version at all. That is still
	// safe - GetActive then reports ErrNotFound rather than returning a
	// stale or inconsistent result - but it is not atomic.
	Activate(ctx context.Context, entryID string, version int) error
	GetActive(ctx context.Context, entryID string) (LLMClassification, error)
	ListVersions(ctx context.Context, entryID string) ([]LLMClassification, error)
	AddTag(ctx context.Context, classificationID int64, tag string) error
	AddRelatedEntry(ctx context.Context, classificationID int64, relatedEntryID string) error
}

// UserTagRepository persists user-authored entry tags, kept separate from
// LLMClassificationRepository's LLM-authored tags.
type UserTagRepository interface {
	Add(ctx context.Context, entryID, tag string, at time.Time) error
	Remove(ctx context.Context, entryID, tag string) error
	ListByEntry(ctx context.Context, entryID string) ([]string, error)
}
