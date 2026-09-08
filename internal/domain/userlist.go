package domain

import (
	"context"
	"time"
)

// UserList is a named, owner-curated grouping of local actors (Issue
// #115): the entity behind Misskey-compatible users/lists/* CRUD and the
// notes/user-list-timeline filtered timeline built from its membership.
// This service is single-owner (AGENTS.md), so a list's owner is always
// the owner actor — unlike real Misskey there is no owner field here,
// since every row would carry the same value.
type UserList struct {
	ID   string
	Name string
	// IsPublic is persisted so users/lists/update's isPublic field
	// round-trips, but has no other effect: real Misskey's public list
	// page is a federation-facing feature this non-federating deployment
	// does not implement (Issue #115 Non-goals).
	IsPublic  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UserListRepository persists UserList rows and their actor membership.
// Unlike EntryRepository's Entries+Threads pairing, no method here
// composes with another repository inside a UnitOfWork transaction: each
// call commits its own single change, the same granularity
// ReactionRepository's Create/Delete already use for a similar
// small-mutation join.
type UserListRepository interface {
	Create(ctx context.Context, l UserList) error
	Get(ctx context.Context, id string) (UserList, error)
	// ListAll returns every list (single-owner: always the owner's own
	// lists) in a stable (created_at, id) order. Misskey's own
	// users/lists/list returns every list in one response, so there is
	// no pagination window to plumb here.
	ListAll(ctx context.Context) ([]UserList, error)
	// Update applies a partial update: a nil name or isPublic leaves that
	// field unchanged. It returns the row as it reads back after the
	// update, and ErrNotFound if id does not exist.
	Update(ctx context.Context, id string, name *string, isPublic *bool, updatedAt time.Time) (UserList, error)
	// Delete removes l and every user_list_members row naming it
	// (explicitly, not via an ON DELETE CASCADE — see migration
	// 0033_user_lists.sql). Deleting an unknown id returns ErrNotFound.
	Delete(ctx context.Context, id string) error

	// AddMember adds actorID to listID's membership, idempotently: a
	// second call for the same pair is a silent no-op rather than
	// ErrConflict, mirroring ReactionRepository.Create's own "second call
	// overwrites rather than conflicts" precedent — there is no
	// meaningful difference between two AddMember calls for the same
	// pair worth distinguishing. It does not itself validate listID or
	// actorID; callers (internal/userlist.Service) are responsible for
	// checking listID exists first, and internal/httpserver checks
	// actorID against the known actor set before ever calling this (see
	// plan-115 §2.2).
	AddMember(ctx context.Context, listID, actorID string, addedAt time.Time) error
	// RemoveMember removes actorID from listID's membership, if present.
	// Idempotent: removing a non-member is not an error, mirroring
	// ReactionRepository.Delete's own doc comment.
	RemoveMember(ctx context.Context, listID, actorID string) error
	// MemberActorIDs returns listID's member actor IDs in the order they
	// were added (oldest first). A list with no members returns an empty
	// result, not an error.
	MemberActorIDs(ctx context.Context, listID string) ([]string, error)
}
