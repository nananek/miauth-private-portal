package timeline

import (
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestSetReaction_CreatesThenOverwritesOnSecondCall(t *testing.T) {
	ts := newTestService(t)
	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "body", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := ts.SetReaction(t.Context(), entry.ID, ts.owner.ID, "👍"); err != nil {
		t.Fatalf("SetReaction: %v", err)
	}
	my, err := ts.MyReaction(t.Context(), entry.ID, ts.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if my == nil || *my != "👍" {
		t.Errorf("MyReaction = %v, want 👍", my)
	}

	ts.clock.Advance(time.Minute)
	if err := ts.SetReaction(t.Context(), entry.ID, ts.owner.ID, "❤️"); err != nil {
		t.Fatalf("SetReaction (overwrite): %v", err)
	}
	my, err = ts.MyReaction(t.Context(), entry.ID, ts.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if my == nil || *my != "❤️" {
		t.Errorf("MyReaction after overwrite = %v, want ❤️", my)
	}

	counts, err := ts.ReactionCounts(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts["❤️"] != 1 {
		t.Errorf("ReactionCounts = %v, want exactly one ❤️ (no leftover 👍)", counts)
	}
}

func TestMyReaction_NilWhenNeverReacted(t *testing.T) {
	ts := newTestService(t)
	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "body", nil)
	if err != nil {
		t.Fatal(err)
	}

	my, err := ts.MyReaction(t.Context(), entry.ID, ts.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if my != nil {
		t.Errorf("MyReaction = %v, want nil", my)
	}
}

func TestRemoveReaction_IsIdempotent(t *testing.T) {
	ts := newTestService(t)
	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "body", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := ts.RemoveReaction(t.Context(), entry.ID, ts.owner.ID); err != nil {
		t.Fatalf("RemoveReaction on absent reaction: %v", err)
	}

	if err := ts.SetReaction(t.Context(), entry.ID, ts.owner.ID, "👍"); err != nil {
		t.Fatal(err)
	}
	if err := ts.RemoveReaction(t.Context(), entry.ID, ts.owner.ID); err != nil {
		t.Fatalf("RemoveReaction: %v", err)
	}
	my, err := ts.MyReaction(t.Context(), entry.ID, ts.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if my != nil {
		t.Errorf("MyReaction after remove = %v, want nil", my)
	}
	if err := ts.RemoveReaction(t.Context(), entry.ID, ts.owner.ID); err != nil {
		t.Fatalf("second RemoveReaction (already absent): %v", err)
	}
}

func TestCountAllReactions_CountsAcrossEntries(t *testing.T) {
	ts := newTestService(t)
	first, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "two", nil)
	if err != nil {
		t.Fatal(err)
	}

	count, err := ts.CountAllReactions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("CountAllReactions before any reaction = %d, want 0", count)
	}

	if err := ts.SetReaction(t.Context(), first.ID, ts.owner.ID, "👍"); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetReaction(t.Context(), second.ID, ts.owner.ID, "❤️"); err != nil {
		t.Fatal(err)
	}

	count, err = ts.CountAllReactions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("CountAllReactions = %d, want 2", count)
	}
}

func TestListReactions_NewestFirstFilteredAndPaginated(t *testing.T) {
	ts := newTestService(t)
	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}

	if err := ts.SetReaction(t.Context(), entry.ID, ts.owner.ID, "👍"); err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	if err := ts.SetReaction(t.Context(), entry.ID, assistant.ID, "❤️"); err != nil {
		t.Fatal(err)
	}

	all, err := ts.ListReactions(t.Context(), entry.ID, nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ReactorActorID != assistant.ID || all[1].ReactorActorID != ts.owner.ID {
		t.Fatalf("ListReactions unfiltered = %+v, want newest-first [assistant, owner]", all)
	}

	emoji := "👍"
	filtered, err := ts.ListReactions(t.Context(), entry.ID, &emoji, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ReactorActorID != ts.owner.ID {
		t.Fatalf("ListReactions filtered by 👍 = %+v, want just the owner's reaction", filtered)
	}

	anchor, err := ts.GetReaction(t.Context(), all[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	before := &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}
	older, err := ts.ListReactions(t.Context(), entry.ID, nil, before, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].ReactorActorID != ts.owner.ID {
		t.Fatalf("ListReactions before newest = %+v, want just the owner's older reaction", older)
	}
}
