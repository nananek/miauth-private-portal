package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// mustCreateThreadAndRoot creates a thread and its root entry (same ID),
// the pattern every EntryRepository.Create caller must follow for a new
// thread (see internal/domain.EntryRepository).
func mustCreateThreadAndRoot(t *testing.T, db *DB, actorID string, at time.Time) domain.Entry {
	t.Helper()
	id := domain.NewID()
	if err := db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	e := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryUserPost, AuthorActorID: actorID,
		Body: "root", ProcessingStatus: domain.ProcessingNone, CreatedAt: at, UpdatedAt: at,
	}
	if err := db.Entries.Create(t.Context(), e); err != nil {
		t.Fatalf("create root entry: %v", err)
	}
	return e
}

func TestEntryRepository_CreateAndGet_Root(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()

	root := mustCreateThreadAndRoot(t, db, actorID, now)

	got, err := db.Entries.Get(t.Context(), root.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.IsRoot() {
		t.Error("root entry should report IsRoot() == true")
	}
	if got.Body != "root" {
		t.Errorf("Body = %q, want %q", got.Body, "root")
	}
}

func TestEntryRepository_CreateReply(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	root := mustCreateThreadAndRoot(t, db, actorID, now)

	replyID := domain.NewID()
	reply := domain.Entry{
		ID: replyID, ThreadID: root.ThreadID, ParentEntryID: &root.ID, Kind: domain.EntryUserPost,
		AuthorActorID: actorID, Body: "reply", ProcessingStatus: domain.ProcessingNone,
		CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute),
	}
	if err := db.Entries.Create(t.Context(), reply); err != nil {
		t.Fatalf("create reply: %v", err)
	}

	got, err := db.Entries.Get(t.Context(), replyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsRoot() {
		t.Error("reply entry should report IsRoot() == false")
	}
	if got.ParentEntryID == nil || *got.ParentEntryID != root.ID {
		t.Errorf("ParentEntryID = %v, want %q", got.ParentEntryID, root.ID)
	}
}

// TestEntryRepository_Create_RejectsUnknownThread backs the "SQLite
// specifics stay in the adapter" acceptance criterion: a foreign-key
// violation against a nonexistent thread must surface as an error, not a
// silently-created orphan row.
func TestEntryRepository_Create_RejectsUnknownThread(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	id := domain.NewID()

	err := db.Entries.Create(t.Context(), domain.Entry{
		ID: id, ThreadID: "does-not-exist", Kind: domain.EntryUserPost, AuthorActorID: actorID,
		Body: "orphan", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("expected an error creating an entry against a nonexistent thread")
	}
}

func TestEntryRepository_ListByThread_OrdersOldestFirst(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	root := mustCreateThreadAndRoot(t, db, actorID, now)

	for i := 1; i <= 3; i++ {
		id := domain.NewID()
		at := now.Add(time.Duration(i) * time.Minute)
		if err := db.Entries.Create(t.Context(), domain.Entry{
			ID: id, ThreadID: root.ThreadID, ParentEntryID: &root.ID, Kind: domain.EntryUserPost,
			AuthorActorID: actorID, Body: "reply", ProcessingStatus: domain.ProcessingNone,
			CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := db.Entries.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("len(entries) = %d, want 4", len(entries))
	}
	if entries[0].ID != root.ID {
		t.Errorf("entries[0] should be the root entry")
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].CreatedAt.Before(entries[i-1].CreatedAt) {
			t.Errorf("entries not ordered oldest-first: %v before %v", entries[i].CreatedAt, entries[i-1].CreatedAt)
		}
	}
}

// TestEntryRepository_Pagination_UsesCreatedAtIDTieBreaker verifies the
// stable cursor docs/compat/aria-v1.5.11.md requires: two entries sharing
// the exact same created_at must still page deterministically via the id
// tie-breaker, never via offset or lexical ID order.
func TestEntryRepository_Pagination_UsesCreatedAtIDTieBreaker(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	sameTime := time.Now()

	var ids []string
	for i := 0; i < 3; i++ {
		root := mustCreateThreadAndRoot(t, db, actorID, sameTime)
		ids = append(ids, root.ID)
	}

	// Page through with limit 1, following the returned cursor each time,
	// and confirm every entry is seen exactly once with no gaps/repeats.
	seen := map[string]bool{}
	var cursor *domain.Cursor
	for range len(ids) {
		page, err := db.Entries.ListTimeline(t.Context(), domain.Page{After: cursor, Limit: 1}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 {
			t.Fatalf("page length = %d, want 1", len(page))
		}
		e := page[0]
		if seen[e.ID] {
			t.Fatalf("entry %s returned twice across pages", e.ID)
		}
		seen[e.ID] = true
		cursor = &domain.Cursor{CreatedAt: e.CreatedAt, ID: e.ID}
	}

	for _, id := range ids {
		if !seen[id] {
			t.Errorf("entry %s was never returned by pagination", id)
		}
	}

	final, err := db.Entries.ListTimeline(t.Context(), domain.Page{After: cursor, Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 0 {
		t.Errorf("expected no entries after the last page's cursor, got %d", len(final))
	}
}

func TestEntryRepository_ListTimeline_ExcludesArchivedAndHiddenByDefault(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()

	visible := mustCreateThreadAndRoot(t, db, actorID, now)
	archived := mustCreateThreadAndRoot(t, db, actorID, now.Add(time.Minute))
	hidden := mustCreateThreadAndRoot(t, db, actorID, now.Add(2*time.Minute))

	if err := db.Entries.SetArchived(t.Context(), archived.ID, true, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Entries.SetHidden(t.Context(), hidden.ID, true, now); err != nil {
		t.Fatal(err)
	}

	visibleOnly, err := db.Entries.ListTimeline(t.Context(), domain.Page{Limit: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(visibleOnly) != 1 || visibleOnly[0].ID != visible.ID {
		t.Errorf("ListTimeline(includeHidden=false) = %v, want only %s", visibleOnly, visible.ID)
	}

	all, err := db.Entries.ListTimeline(t.Context(), domain.Page{Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("ListTimeline(includeHidden=true) returned %d entries, want 3", len(all))
	}
}

func TestEntryRepository_SetProcessingStatus(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	root := mustCreateThreadAndRoot(t, db, actorID, now)

	if err := db.Entries.SetProcessingStatus(t.Context(), root.ID, domain.ProcessingComplete, now); err != nil {
		t.Fatal(err)
	}
	got, err := db.Entries.Get(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProcessingStatus != domain.ProcessingComplete {
		t.Errorf("ProcessingStatus = %q, want %q", got.ProcessingStatus, domain.ProcessingComplete)
	}
}

func TestEntryRepository_SetArchived_ToggleClearsTimestamp(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	root := mustCreateThreadAndRoot(t, db, actorID, now)

	if err := db.Entries.SetArchived(t.Context(), root.ID, true, now); err != nil {
		t.Fatal(err)
	}
	got, err := db.Entries.Get(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchivedAt == nil {
		t.Fatal("ArchivedAt should be set")
	}

	if err := db.Entries.SetArchived(t.Context(), root.ID, false, now); err != nil {
		t.Fatal(err)
	}
	got, err = db.Entries.Get(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchivedAt != nil {
		t.Errorf("ArchivedAt should be cleared, got %v", got.ArchivedAt)
	}
}

func TestEntryRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Entries.Get(t.Context(), "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}
