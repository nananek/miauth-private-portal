package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// seedReactionTestEntry creates a minimal owner-authored root entry for
// reaction tests to attach reactions to; reaction tests care only about
// entry_id/reactor_actor_id, not the entry's own content.
func seedReactionTestEntry(t *testing.T, db *DB) (entryID, actorID string) {
	t.Helper()
	owner, err := db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatalf("get owner actor: %v", err)
	}
	now := time.Now()
	id := domain.NewID()
	if err := db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	entry := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryUserPost, AuthorActorID: owner.ID,
		Body: "body", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	return id, owner.ID
}

func newReactionTestDB(t *testing.T) *DB {
	t.Helper()
	db := newTestDB(t)
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}
	if err := db.Actors.Create(t.Context(), domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create owner actor: %v", err)
	}
	return db
}

func TestReactionRepository_Create_GetByActor(t *testing.T) {
	db := newReactionTestDB(t)
	entryID, actorID := seedReactionTestEntry(t, db)
	now := time.Now()

	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: domain.NewID(), EntryID: entryID, ReactorActorID: actorID, Emoji: "👍", CreatedAt: now,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Reactions.GetByActor(t.Context(), entryID, actorID)
	if err != nil {
		t.Fatalf("GetByActor: %v", err)
	}
	if got.Emoji != "👍" {
		t.Errorf("Emoji = %q, want %q", got.Emoji, "👍")
	}
}

// TestReactionRepository_Create_OverwritesExistingReaction pins that a
// second Create for the same (entry, actor) pair replaces the emoji
// rather than conflicting on the UNIQUE(entry_id, reactor_actor_id)
// constraint (see domain.Reaction's doc comment).
func TestReactionRepository_Create_OverwritesExistingReaction(t *testing.T) {
	db := newReactionTestDB(t)
	entryID, actorID := seedReactionTestEntry(t, db)
	now := time.Now()

	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: domain.NewID(), EntryID: entryID, ReactorActorID: actorID, Emoji: "👍", CreatedAt: now,
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	secondID := domain.NewID()
	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: secondID, EntryID: entryID, ReactorActorID: actorID, Emoji: "❤️", CreatedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("second Create: %v", err)
	}

	got, err := db.Reactions.GetByActor(t.Context(), entryID, actorID)
	if err != nil {
		t.Fatalf("GetByActor: %v", err)
	}
	if got.Emoji != "❤️" {
		t.Errorf("Emoji after overwrite = %q, want %q", got.Emoji, "❤️")
	}

	counts, err := db.Reactions.CountsByEmoji(t.Context(), entryID)
	if err != nil {
		t.Fatalf("CountsByEmoji: %v", err)
	}
	if len(counts) != 1 || counts["❤️"] != 1 {
		t.Errorf("CountsByEmoji = %v, want exactly one ❤️ reaction (no leftover 👍 row)", counts)
	}
}

func TestReactionRepository_GetByActor_NotFound(t *testing.T) {
	db := newReactionTestDB(t)
	entryID, actorID := seedReactionTestEntry(t, db)

	_, err := db.Reactions.GetByActor(t.Context(), entryID, actorID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByActor() error = %v, want ErrNotFound", err)
	}
}

func TestReactionRepository_Delete_IsIdempotent(t *testing.T) {
	db := newReactionTestDB(t)
	entryID, actorID := seedReactionTestEntry(t, db)

	if err := db.Reactions.Delete(t.Context(), entryID, actorID); err != nil {
		t.Fatalf("Delete on absent reaction: %v", err)
	}

	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: domain.NewID(), EntryID: entryID, ReactorActorID: actorID, Emoji: "👍", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Reactions.Delete(t.Context(), entryID, actorID); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if _, err := db.Reactions.GetByActor(t.Context(), entryID, actorID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByActor() after Delete error = %v, want ErrNotFound", err)
	}
	if err := db.Reactions.Delete(t.Context(), entryID, actorID); err != nil {
		t.Fatalf("second Delete (already absent): %v", err)
	}
}

func TestReactionRepository_CountsByEmoji_GroupsAcrossReactors(t *testing.T) {
	db := newReactionTestDB(t)
	entryID, owner := seedReactionTestEntry(t, db)
	assistant, err := db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatalf("get assistant actor: %v", err)
	}

	now := time.Now()
	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: domain.NewID(), EntryID: entryID, ReactorActorID: owner, Emoji: "👍", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create owner reaction: %v", err)
	}
	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: domain.NewID(), EntryID: entryID, ReactorActorID: assistant.ID, Emoji: "👍", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create assistant reaction: %v", err)
	}

	counts, err := db.Reactions.CountsByEmoji(t.Context(), entryID)
	if err != nil {
		t.Fatalf("CountsByEmoji: %v", err)
	}
	if counts["👍"] != 2 {
		t.Errorf("counts[👍] = %d, want 2", counts["👍"])
	}
}

func TestReactionRepository_CountAll(t *testing.T) {
	db := newReactionTestDB(t)
	entryID, actorID := seedReactionTestEntry(t, db)
	otherEntryID, _ := seedReactionTestEntry(t, db)

	count, err := db.Reactions.CountAll(t.Context())
	if err != nil {
		t.Fatalf("CountAll: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountAll before any reaction = %d, want 0", count)
	}

	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: domain.NewID(), EntryID: entryID, ReactorActorID: actorID, Emoji: "👍", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create reaction 1: %v", err)
	}
	if err := db.Reactions.Create(t.Context(), domain.Reaction{
		ID: domain.NewID(), EntryID: otherEntryID, ReactorActorID: actorID, Emoji: "❤️", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create reaction 2: %v", err)
	}

	count, err = db.Reactions.CountAll(t.Context())
	if err != nil {
		t.Fatalf("CountAll: %v", err)
	}
	if count != 2 {
		t.Errorf("CountAll = %d, want 2", count)
	}
}

func TestReactionRepository_ListByEntry_NewestFirstFilteredAndPaginated(t *testing.T) {
	db := newReactionTestDB(t)
	entryID, owner := seedReactionTestEntry(t, db)
	assistant, err := db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatalf("get assistant actor: %v", err)
	}
	system, err := db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatalf("get system actor: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thumbsUpOwner := domain.Reaction{ID: domain.NewID(), EntryID: entryID, ReactorActorID: owner, Emoji: "👍", CreatedAt: base}
	thumbsUpAssistant := domain.Reaction{ID: domain.NewID(), EntryID: entryID, ReactorActorID: assistant.ID, Emoji: "👍", CreatedAt: base.Add(time.Minute)}
	heartSystem := domain.Reaction{ID: domain.NewID(), EntryID: entryID, ReactorActorID: system.ID, Emoji: "❤️", CreatedAt: base.Add(2 * time.Minute)}
	for _, react := range []domain.Reaction{thumbsUpOwner, thumbsUpAssistant, heartSystem} {
		if err := db.Reactions.Create(t.Context(), react); err != nil {
			t.Fatalf("create reaction %s: %v", react.ID, err)
		}
	}

	t.Run("unfiltered newest first", func(t *testing.T) {
		got, err := db.Reactions.ListByEntry(t.Context(), entryID, nil, nil, 10)
		if err != nil {
			t.Fatalf("ListByEntry: %v", err)
		}
		wantOrder := []string{heartSystem.ID, thumbsUpAssistant.ID, thumbsUpOwner.ID}
		assertReactionIDOrder(t, got, wantOrder)
	})

	t.Run("filtered by emoji", func(t *testing.T) {
		emoji := "👍"
		got, err := db.Reactions.ListByEntry(t.Context(), entryID, &emoji, nil, 10)
		if err != nil {
			t.Fatalf("ListByEntry: %v", err)
		}
		assertReactionIDOrder(t, got, []string{thumbsUpAssistant.ID, thumbsUpOwner.ID})
	})

	t.Run("paginated with before cursor", func(t *testing.T) {
		got, err := db.Reactions.ListByEntry(t.Context(), entryID, nil, &domain.Cursor{CreatedAt: heartSystem.CreatedAt, ID: heartSystem.ID}, 10)
		if err != nil {
			t.Fatalf("ListByEntry: %v", err)
		}
		assertReactionIDOrder(t, got, []string{thumbsUpAssistant.ID, thumbsUpOwner.ID})
	})

	t.Run("limit bounds the page", func(t *testing.T) {
		got, err := db.Reactions.ListByEntry(t.Context(), entryID, nil, nil, 1)
		if err != nil {
			t.Fatalf("ListByEntry: %v", err)
		}
		assertReactionIDOrder(t, got, []string{heartSystem.ID})
	})
}

func assertReactionIDOrder(t *testing.T, got []domain.Reaction, wantIDs []string) {
	t.Helper()
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d reactions, want %d", len(got), len(wantIDs))
	}
	for i, react := range got {
		if react.ID != wantIDs[i] {
			t.Errorf("reaction[%d].ID = %q, want %q", i, react.ID, wantIDs[i])
		}
	}
}
