package domain

import (
	"context"
	"time"
)

// EntryKind distinguishes how an entry's Body originated.
type EntryKind string

const (
	EntryUserPost    EntryKind = "user_post"
	EntryLLMReply    EntryKind = "llm_reply"
	EntryLLMFollowUp EntryKind = "llm_follow_up"
	EntryNews        EntryKind = "news"
	EntryMail        EntryKind = "mail"
	EntrySystem      EntryKind = "system"
)

// ProcessingStatus tracks an entry's own asynchronous pipeline (LLM
// classification/reply generation), independent of the user-controlled
// archive/hide visibility on the same entry.
type ProcessingStatus string

const (
	ProcessingNone       ProcessingStatus = "none"
	ProcessingPending    ProcessingStatus = "pending"
	ProcessingInProgress ProcessingStatus = "processing"
	ProcessingComplete   ProcessingStatus = "complete"
	ProcessingFailed     ProcessingStatus = "failed"
)

// Thread groups a root Entry and every Entry that replies to it, directly
// or transitively. A Thread's ID always equals its root Entry's ID (see
// EntryRepository), so no circular foreign key is needed between the two
// tables.
type Thread struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Entry is one timeline item: a user post, an LLM-generated reply or
// follow-up question, or an ingested news/mail item. Body is
// user-authored or ingestion-provenance source text. LLM
// classification/summary/tag/related-post data is stored separately (see
// LLMClassification) and must never overwrite Body.
type Entry struct {
	ID       string
	ThreadID string
	// ParentEntryID is nil only for a thread's root entry (where ID ==
	// ThreadID); otherwise it names the entry this one directly replies
	// to.
	ParentEntryID    *string
	Kind             EntryKind
	AuthorActorID    string
	Body             string
	ProcessingStatus ProcessingStatus
	ArchivedAt       *time.Time
	HiddenAt         *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// IsRoot reports whether e is its thread's root entry.
func (e Entry) IsRoot() bool {
	return e.ParentEntryID == nil
}

// Cursor is a pagination position: the (created_at, id) tie-breaker a
// stable timeline page requires, since IDs are opaque and same-timestamp
// entries are otherwise unorderable. Never use offset-only pagination or
// lexical ID order against this cursor.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Page bounds one paginated timeline read.
type Page struct {
	// After, when set, returns entries strictly after this cursor in
	// (created_at, id) order.
	After *Cursor
	Limit int
}

// ThreadRepository persists threads.
type ThreadRepository interface {
	Create(ctx context.Context, t Thread) error
	Get(ctx context.Context, id string) (Thread, error)
	// Touch bumps a thread's updated_at, typically called whenever a new
	// reply attaches to it.
	Touch(ctx context.Context, id string, at time.Time) error
}

// EntryRepository persists timeline entries.
type EntryRepository interface {
	// Create inserts e. Creating a thread's root entry (e.IsRoot()) and
	// creating the owning Thread row are two separate calls; call
	// ThreadRepository.Create first (same e.ID as e.ThreadID) within the
	// same UnitOfWork transaction so both commit atomically.
	Create(ctx context.Context, e Entry) error
	Get(ctx context.Context, id string) (Entry, error)
	// ListByThread returns every entry in threadID, ordered oldest-first.
	ListByThread(ctx context.Context, threadID string) ([]Entry, error)
	// ListChildren returns only the entries that reply directly to
	// parentEntryID, ordered oldest-first by (created_at, id). It does not
	// include deeper descendants.
	ListChildren(ctx context.Context, parentEntryID string) ([]Entry, error)
	// ListTimeline returns entries ordered oldest-first by (created_at,
	// id). When includeHidden is false, archived and hidden entries are
	// excluded.
	ListTimeline(ctx context.Context, page Page, includeHidden bool) ([]Entry, error)
	// UpdateBody replaces an entry's body and bumps updated_at. It is a
	// persistence primitive only: use-case callers are responsible for
	// restricting edits to the author's own user_post entries.
	UpdateBody(ctx context.Context, id, body string, at time.Time) error
	SetProcessingStatus(ctx context.Context, id string, status ProcessingStatus, at time.Time) error
	SetArchived(ctx context.Context, id string, archived bool, at time.Time) error
	SetHidden(ctx context.Context, id string, hidden bool, at time.Time) error
}
