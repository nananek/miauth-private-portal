package drive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// fakeStorage is an in-memory Storage for Service tests: business-rule
// tests (ownership, folder containment, partial updates, ...) should not
// need real disk or network I/O to fail for reasons unrelated to what
// they are testing.
type fakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeStorage() *fakeStorage { return &fakeStorage{objects: map[string][]byte{}} }

func (f *fakeStorage) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[key]; exists {
		return ErrKeyExists
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = data
	return nil
}

func (f *fakeStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, notFound(key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeStorage) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

type testDriveService struct {
	*Service
	db      *sqlite.DB
	storage *fakeStorage
}

func newTestDriveService(t *testing.T, cfg Config) *testDriveService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 10 << 20
	}
	if cfg.MaxImageWidth == 0 {
		cfg.MaxImageWidth = 8000
	}
	if cfg.MaxImageHeight == 0 {
		cfg.MaxImageHeight = 8000
	}

	storage := newFakeStorage()
	httpClient := safehttp.NewClient(safehttp.Config{
		MaxRedirects: 3,
		// httptest.Server serves plain http:// and binds 127.0.0.1: both
		// would be rejected by production's policy (https-only, public
		// IPs only), which is exactly what a test exercising
		// UploadFromURL's own logic — not safehttp's already-tested
		// scheme/IP policy — needs to bypass.
		AllowInsecureHTTP: true,
		AllowIPForTesting: func(net.IP) bool { return true },
	})
	return &testDriveService{
		Service: NewService(storage, httpClient, db.Repos, cfg),
		db:      db,
		storage: storage,
	}
}

// strPtrPtr builds a **string pointing at a *string pointing at s, for
// FileUpdate.FolderID/FolderUpdate.ParentID's double-pointer "field
// present, set to this value" case — Go does not allow taking the
// address of an address-of expression directly (&(&s) is not valid),
// so this exists to make call sites below readable.
func strPtrPtr(s string) **string {
	p := &s
	return &p
}

func mustCreateTestActor(t *testing.T, db *sqlite.DB) string {
	t.Helper()
	a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), a); err != nil {
		t.Fatalf("create actor: %v", err)
	}
	return a.ID
}

func TestService_CreateFile_Success(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	data := encodePNG(t, 10, 20)

	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Name: "photo.png", Data: data})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if f.Purpose != domain.FilePurposeAttachment {
		t.Errorf("Purpose = %q, want %q", f.Purpose, domain.FilePurposeAttachment)
	}
	if f.MIME != "image/png" {
		t.Errorf("MIME = %q, want image/png", f.MIME)
	}
	if f.Width == nil || *f.Width != 10 || f.Height == nil || *f.Height != 20 {
		t.Errorf("Width/Height = %v/%v, want 10/20", f.Width, f.Height)
	}
	if f.SHA256 == "" || f.MD5 == "" || f.SHA256 == f.MD5 {
		t.Errorf("SHA256/MD5 = %q/%q, want both set and different", f.SHA256, f.MD5)
	}
	if !ts.storage.has(f.StorageKey) {
		t.Errorf("storage does not have key %q after CreateFile", f.StorageKey)
	}
	got, err := ts.db.Files.Get(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("Files.Get: %v", err)
	}
	if got.Name != "photo.png" {
		t.Errorf("persisted Name = %q, want photo.png", got.Name)
	}
}

func TestService_CreateSystemFile_HasNoOwner(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	f, err := ts.CreateSystemFile(t.Context(), domain.FilePurposeSourceFavicon, "favicon.png", encodePNG(t, 4, 4))
	if err != nil {
		t.Fatalf("CreateSystemFile: %v", err)
	}
	if f.OwnerActorID != nil {
		t.Errorf("OwnerActorID = %v, want nil", f.OwnerActorID)
	}
	if f.Purpose != domain.FilePurposeSourceFavicon {
		t.Errorf("Purpose = %q, want %q", f.Purpose, domain.FilePurposeSourceFavicon)
	}
	if !ts.storage.has(f.StorageKey) {
		t.Error("storage does not have the created file's key")
	}
}

func TestService_CreateSystemFile_RejectsAttachmentPurpose(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	_, err := ts.CreateSystemFile(t.Context(), domain.FilePurposeAttachment, "x.png", encodePNG(t, 4, 4))
	if err == nil {
		t.Error("expected an error when using FilePurposeAttachment with CreateSystemFile")
	}
}

func TestService_CreateFile_EmptyNameGetsADefault(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if f.Name == "" {
		t.Error("Name is empty, want a generated default")
	}
}

func TestService_CreateFile_RejectsOversizedData(t *testing.T) {
	ts := newTestDriveService(t, Config{MaxFileBytes: 10})
	owner := mustCreateTestActor(t, ts.db)
	_, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 10, 10)})
	if !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("err = %v, want ErrFileTooLarge", err)
	}
}

func TestService_CreateFile_RejectsSVG(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`)
	_, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: svg})
	if !errors.Is(err, ErrInvalidImage) {
		t.Errorf("err = %v, want ErrInvalidImage", err)
	}
}

func TestService_CreateFile_RejectsUnknownFolder(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	unknown := domain.NewID()
	_, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, FolderID: &unknown, Data: encodePNG(t, 4, 4)})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestService_CreateFile_RejectsAnotherOwnersFolder(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	other := mustCreateTestActor(t, ts.db)
	folder, err := ts.CreateFolder(t.Context(), other, "other's folder", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	_, err = ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, FolderID: &folder.ID, Data: encodePNG(t, 4, 4)})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (ownership must not leak as a different error)", err)
	}
}

func TestService_UploadFromURL_Success(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	png := encodePNG(t, 6, 6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	f, err := ts.UploadFromURL(t.Context(), UploadFromURLInput{OwnerActorID: owner, URL: srv.URL + "/photo.png"})
	if err != nil {
		t.Fatalf("UploadFromURL: %v", err)
	}
	if f.Name != "photo.png" {
		t.Errorf("Name = %q, want photo.png (derived from the URL path)", f.Name)
	}
	if !ts.storage.has(f.StorageKey) {
		t.Error("storage does not have the uploaded file's key")
	}
}

func TestService_UploadFromURL_RejectsOversizedResponse(t *testing.T) {
	ts := newTestDriveService(t, Config{MaxFileBytes: 5})
	owner := mustCreateTestActor(t, ts.db)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(encodePNG(t, 10, 10))
	}))
	defer srv.Close()

	_, err := ts.UploadFromURL(t.Context(), UploadFromURLInput{OwnerActorID: owner, URL: srv.URL})
	if !errors.Is(err, ErrUploadFromURLFailed) {
		t.Errorf("err = %v, want ErrUploadFromURLFailed", err)
	}
}

func TestService_UploadFromURL_RejectsNonOKStatus(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := ts.UploadFromURL(t.Context(), UploadFromURLInput{OwnerActorID: owner, URL: srv.URL})
	if !errors.Is(err, ErrUploadFromURLFailed) {
		t.Errorf("err = %v, want ErrUploadFromURLFailed", err)
	}
}

func TestService_ListFiles_StaleUntilFileIDIsAnEmptyPage(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	if _, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	unknown := domain.NewID()
	files, err := ts.ListFiles(t.Context(), owner, nil, &unknown, 10)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("ListFiles with a stale untilFileID = %d files, want 0", len(files))
	}
}

func TestService_UpdateFile_PartialUpdateSemantics(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	comment := "original"
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Name: "a.png", Comment: &comment, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// Setting only Name must not touch Comment.
	newName := "b.png"
	updated, err := ts.UpdateFile(t.Context(), owner, f.ID, FileUpdate{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateFile(name only): %v", err)
	}
	if updated.Name != "b.png" || updated.Comment == nil || *updated.Comment != "original" {
		t.Errorf("got = %+v, want Name=b.png Comment=original", updated)
	}

	// An explicit-null Comment clears it.
	var nilComment *string
	updated, err = ts.UpdateFile(t.Context(), owner, f.ID, FileUpdate{Comment: &nilComment})
	if err != nil {
		t.Fatalf("UpdateFile(clear comment): %v", err)
	}
	if updated.Comment != nil {
		t.Errorf("Comment = %v, want nil after an explicit-null update", updated.Comment)
	}
	if updated.Name != "b.png" {
		t.Errorf("Name = %q, want b.png to survive the comment-only update", updated.Name)
	}
}

func TestService_UpdateFile_FolderIDMoveValidatesOwnership(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	other := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	otherFolder, err := ts.CreateFolder(t.Context(), other, "not yours", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	_, err = ts.UpdateFile(t.Context(), owner, f.ID, FileUpdate{FolderID: strPtrPtr(otherFolder.ID)})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestService_UpdateFile_NotOwnedIsNotFound(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	other := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	newName := "stolen.png"
	_, err = ts.UpdateFile(t.Context(), other, f.ID, FileUpdate{Name: &newName})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestService_DeleteFile_RemovesRowAndStorageObject(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := ts.DeleteFile(t.Context(), owner, f.ID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := ts.db.Files.Get(t.Context(), f.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Files.Get after delete: err = %v, want ErrNotFound", err)
	}
	if ts.storage.has(f.StorageKey) {
		t.Error("storage still has the object after DeleteFile")
	}
}

func TestService_DeleteFile_NotOwnedIsNotFoundAndLeavesFileIntact(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	other := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := ts.DeleteFile(t.Context(), other, f.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if !ts.storage.has(f.StorageKey) {
		t.Error("a denied delete must not remove the storage object")
	}
}

func TestService_OpenFile_NoOwnershipCheck(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	data := encodePNG(t, 4, 4)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: data})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// OpenFile is the public, unauthenticated GET /files/{id} path: it
	// deliberately takes no caller identity at all (see its doc comment).
	got, rc, err := ts.OpenFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body, data) {
		t.Error("OpenFile did not return the stored bytes")
	}
	if got.ID != f.ID {
		t.Errorf("got.ID = %q, want %q", got.ID, f.ID)
	}
}

func TestService_OpenFile_NotFound(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	_, _, err := ts.OpenFile(t.Context(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestService_Stats(t *testing.T) {
	ts := newTestDriveService(t, Config{CapacityBytes: 12345})
	owner := mustCreateTestActor(t, ts.db)
	if _, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	capacity, usage, err := ts.Stats(t.Context(), owner)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if capacity != 12345 {
		t.Errorf("capacity = %d, want 12345", capacity)
	}
	if usage <= 0 {
		t.Errorf("usage = %d, want > 0 after one upload", usage)
	}
}

func TestService_ShowFolder_ReportsChildCounts(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	parent, err := ts.CreateFolder(t.Context(), owner, "parent", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if _, err := ts.CreateFolder(t.Context(), owner, "child", &parent.ID); err != nil {
		t.Fatalf("CreateFolder(child): %v", err)
	}
	if _, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, FolderID: &parent.ID, Data: encodePNG(t, 4, 4)}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	_, foldersCount, filesCount, err := ts.ShowFolder(t.Context(), owner, parent.ID)
	if err != nil {
		t.Fatalf("ShowFolder: %v", err)
	}
	if foldersCount != 1 || filesCount != 1 {
		t.Errorf("foldersCount/filesCount = %d/%d, want 1/1", foldersCount, filesCount)
	}
}

func TestService_UpdateFolder_RejectsMovingIntoItself(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFolder(t.Context(), owner, "f", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	_, err = ts.UpdateFolder(t.Context(), owner, f.ID, FolderUpdate{ParentID: strPtrPtr(f.ID)})
	if !errors.Is(err, ErrFolderCycle) {
		t.Errorf("err = %v, want ErrFolderCycle", err)
	}
}

func TestService_UpdateFolder_RejectsMovingIntoOwnDescendant(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	grandparent, err := ts.CreateFolder(t.Context(), owner, "grandparent", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	parent, err := ts.CreateFolder(t.Context(), owner, "parent", &grandparent.ID)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	child, err := ts.CreateFolder(t.Context(), owner, "child", &parent.ID)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	// Moving grandparent under its own grandchild would create a cycle.
	_, err = ts.UpdateFolder(t.Context(), owner, grandparent.ID, FolderUpdate{ParentID: strPtrPtr(child.ID)})
	if !errors.Is(err, ErrFolderCycle) {
		t.Errorf("err = %v, want ErrFolderCycle", err)
	}
}

func TestService_UpdateFolder_ClearsParentToRoot(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	parent, err := ts.CreateFolder(t.Context(), owner, "parent", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	child, err := ts.CreateFolder(t.Context(), owner, "child", &parent.ID)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	var nilParent *string
	updated, err := ts.UpdateFolder(t.Context(), owner, child.ID, FolderUpdate{ParentID: &nilParent})
	if err != nil {
		t.Fatalf("UpdateFolder: %v", err)
	}
	if updated.ParentID != nil {
		t.Errorf("ParentID = %v, want nil", updated.ParentID)
	}
}

func TestService_DeleteFolder_RejectsNonEmptyWithFile(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	folder, err := ts.CreateFolder(t.Context(), owner, "f", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if _, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, FolderID: &folder.ID, Data: encodePNG(t, 4, 4)}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := ts.DeleteFolder(t.Context(), owner, folder.ID); !errors.Is(err, ErrFolderNotEmpty) {
		t.Errorf("err = %v, want ErrFolderNotEmpty", err)
	}
}

func TestService_DeleteFolder_RejectsNonEmptyWithSubfolder(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	parent, err := ts.CreateFolder(t.Context(), owner, "parent", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if _, err := ts.CreateFolder(t.Context(), owner, "child", &parent.ID); err != nil {
		t.Fatalf("CreateFolder(child): %v", err)
	}
	if err := ts.DeleteFolder(t.Context(), owner, parent.ID); !errors.Is(err, ErrFolderNotEmpty) {
		t.Errorf("err = %v, want ErrFolderNotEmpty", err)
	}
}

func TestService_DeleteFolder_EmptySucceeds(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	folder, err := ts.CreateFolder(t.Context(), owner, "f", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := ts.DeleteFolder(t.Context(), owner, folder.ID); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if _, err := ts.db.Folders.Get(t.Context(), folder.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Folders.Get after delete: err = %v, want ErrNotFound", err)
	}
}
