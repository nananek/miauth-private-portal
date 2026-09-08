package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestFolderRepository_CreateGet_RoundTripsEveryField(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	parent := mustCreateFolder(t, db, ownerID, nil, time.Now())

	createdAt := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	f := domain.Folder{ID: domain.NewID(), OwnerActorID: ownerID, Name: "child", ParentID: &parent.ID, CreatedAt: createdAt}
	if err := db.Folders.Create(t.Context(), f); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Folders.Get(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != f.ID || got.OwnerActorID != ownerID || got.Name != "child" || got.ParentID == nil || *got.ParentID != parent.ID || !got.CreatedAt.Equal(createdAt) {
		t.Errorf("Get round-trip mismatch: got %+v, want fields from %+v", got, f)
	}
}

func TestFolderRepository_CreateGet_RootFolderHasNilParent(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	f := mustCreateFolder(t, db, ownerID, nil, time.Now())

	got, err := db.Folders.Get(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ParentID != nil {
		t.Errorf("ParentID = %v, want nil for a root folder", got.ParentID)
	}
}

func TestFolderRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Folders.Get(t.Context(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFolderRepository_Create_RejectsUnknownOwner(t *testing.T) {
	db := newTestDB(t)
	err := db.Folders.Create(t.Context(), domain.Folder{ID: domain.NewID(), OwnerActorID: "no-such-actor", Name: "x", CreatedAt: time.Now()})
	if err == nil {
		t.Error("expected an error for an unknown owner_actor_id")
	}
}

func TestFolderRepository_ListByOwner_ScopedByParentAndPaginated(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	otherOwnerID := mustCreateDistinctActor(t, db)

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// Older than root1/root2 below, so it sorts last in the root listing
	// — mustCreateFolder's own time.Now() default would instead make it
	// the newest (test execution time is long after 2024), which is not
	// what this test wants to exercise.
	parent := mustCreateFolder(t, db, ownerID, nil, base.Add(-time.Hour))
	mk := func(id, owner string, parentID *string, at time.Time) domain.Folder {
		f := domain.Folder{ID: id, OwnerActorID: owner, Name: "f", ParentID: parentID, CreatedAt: at}
		if err := db.Folders.Create(t.Context(), f); err != nil {
			t.Fatalf("seed folder %s: %v", id, err)
		}
		return f
	}

	root1 := mk("root1", ownerID, nil, base)
	root2 := mk("root2", ownerID, nil, base.Add(time.Minute))
	mk("child1", ownerID, &parent.ID, base.Add(2*time.Minute))
	mk("other-owner-root", otherOwnerID, nil, base.Add(3*time.Minute))

	// mustCreateFolder for `parent` itself is also a root folder created
	// before root1/root2, so the root listing has three entries newest
	// first: root2, root1, parent.
	got, err := db.Folders.ListByOwner(t.Context(), ownerID, nil, nil, 10)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(got) != 3 || got[0].ID != root2.ID || got[1].ID != root1.ID || got[2].ID != parent.ID {
		t.Fatalf("ListByOwner root = %v, want [root2, root1, parent]", folderIDs(got))
	}

	page2, err := db.Folders.ListByOwner(t.Context(), ownerID, nil, &domain.Cursor{CreatedAt: root1.CreatedAt, ID: root1.ID}, 10)
	if err != nil {
		t.Fatalf("ListByOwner page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != parent.ID {
		t.Fatalf("ListByOwner page2 = %v, want [parent]", folderIDs(page2))
	}

	children, err := db.Folders.ListByOwner(t.Context(), ownerID, &parent.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListByOwner children: %v", err)
	}
	if len(children) != 1 || children[0].ID != "child1" {
		t.Fatalf("ListByOwner children = %v, want [child1]", folderIDs(children))
	}
}

func folderIDs(folders []domain.Folder) []string {
	out := make([]string, len(folders))
	for i, f := range folders {
		out[i] = f.ID
	}
	return out
}

func TestFolderRepository_Update(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	f := mustCreateFolder(t, db, ownerID, nil, time.Now())
	newParent := mustCreateFolder(t, db, ownerID, nil, time.Now())

	f.Name = "renamed"
	f.ParentID = &newParent.ID
	if err := db.Folders.Update(t.Context(), f); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := db.Folders.Get(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "renamed" || got.ParentID == nil || *got.ParentID != newParent.ID {
		t.Errorf("got = %+v, want the updated fields", got)
	}
}

func TestFolderRepository_Update_ClearsParentToNil(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	parent := mustCreateFolder(t, db, ownerID, nil, time.Now())
	child := mustCreateFolder(t, db, ownerID, &parent.ID, time.Now())

	child.ParentID = nil
	if err := db.Folders.Update(t.Context(), child); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := db.Folders.Get(t.Context(), child.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ParentID != nil {
		t.Errorf("ParentID = %v, want nil after clearing", got.ParentID)
	}
}

func TestFolderRepository_Update_UnknownIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	err := db.Folders.Update(t.Context(), domain.Folder{ID: "does-not-exist", OwnerActorID: "x", Name: "y"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFolderRepository_Delete(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	f := mustCreateFolder(t, db, ownerID, nil, time.Now())

	if err := db.Folders.Delete(t.Context(), f.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.Folders.Get(t.Context(), f.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestFolderRepository_Delete_UnknownIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.Folders.Delete(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFolderRepository_CountChildFolders(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	parent := mustCreateFolder(t, db, ownerID, nil, time.Now())
	mustCreateFolder(t, db, ownerID, &parent.ID, time.Now())
	mustCreateFolder(t, db, ownerID, &parent.ID, time.Now())
	other := mustCreateFolder(t, db, ownerID, nil, time.Now())

	n, err := db.Folders.CountChildFolders(t.Context(), parent.ID)
	if err != nil {
		t.Fatalf("CountChildFolders: %v", err)
	}
	if n != 2 {
		t.Errorf("CountChildFolders(parent) = %d, want 2", n)
	}
	n, err = db.Folders.CountChildFolders(t.Context(), other.ID)
	if err != nil {
		t.Fatalf("CountChildFolders: %v", err)
	}
	if n != 0 {
		t.Errorf("CountChildFolders(other) = %d, want 0", n)
	}
}
