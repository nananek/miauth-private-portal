package sqlite

import (
	"time"

	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// seedMentionTestEntry creates a minimal owner-authored root user_post
// entry for mention tests to attach mentions to; mention tests care only
// about entry_id/mentioned_actor_id, not the entry's own content.
func seedMentionTestEntry(t *testing.T, db *DB, createdAt time.Time) (entryID, ownerActorID string) {
	t.Helper()
	owner, err := db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatalf("get owner actor: %v", err)
	}
	id := domain.NewID()
	if err := db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: createdAt, UpdatedAt: createdAt}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	entry := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryUserPost, AuthorActorID: owner.ID,
		Body: "@owner body", ProcessingStatus: domain.ProcessingNone, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	return id, owner.ID
}

func newMentionTestDB(t *testing.T) *DB {
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

func TestMentionRepository_Create_ListEntriesByMentionedActor(t *testing.T) {
	db := newMentionTestDB(t)
	entryID, ownerID := seedMentionTestEntry(t, db, time.Now())

	if err := db.Mentions.Create(t.Context(), domain.Mention{
		ID: domain.NewID(), EntryID: entryID, MentionedActorID: ownerID, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Mentions.ListEntriesByMentionedActor(t.Context(), ownerID, nil, 10)
	if err != nil {
		t.Fatalf("ListEntriesByMentionedActor: %v", err)
	}
	if len(got) != 1 || got[0].ID != entryID {
		t.Fatalf("got = %v, want exactly [%s]", entryIDs(got), entryID)
	}
}

func TestMentionRepository_ListEntriesByMentionedActor_EmptyWhenNoMentions(t *testing.T) {
	db := newMentionTestDB(t)
	owner, err := db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatalf("get owner actor: %v", err)
	}
	seedMentionTestEntry(t, db, time.Now()) // an entry exists, but no mention row references it

	got, err := db.Mentions.ListEntriesByMentionedActor(t.Context(), owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListEntriesByMentionedActor: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty", entryIDs(got))
	}
}

func TestMentionRepository_ListEntriesByMentionedActor_ExcludesArchivedAndHidden(t *testing.T) {
	db := newMentionTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	visibleID, ownerID := seedMentionTestEntry(t, db, base)
	if err := db.Mentions.Create(t.Context(), domain.Mention{
		ID: domain.NewID(), EntryID: visibleID, MentionedActorID: ownerID, CreatedAt: base,
	}); err != nil {
		t.Fatalf("create mention for visible entry: %v", err)
	}

	hiddenID, _ := seedMentionTestEntry(t, db, base.Add(time.Minute))
	if err := db.Mentions.Create(t.Context(), domain.Mention{
		ID: domain.NewID(), EntryID: hiddenID, MentionedActorID: ownerID, CreatedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("create mention for hidden entry: %v", err)
	}
	if err := db.Entries.SetHidden(t.Context(), hiddenID, true, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("hide entry: %v", err)
	}

	archivedID, _ := seedMentionTestEntry(t, db, base.Add(2*time.Minute))
	if err := db.Mentions.Create(t.Context(), domain.Mention{
		ID: domain.NewID(), EntryID: archivedID, MentionedActorID: ownerID, CreatedAt: base.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("create mention for archived entry: %v", err)
	}
	if err := db.Entries.SetArchived(t.Context(), archivedID, true, base.Add(3*time.Minute)); err != nil {
		t.Fatalf("archive entry: %v", err)
	}

	got, err := db.Mentions.ListEntriesByMentionedActor(t.Context(), ownerID, nil, 10)
	if err != nil {
		t.Fatalf("ListEntriesByMentionedActor: %v", err)
	}
	if len(got) != 1 || got[0].ID != visibleID {
		t.Fatalf("got = %v, want exactly [%s] (archived/hidden excluded)", entryIDs(got), visibleID)
	}
}

func TestMentionRepository_ListEntriesByMentionedActor_NewestFirstAndPaginated(t *testing.T) {
	db := newMentionTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var ids []string
	var ownerID string
	for i := 0; i < 3; i++ {
		id, owner := seedMentionTestEntry(t, db, base.Add(time.Duration(i)*time.Minute))
		ownerID = owner
		if err := db.Mentions.Create(t.Context(), domain.Mention{
			ID: domain.NewID(), EntryID: id, MentionedActorID: owner, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("create mention %d: %v", i, err)
		}
		ids = append(ids, id) // ids[0] oldest ... ids[2] newest
	}

	t.Run("unfiltered newest first", func(t *testing.T) {
		got, err := db.Mentions.ListEntriesByMentionedActor(t.Context(), ownerID, nil, 10)
		if err != nil {
			t.Fatalf("ListEntriesByMentionedActor: %v", err)
		}
		assertEntryIDOrder(t, got, []string{ids[2], ids[1], ids[0]})
	})

	t.Run("paginated with before cursor", func(t *testing.T) {
		newest, err := db.Entries.Get(t.Context(), ids[2])
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.Mentions.ListEntriesByMentionedActor(t.Context(), ownerID,
			&domain.Cursor{CreatedAt: newest.CreatedAt, ID: newest.ID}, 10)
		if err != nil {
			t.Fatalf("ListEntriesByMentionedActor: %v", err)
		}
		assertEntryIDOrder(t, got, []string{ids[1], ids[0]})
	})

	t.Run("limit bounds the page", func(t *testing.T) {
		got, err := db.Mentions.ListEntriesByMentionedActor(t.Context(), ownerID, nil, 1)
		if err != nil {
			t.Fatalf("ListEntriesByMentionedActor: %v", err)
		}
		assertEntryIDOrder(t, got, []string{ids[2]})
	})
}

func assertEntryIDOrder(t *testing.T, got []domain.Entry, wantIDs []string) {
	t.Helper()
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d entries, want %d (%v)", len(got), len(wantIDs), entryIDs(got))
	}
	for i, e := range got {
		if e.ID != wantIDs[i] {
			t.Errorf("entry[%d].ID = %q, want %q", i, e.ID, wantIDs[i])
		}
	}
}
