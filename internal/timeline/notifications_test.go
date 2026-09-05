package timeline

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestCreateRoot_DoesNotRecordNotification(t *testing.T) {
	ts := newTestService(t)

	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "hello", nil); err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	got, err := ts.ListNotifications(t.Context(), nil, 10)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListNotifications = %v, want empty (a user_post creation must never notify)", got)
	}
}

func TestCreateReply_DoesNotRecordNotification(t *testing.T) {
	ts := newTestService(t)

	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	if _, err := ts.CreateReply(t.Context(), root.ID, domain.EntryUserPost, "reply", nil); err != nil {
		t.Fatalf("CreateReply: %v", err)
	}

	got, err := ts.ListNotifications(t.Context(), nil, 10)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListNotifications = %v, want empty (a user_post reply must never notify)", got)
	}
}

// TestCreateGeneratedReply_RecordsReplyNotification pins plan-issue-23's
// PR6 mapping: an llm_reply/llm_follow_up entry's creation records a
// "reply"-typed notification pointing back at the newly created entry
// itself, atomically with the entry and the generation completion.
func TestCreateGeneratedReply_RecordsReplyNotification(t *testing.T) {
	for _, kind := range []domain.EntryKind{domain.EntryLLMReply, domain.EntryLLMFollowUp} {
		t.Run(string(kind), func(t *testing.T) {
			ts := newTestService(t)
			root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
			if err != nil {
				t.Fatalf("CreateRoot: %v", err)
			}
			gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
			if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
				t.Fatalf("create generation: %v", err)
			}

			reply, err := ts.CreateGeneratedReply(t.Context(), root.ID, kind, "generated body", gen.ID, nil, nil)
			if err != nil {
				t.Fatalf("CreateGeneratedReply: %v", err)
			}

			got, err := ts.ListNotifications(t.Context(), nil, 10)
			if err != nil {
				t.Fatalf("ListNotifications: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ListNotifications = %v, want exactly one notification", got)
			}
			if got[0].Type != domain.NotificationReply {
				t.Errorf("Type = %q, want %q", got[0].Type, domain.NotificationReply)
			}
			if got[0].RelatedEntryID != reply.ID {
				t.Errorf("RelatedEntryID = %q, want the generated reply %q", got[0].RelatedEntryID, reply.ID)
			}
		})
	}
}

// TestCreateExternalEntry_RecordsAppNotificationOnlyWhenNew pins that a
// notification is created only for a genuinely new ingested item, never
// for a redelivered duplicate — mirroring CreateExternalEntry's own
// created=false dedupe branch, which this notification hook must not run
// inside.
func TestCreateExternalEntry_RecordsAppNotificationOnlyWhenNew(t *testing.T) {
	ts := newTestService(t)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", DedupeKey: "dedupe-1"}

	entry, created, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "breaking news")
	if err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true for a brand new item")
	}

	got, err := ts.ListNotifications(t.Context(), nil, 10)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(got) != 1 || got[0].Type != domain.NotificationApp || got[0].RelatedEntryID != entry.ID {
		t.Fatalf("ListNotifications = %v, want exactly one app notification for %q", got, entry.ID)
	}

	// Re-delivering the same item (created=false) must not add a second
	// notification.
	if _, secondCreated, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "breaking news"); err != nil {
		t.Fatalf("second CreateExternalEntry: %v", err)
	} else if secondCreated {
		t.Fatal("second delivery: created = true, want false")
	}

	got, err = ts.ListNotifications(t.Context(), nil, 10)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("ListNotifications after duplicate delivery = %v, want still exactly one", got)
	}
}

func TestListNotifications_NewestFirstAndPaginated(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	var ids []string
	for i := 0; i < 3; i++ {
		gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
		if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
			t.Fatalf("create generation: %v", err)
		}
		reply, err := ts.CreateGeneratedReply(t.Context(), root.ID, domain.EntryLLMReply, "reply", gen.ID, nil, nil)
		if err != nil {
			t.Fatalf("CreateGeneratedReply: %v", err)
		}
		ids = append(ids, reply.ID) // ids[0] oldest ... ids[2] newest
		ts.clock.Advance(time.Minute)
	}

	first, err := ts.ListNotifications(t.Context(), nil, 2)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(first) != 2 || first[0].RelatedEntryID != ids[2] || first[1].RelatedEntryID != ids[1] {
		t.Fatalf("first page related entries = %v, want newest-first [%s, %s]", relatedEntryIDsForTest(first), ids[2], ids[1])
	}

	second, err := ts.ListNotifications(t.Context(), &domain.Cursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID}, 2)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(second) != 1 || second[0].RelatedEntryID != ids[0] {
		t.Fatalf("second page related entries = %v, want [%s]", relatedEntryIDsForTest(second), ids[0])
	}
}

func TestGetNotification_ReturnsErrNotFoundForUnknownID(t *testing.T) {
	ts := newTestService(t)
	if _, err := ts.GetNotification(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetNotification(unknown) error = %v, want domain.ErrNotFound", err)
	}
}

func relatedEntryIDsForTest(notifications []domain.Notification) []string {
	ids := make([]string, len(notifications))
	for i, n := range notifications {
		ids[i] = n.RelatedEntryID
	}
	return ids
}
