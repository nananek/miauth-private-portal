package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func mustCreateFolder(t *testing.T, db *DB, ownerActorID string, parentID *string, createdAt time.Time) domain.Folder {
	t.Helper()
	f := domain.Folder{ID: domain.NewID(), OwnerActorID: ownerActorID, Name: "folder", ParentID: parentID, CreatedAt: createdAt}
	if err := db.Folders.Create(t.Context(), f); err != nil {
		t.Fatalf("create folder: %v", err)
	}
	return f
}

func TestFileRepository_CreateGet_RoundTripsEveryField(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	folder := mustCreateFolder(t, db, ownerID, nil, time.Now())

	width, height := 640, 480
	comment := "a comment"
	createdAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	f := domain.File{
		ID: domain.NewID(), OwnerActorID: &ownerID, Purpose: domain.FilePurposeAttachment,
		MIME: "image/png", ByteSize: 1024, SHA256: "sha256hex", MD5: "md5hex",
		StorageKey: "drive/" + domain.NewID(), Width: &width, Height: &height,
		Name: "photo.png", Comment: &comment, IsSensitive: true, FolderID: &folder.ID, CreatedAt: createdAt,
	}
	if err := db.Files.Create(t.Context(), f); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Files.Get(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != f.ID || *got.OwnerActorID != ownerID || got.Purpose != domain.FilePurposeAttachment ||
		got.MIME != f.MIME || got.ByteSize != f.ByteSize || got.SHA256 != f.SHA256 || got.MD5 != f.MD5 ||
		got.StorageKey != f.StorageKey || *got.Width != width || *got.Height != height ||
		got.Name != f.Name || *got.Comment != comment || !got.IsSensitive || *got.FolderID != folder.ID ||
		!got.CreatedAt.Equal(createdAt) {
		t.Errorf("Get round-trip mismatch: got %+v, want fields from %+v", got, f)
	}
}

func TestFileRepository_CreateGet_NullableFieldsStayNil(t *testing.T) {
	db := newTestDB(t)

	f := domain.File{
		ID: domain.NewID(), OwnerActorID: nil, Purpose: domain.FilePurposeSourceFavicon,
		MIME: "image/x-icon", ByteSize: 1, SHA256: "s", MD5: "m", StorageKey: "drive/" + domain.NewID(),
		Width: nil, Height: nil, Name: "", Comment: nil, IsSensitive: false, FolderID: nil, CreatedAt: time.Now(),
	}
	if err := db.Files.Create(t.Context(), f); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Files.Get(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OwnerActorID != nil || got.Width != nil || got.Height != nil || got.Comment != nil || got.FolderID != nil {
		t.Errorf("got = %+v, want every nullable field nil", got)
	}
}

func TestFileRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Files.Get(t.Context(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFileRepository_ListByOwner_ScopedByFolderAndPaginated(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	otherOwnerID := mustCreateDistinctActor(t, db)
	folder := mustCreateFolder(t, db, ownerID, nil, time.Now())

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mustCreateFile := func(id string, owner string, folderID *string, at time.Time) domain.File {
		f := domain.File{
			ID: id, OwnerActorID: &owner, Purpose: domain.FilePurposeAttachment, MIME: "image/png",
			ByteSize: 1, SHA256: "s", MD5: "m", StorageKey: "drive/" + id, Name: "f", FolderID: folderID, CreatedAt: at,
		}
		if err := db.Files.Create(t.Context(), f); err != nil {
			t.Fatalf("seed file %s: %v", id, err)
		}
		return f
	}

	root1 := mustCreateFile("root1", ownerID, nil, base)
	root2 := mustCreateFile("root2", ownerID, nil, base.Add(time.Minute))
	root3 := mustCreateFile("root3", ownerID, nil, base.Add(2*time.Minute))
	mustCreateFile("in-folder", ownerID, &folder.ID, base.Add(3*time.Minute))
	mustCreateFile("other-owner-root", otherOwnerID, nil, base.Add(4*time.Minute))

	// Root listing must exclude the folder's own file and the other
	// owner's file, newest first.
	got, err := db.Files.ListByOwner(t.Context(), ownerID, nil, nil, 10)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(got) != 3 || got[0].ID != root3.ID || got[1].ID != root2.ID || got[2].ID != root1.ID {
		t.Fatalf("ListByOwner root = %v, want [root3, root2, root1]", ids(got))
	}

	// Paginating from root2's cursor returns only root1.
	page2, err := db.Files.ListByOwner(t.Context(), ownerID, nil, &domain.Cursor{CreatedAt: root2.CreatedAt, ID: root2.ID}, 10)
	if err != nil {
		t.Fatalf("ListByOwner page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != root1.ID {
		t.Fatalf("ListByOwner page2 = %v, want [root1]", ids(page2))
	}

	inFolder, err := db.Files.ListByOwner(t.Context(), ownerID, &folder.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListByOwner folder: %v", err)
	}
	if len(inFolder) != 1 || inFolder[0].ID != "in-folder" {
		t.Fatalf("ListByOwner folder = %v, want [in-folder]", ids(inFolder))
	}
}

func ids(files []domain.File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.ID
	}
	return out
}

func TestFileRepository_Update_OverwritesMutableColumnsOnly(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	folder := mustCreateFolder(t, db, ownerID, nil, time.Now())

	f := domain.File{
		ID: domain.NewID(), OwnerActorID: &ownerID, Purpose: domain.FilePurposeAttachment, MIME: "image/png",
		ByteSize: 1, SHA256: "s", MD5: "m", StorageKey: "drive/" + domain.NewID(), Name: "old", CreatedAt: time.Now(),
	}
	if err := db.Files.Create(t.Context(), f); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newComment := "new comment"
	f.Name = "new"
	f.Comment = &newComment
	f.IsSensitive = true
	f.FolderID = &folder.ID
	if err := db.Files.Update(t.Context(), f); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := db.Files.Get(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "new" || got.Comment == nil || *got.Comment != newComment || !got.IsSensitive || got.FolderID == nil || *got.FolderID != folder.ID {
		t.Errorf("got = %+v, want the updated fields", got)
	}
	// Immutable columns (MIME/ByteSize/SHA256/StorageKey) must be
	// unaffected by Update, which never touches them.
	if got.MIME != f.MIME || got.StorageKey != f.StorageKey {
		t.Errorf("Update must not change MIME/StorageKey, got %+v", got)
	}
}

func TestFileRepository_Update_UnknownIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	err := db.Files.Update(t.Context(), domain.File{ID: "does-not-exist", Name: "x"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFileRepository_Delete(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	f := domain.File{
		ID: domain.NewID(), OwnerActorID: &ownerID, Purpose: domain.FilePurposeAttachment, MIME: "image/png",
		ByteSize: 1, SHA256: "s", MD5: "m", StorageKey: "drive/" + domain.NewID(), Name: "f", CreatedAt: time.Now(),
	}
	if err := db.Files.Create(t.Context(), f); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Files.Delete(t.Context(), f.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.Files.Get(t.Context(), f.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestFileRepository_Delete_UnknownIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.Files.Delete(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFileRepository_SumByteSizeByOwner(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	otherOwnerID := mustCreateDistinctActor(t, db)

	for i, size := range []int64{100, 250, 4096} {
		f := domain.File{
			ID: domain.NewID(), OwnerActorID: &ownerID, Purpose: domain.FilePurposeAttachment, MIME: "image/png",
			ByteSize: size, SHA256: "s", MD5: "m", StorageKey: "drive/sum" + string(rune('a'+i)), Name: "f", CreatedAt: time.Now(),
		}
		if err := db.Files.Create(t.Context(), f); err != nil {
			t.Fatalf("seed file %d: %v", i, err)
		}
	}
	other := domain.File{
		ID: domain.NewID(), OwnerActorID: &otherOwnerID, Purpose: domain.FilePurposeAttachment, MIME: "image/png",
		ByteSize: 999999, SHA256: "s", MD5: "m", StorageKey: "drive/other", Name: "f", CreatedAt: time.Now(),
	}
	if err := db.Files.Create(t.Context(), other); err != nil {
		t.Fatalf("seed other-owner file: %v", err)
	}

	sum, err := db.Files.SumByteSizeByOwner(t.Context(), ownerID)
	if err != nil {
		t.Fatalf("SumByteSizeByOwner: %v", err)
	}
	if sum != 100+250+4096 {
		t.Errorf("sum = %d, want %d", sum, 100+250+4096)
	}
}

func TestFileRepository_SumByteSizeByOwner_NoFilesIsZero(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	sum, err := db.Files.SumByteSizeByOwner(t.Context(), ownerID)
	if err != nil {
		t.Fatalf("SumByteSizeByOwner: %v", err)
	}
	if sum != 0 {
		t.Errorf("sum = %d, want 0", sum)
	}
}

func TestFileRepository_CountByFolder(t *testing.T) {
	db := newTestDB(t)
	ownerID := mustCreateActor(t, db)
	folder := mustCreateFolder(t, db, ownerID, nil, time.Now())
	otherFolder := mustCreateFolder(t, db, ownerID, nil, time.Now())

	for i := 0; i < 2; i++ {
		f := domain.File{
			ID: domain.NewID(), OwnerActorID: &ownerID, Purpose: domain.FilePurposeAttachment, MIME: "image/png",
			ByteSize: 1, SHA256: "s", MD5: "m", StorageKey: "drive/count" + string(rune('a'+i)), Name: "f",
			FolderID: &folder.ID, CreatedAt: time.Now(),
		}
		if err := db.Files.Create(t.Context(), f); err != nil {
			t.Fatalf("seed file %d: %v", i, err)
		}
	}

	n, err := db.Files.CountByFolder(t.Context(), folder.ID)
	if err != nil {
		t.Fatalf("CountByFolder: %v", err)
	}
	if n != 2 {
		t.Errorf("CountByFolder(folder) = %d, want 2", n)
	}
	n, err = db.Files.CountByFolder(t.Context(), otherFolder.ID)
	if err != nil {
		t.Fatalf("CountByFolder: %v", err)
	}
	if n != 0 {
		t.Errorf("CountByFolder(otherFolder) = %d, want 0", n)
	}
}
