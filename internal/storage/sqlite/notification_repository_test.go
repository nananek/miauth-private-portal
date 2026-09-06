package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// seedNotificationTestEntry creates a minimal owner-authored root entry
// for notification tests to attach a notification to; these tests care
// only about related_entry_id existing (the FK target), not the entry's
// own content.
func seedNotificationTestEntry(t *testing.T, db *DB) (entryID string) {
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
	return id
}

func newNotificationTestDB(t *testing.T) *DB {
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

func TestNotificationRepository_Create_Get(t *testing.T) {
	db := newNotificationTestDB(t)
	entryID := seedNotificationTestEntry(t, db)
	now := time.Now()

	if err := db.Notifications.Create(t.Context(), domain.Notification{
		ID: "n1", Type: domain.NotificationReply, RelatedEntryID: entryID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Notifications.Get(t.Context(), "n1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type != domain.NotificationReply {
		t.Errorf("Type = %q, want %q", got.Type, domain.NotificationReply)
	}
	if got.RelatedEntryID != entryID {
		t.Errorf("RelatedEntryID = %q, want %q", got.RelatedEntryID, entryID)
	}
}

func TestNotificationRepository_Get_NotFound(t *testing.T) {
	db := newNotificationTestDB(t)

	_, err := db.Notifications.Get(t.Context(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestNotificationRepository_ListDesc_NewestFirstAndPaginated(t *testing.T) {
	db := newNotificationTestDB(t)
	entryID := seedNotificationTestEntry(t, db)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var ids []string
	for i := 0; i < 3; i++ {
		id := domain.NewID()
		notifType := domain.NotificationReply
		if i == 1 {
			notifType = domain.NotificationApp
		}
		if err := db.Notifications.Create(t.Context(), domain.Notification{
			ID: id, Type: notifType, RelatedEntryID: entryID, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("create notification %d: %v", i, err)
		}
		ids = append(ids, id) // ids[0] oldest ... ids[2] newest
	}

	t.Run("unfiltered newest first", func(t *testing.T) {
		got, err := db.Notifications.ListDesc(t.Context(), nil, 10)
		if err != nil {
			t.Fatalf("ListDesc: %v", err)
		}
		assertNotificationIDOrder(t, got, []string{ids[2], ids[1], ids[0]})
		if got[1].Type != domain.NotificationApp {
			t.Errorf("got[1].Type = %q, want %q", got[1].Type, domain.NotificationApp)
		}
	})

	t.Run("paginated with before cursor", func(t *testing.T) {
		newest, err := db.Notifications.Get(t.Context(), ids[2])
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.Notifications.ListDesc(t.Context(), &domain.Cursor{CreatedAt: newest.CreatedAt, ID: newest.ID}, 10)
		if err != nil {
			t.Fatalf("ListDesc: %v", err)
		}
		assertNotificationIDOrder(t, got, []string{ids[1], ids[0]})
	})

	t.Run("limit bounds the page", func(t *testing.T) {
		got, err := db.Notifications.ListDesc(t.Context(), nil, 1)
		if err != nil {
			t.Fatalf("ListDesc: %v", err)
		}
		assertNotificationIDOrder(t, got, []string{ids[2]})
	})
}

func assertNotificationIDOrder(t *testing.T, got []domain.Notification, wantIDs []string) {
	t.Helper()
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d notifications, want %d", len(got), len(wantIDs))
	}
	for i, n := range got {
		if n.ID != wantIDs[i] {
			t.Errorf("notification[%d].ID = %q, want %q", i, n.ID, wantIDs[i])
		}
	}
}
