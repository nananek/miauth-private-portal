package sqlite

import (
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// mustCreateAttachmentFile inserts a minimal attachment-purpose files
// row owned by ownerActorID, for entry_files tests that only need a
// valid files.id foreign key target.
func mustCreateAttachmentFile(t *testing.T, db *DB, ownerActorID string) domain.File {
	t.Helper()
	f := domain.File{
		ID: domain.NewID(), OwnerActorID: &ownerActorID, Purpose: domain.FilePurposeAttachment,
		MIME: "image/png", ByteSize: 1, SHA256: domain.NewID(), MD5: domain.NewID(),
		StorageKey: domain.NewID(), Name: "a.png", CreatedAt: time.Now(),
	}
	if err := db.Files.Create(t.Context(), f); err != nil {
		t.Fatalf("create file: %v", err)
	}
	return f
}

func TestEntryFileRepository_CreateAndListFilesByEntry_PreservesOrder(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	root := mustCreateThreadAndRoot(t, db, actorID, time.Now())
	a := mustCreateAttachmentFile(t, db, actorID)
	b := mustCreateAttachmentFile(t, db, actorID)
	c := mustCreateAttachmentFile(t, db, actorID)

	// Deliberately not sorted by ID/creation order: ListFilesByEntry must
	// return exactly this order back, not some other stable ordering.
	if err := db.EntryFiles.Create(t.Context(), root.ID, []string{c.ID, a.ID, b.ID}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	files, err := db.EntryFiles.ListFilesByEntry(t.Context(), root.ID)
	if err != nil {
		t.Fatalf("ListFilesByEntry: %v", err)
	}
	if len(files) != 3 || files[0].ID != c.ID || files[1].ID != a.ID || files[2].ID != b.ID {
		t.Fatalf("ListFilesByEntry order = %v, want [%s, %s, %s]", files, c.ID, a.ID, b.ID)
	}
}

func TestEntryFileRepository_ListFilesByEntry_EmptyForUnattachedEntry(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	root := mustCreateThreadAndRoot(t, db, actorID, time.Now())

	files, err := db.EntryFiles.ListFilesByEntry(t.Context(), root.ID)
	if err != nil {
		t.Fatalf("ListFilesByEntry: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("ListFilesByEntry = %v, want empty", files)
	}
}

// TestEntryFileRepository_SameFileAttachedToMultipleEntries is Issue
// #77 PR6's core many-to-many contract test — PR0's trace found a single
// drive file can be attached to more than one note, correcting
// plan-77 v2's tentative 1:1 framing.
func TestEntryFileRepository_SameFileAttachedToMultipleEntries(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	entry1 := mustCreateThreadAndRoot(t, db, actorID, time.Now())
	entry2 := mustCreateThreadAndRoot(t, db, actorID, time.Now())
	f := mustCreateAttachmentFile(t, db, actorID)

	if err := db.EntryFiles.Create(t.Context(), entry1.ID, []string{f.ID}); err != nil {
		t.Fatalf("Create entry1: %v", err)
	}
	if err := db.EntryFiles.Create(t.Context(), entry2.ID, []string{f.ID}); err != nil {
		t.Fatalf("Create entry2: %v", err)
	}

	count, err := db.EntryFiles.CountByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("CountByFile: %v", err)
	}
	if count != 2 {
		t.Errorf("CountByFile = %d, want 2", count)
	}

	entries, err := db.EntryFiles.ListEntriesByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("ListEntriesByFile: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.ID] = true
	}
	if len(entries) != 2 || !got[entry1.ID] || !got[entry2.ID] {
		t.Errorf("ListEntriesByFile = %v, want exactly [%s, %s]", entries, entry1.ID, entry2.ID)
	}
}

func TestEntryFileRepository_CountByFile_ZeroForUnattachedFile(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	f := mustCreateAttachmentFile(t, db, actorID)

	count, err := db.EntryFiles.CountByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("CountByFile: %v", err)
	}
	if count != 0 {
		t.Errorf("CountByFile = %d, want 0", count)
	}
}
