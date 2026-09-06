package domain

import (
	"context"
	"time"
)

// Mention records that entryID's Body contained an @-mention of
// MentionedActorID's username at the moment the entry was created
// (Issue #23 PR5). single-owner means MentionedActorID is always the
// owner actor — the only login-capable local actor Aria's "Mention"/
// "Direct" home-timeline tabs could ever plausibly surface (see
// docs/compat/aria-v1.5.11.md's "POST /api/notes/mentions" section) —
// and detection only ever runs on EntryUserPost bodies at creation time
// (see timeline.Service.recordSelfMentionIfAny). It is never recomputed
// or backfilled for entries that already existed before this feature
// shipped (owner-confirmed scope, 2026-09-06): only new posts going
// forward are ever detected.
type Mention struct {
	ID               string
	EntryID          string
	MentionedActorID string
	CreatedAt        time.Time
}

// MentionRepository persists detected self-mentions and answers
// POST /api/notes/mentions' "notes that mention me" query.
type MentionRepository interface {
	// Create inserts m. Detection runs at most once per entry, at
	// creation time (see Mention's doc comment), so this is a plain
	// insert — never an upsert the way ReactionRepository.Create is.
	Create(ctx context.Context, m Mention) error
	// ListEntriesByMentionedActor returns, newest-first by
	// (created_at, id), the archived/hidden-excluded entries that
	// mention actorID — joining through the mentions table so callers
	// get the full Entry directly, the same shape
	// EntryRepository.ListTimelineDesc returns (POST /api/notes/mentions
	// projects each one exactly like notes/timeline does). When before
	// is nil it returns the most recent page; otherwise entries strictly
	// older than before in that same order.
	ListEntriesByMentionedActor(ctx context.Context, actorID string, before *Cursor, limit int) ([]Entry, error)
}
