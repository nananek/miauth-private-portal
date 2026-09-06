package domain

import (
	"context"
	"time"
)

// Reaction is one actor's reaction to an entry (Issue #23 PR4). A Misskey
// account has at most one reaction per note (see the reactions table's
// UNIQUE(entry_id, reactor_actor_id) constraint); changing it is
// expressed by deleting the old one and creating a new one, matching how
// Aria's own changeReaction flow calls the wire API (see
// docs/compat/aria-v1.5.11.md's "POST /api/notes/reactions/create"
// section) — ReactionRepository.Create itself is an upsert so either
// call sequence lands on the same single-row-per-pair state.
type Reaction struct {
	ID             string
	EntryID        string
	ReactorActorID string
	// Emoji is always a plain Unicode emoji, never a custom-emoji
	// shortcode: this deployment's Non-goals exclude custom emoji/drive,
	// and the HTTP layer rejects a `:name:`/`:name@host:` shortcode with
	// UNSUPPORTED_FEATURE before it ever reaches this type.
	Emoji     string
	CreatedAt time.Time
}

// ReactionRepository persists reactions on entries.
type ReactionRepository interface {
	// Create inserts reactorActorID's reaction to r.EntryID, or replaces
	// it in place if one already exists (see Reaction's doc comment): a
	// second Create for the same (entry, actor) pair overwrites the
	// emoji/created_at of the first rather than conflicting.
	Create(ctx context.Context, r Reaction) error
	// Delete removes reactorActorID's reaction to entryID. Deleting a
	// pair with no existing reaction is not an error: Aria decodes any
	// 2xx response from this wire call (docs/compat/aria-v1.5.11.md), so
	// there is no observed protocol reason to distinguish "removed" from
	// "was already absent".
	Delete(ctx context.Context, entryID, reactorActorID string) error
	// Get returns one reaction by its opaque ID. It exists only to
	// resolve a paginated ListByEntry call's untilId anchor, the same
	// role EntryRepository.Get plays for notes/timeline's untilId. It
	// returns ErrNotFound if id does not exist.
	Get(ctx context.Context, id string) (Reaction, error)
	// GetByActor returns reactorActorID's own reaction to entryID. It
	// returns ErrNotFound if none exists.
	GetByActor(ctx context.Context, entryID, reactorActorID string) (Reaction, error)
	// CountsByEmoji returns entryID's reaction counts keyed by emoji,
	// backing Note.reactions (docs/compat/aria-v1.5.11.md's Minimum Note
	// contract).
	CountsByEmoji(ctx context.Context, entryID string) (map[string]int, error)
	// CountAll returns the total number of reactions across every entry,
	// backing POST /api/stats' reactionsCount (previously a fixed 0
	// before this repository existed — see newStatsResponse's doc
	// comment).
	CountAll(ctx context.Context) (int, error)
	// ListByEntry returns entryID's reactions, newest-first by
	// (created_at, id), optionally filtered to one emoji when emoji is
	// non-nil. When before is nil it returns the most recent page;
	// otherwise it returns reactions strictly older than before in that
	// same order — the same paging contract as
	// EntryRepository.ListTimelineDesc, applied here for Aria's
	// paginated "who reacted" sheet (POST /api/notes/reactions).
	ListByEntry(ctx context.Context, entryID string, emoji *string, before *Cursor, limit int) ([]Reaction, error)
}
