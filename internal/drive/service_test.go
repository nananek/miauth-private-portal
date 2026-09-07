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

func (f *fakeStorage) List(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	return keys, nil
}

func (f *fakeStorage) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeStorage) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects)
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

// TestService_UploadFromURL_PreservesPolicyViolationInErrorChain is
// Issue #77 PR7's SSRF regression test: ErrUploadFromURLFailed's own doc
// comment promises callers can distinguish an SSRF policy rejection with
// errors.Is(err, safehttp.ErrPolicyViolation) — a single fmt.Errorf
// "%w: %v" (as opposed to "%w: %w") would silently drop the original
// safehttp.ErrPolicyViolation from the returned error's chain, breaking
// that promise without failing any other existing assertion (the wire
// layer maps ErrUploadFromURLFailed to a generic client error either
// way, so only an errors.Is check like this one catches a regression
// here — this test previously caught exactly that bug, now fixed).
func TestService_UploadFromURL_PreservesPolicyViolationInErrorChain(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	// Swap in a strict, production-shaped safehttp.Client for this one
	// call: newTestDriveService's own client always sets
	// AllowIPForTesting to bypass the very policy this test exercises.
	ts.Service.httpClient = safehttp.NewClient(safehttp.Config{MaxRedirects: 3})

	_, err := ts.UploadFromURL(t.Context(), UploadFromURLInput{OwnerActorID: owner, URL: "http://127.0.0.1:1/x"})
	if !errors.Is(err, ErrUploadFromURLFailed) {
		t.Errorf("err = %v, want it to satisfy errors.Is(err, ErrUploadFromURLFailed)", err)
	}
	if !errors.Is(err, safehttp.ErrPolicyViolation) {
		t.Errorf("err = %v, want it to also satisfy errors.Is(err, safehttp.ErrPolicyViolation) per ErrUploadFromURLFailed's own doc comment", err)
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

func TestService_ValidateAttachmentFiles_Success(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	a, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	b, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := ts.ValidateAttachmentFiles(t.Context(), owner, []string{a.ID, b.ID}); err != nil {
		t.Errorf("ValidateAttachmentFiles: %v", err)
	}
}

func TestService_ValidateAttachmentFiles_RejectsNotOwned(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	other := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: other, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := ts.ValidateAttachmentFiles(t.Context(), owner, []string{f.ID}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestService_ValidateAttachmentFiles_RejectsWrongPurpose covers an
// owned, non-attachment-purpose file — a shape no current public
// creation path (CreateFile always sets FilePurposeAttachment;
// CreateSystemFile always sets a nil owner) can produce, so this test
// writes the files row directly through the repository to exercise
// ValidateAttachmentFiles' own purpose check in isolation from
// getOwnedFile's ownership check (which alone would already reject a
// nil-owner CreateSystemFile file, without ever reaching the purpose
// branch this test targets).
func TestService_ValidateAttachmentFiles_RejectsWrongPurpose(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	f := domain.File{
		ID: domain.NewID(), OwnerActorID: &owner, Purpose: domain.FilePurposeAvatar,
		MIME: "image/png", ByteSize: 4, SHA256: "sha", MD5: "md5", StorageKey: domain.NewID(),
		Name: "avatar.png", CreatedAt: time.Now(),
	}
	if err := ts.db.Files.Create(t.Context(), f); err != nil {
		t.Fatalf("create test file: %v", err)
	}
	if err := ts.ValidateAttachmentFiles(t.Context(), owner, []string{f.ID}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (a non-attachment-purpose file must be rejected the same way an unowned one is)", err)
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

// failingStorage wraps a working Storage, forcing whichever of its
// operations has a non-nil injected error to fail instead of delegating
// — Issue #77 PR7's storage-failure-path tests: a working files row
// whose backing object briefly or permanently errors out on the storage
// backend (a disk I/O error, an S3 outage) is a distinct failure mode
// from the row itself not existing (domain.ErrNotFound), and must not
// be confused with it by any caller.
type failingStorage struct {
	Storage
	failGet    error
	failPut    error
	failDelete error
}

func (f *failingStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.failGet != nil {
		return nil, f.failGet
	}
	return f.Storage.Get(ctx, key)
}

func (f *failingStorage) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if f.failPut != nil {
		return f.failPut
	}
	return f.Storage.Put(ctx, key, r, size)
}

func (f *failingStorage) Delete(ctx context.Context, key string) error {
	if f.failDelete != nil {
		return f.failDelete
	}
	return f.Storage.Delete(ctx, key)
}

// TestService_OpenFile_StorageBackendFailureIsNotConfusedWithNotFound
// covers a files row that exists and is owned correctly, but whose
// backing storage object briefly errors out (e.g. a disk I/O error or an
// S3 outage) — OpenFile must surface that as a plain error, never as
// domain.ErrNotFound, since internal/httpserver's handleFilesShow maps
// ErrNotFound specifically to 404 and everything else to 500: confusing
// the two would make a transient storage outage look like the file was
// deleted.
func TestService_OpenFile_StorageBackendFailureIsNotConfusedWithNotFound(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	ts.Service.storage = &failingStorage{Storage: ts.storage, failGet: errors.New("simulated storage backend outage")}

	_, _, err = ts.OpenFile(t.Context(), f.ID)
	if err == nil {
		t.Fatal("expected an error when the storage backend fails")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, must not satisfy errors.Is(err, domain.ErrNotFound) — the row exists, only the backend failed", err)
	}
}

// TestService_CreateFile_CleansUpStorageObjectWhenRowInsertFails covers
// storeValidatedImage's own best-effort cleanup comment: if the object
// is stored but the files row then fails to insert (here, forced via an
// owner_actor_id that names no actor row, a foreign-key violation), the
// just-stored object must not survive as a permanently unreferenced
// orphan.
func TestService_CreateFile_CleansUpStorageObjectWhenRowInsertFails(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	unknownOwner := domain.NewID()
	_, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: unknownOwner, Data: encodePNG(t, 4, 4)})
	if err == nil {
		t.Fatal("expected an error for a foreign-key-violating owner_actor_id")
	}
	if n := ts.storage.count(); n != 0 {
		t.Errorf("storage has %d object(s) after a failed row insert, want 0 (cleanup should have deleted it)", n)
	}
}

// TestService_DeleteFile_SucceedsEvenWhenStorageDeleteFails covers
// DeleteFile's own best-effort object-delete comment: the files row is
// this service's source of truth for "does this file exist", so a
// failing storage-side delete (a backend outage at the worst possible
// moment) must not prevent the row itself from being removed — PR7's
// orphan GC (RunOrphanGC) is the safety net for whatever object this
// then leaves behind.
func TestService_DeleteFile_SucceedsEvenWhenStorageDeleteFails(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	f, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	ts.Service.storage = &failingStorage{Storage: ts.storage, failDelete: errors.New("simulated storage backend outage")}

	if err := ts.DeleteFile(t.Context(), owner, f.ID); err != nil {
		t.Fatalf("DeleteFile: %v, want nil (row deletion must succeed despite the storage-side failure)", err)
	}
	if _, err := ts.db.Files.Get(t.Context(), f.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Files.Get after DeleteFile: err = %v, want ErrNotFound", err)
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

// TestService_RunOrphanGC_DeletesUnreferencedObjectOnly is Issue #77
// PR7's core orphan-GC contract test: a Storage object no files row
// references (simulating storeValidatedImage's own documented failure
// path — the row insert failing after the object was already stored,
// with its own best-effort cleanup itself failing) must be deleted,
// while a genuinely referenced file's object must survive untouched.
func TestService_RunOrphanGC_DeletesUnreferencedObjectOnly(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	kept, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	// An object with no files row at all — as storeValidatedImage's own
	// cleanup-failure comment describes, not producible through any
	// public Service method, so this test writes it to Storage directly.
	if err := ts.storage.Put(t.Context(), "drive/orphan", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put orphan: %v", err)
	}

	deleted, err := ts.RunOrphanGC(t.Context())
	if err != nil {
		t.Fatalf("RunOrphanGC: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if ts.storage.has("drive/orphan") {
		t.Error("orphaned object survived RunOrphanGC")
	}
	if !ts.storage.has(kept.StorageKey) {
		t.Error("RunOrphanGC deleted a referenced file's object")
	}
}

func TestService_RunOrphanGC_NoOrphansIsANoOp(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	owner := mustCreateTestActor(t, ts.db)
	if _, err := ts.CreateFile(t.Context(), CreateFileInput{OwnerActorID: owner, Data: encodePNG(t, 4, 4)}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	deleted, err := ts.RunOrphanGC(t.Context())
	if err != nil {
		t.Fatalf("RunOrphanGC: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if ts.storage.count() != 1 {
		t.Errorf("storage has %d object(s) after a no-op sweep, want 1", ts.storage.count())
	}
}

// TestService_RunOrphanGC_OneFailedDeleteDoesNotStopTheSweep covers
// RunOrphanGC's own errors.Join comment: a storage backend failing to
// delete one orphan must not prevent every other orphan from still
// being reclaimed in the same sweep.
func TestService_RunOrphanGC_OneFailedDeleteDoesNotStopTheSweep(t *testing.T) {
	ts := newTestDriveService(t, Config{})
	if err := ts.storage.Put(t.Context(), "drive/orphan-a", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put orphan-a: %v", err)
	}
	if err := ts.storage.Put(t.Context(), "drive/orphan-b", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put orphan-b: %v", err)
	}
	failing := &failingDeleteKeyStorage{Storage: ts.storage, failKey: "drive/orphan-a"}
	ts.Service.storage = failing

	deleted, err := ts.RunOrphanGC(t.Context())
	if err == nil {
		t.Error("expected a non-nil error reporting the one failed delete")
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (only the non-failing orphan)", deleted)
	}
	if !ts.storage.has("drive/orphan-a") {
		t.Error("the object whose delete failed must still be present")
	}
	if ts.storage.has("drive/orphan-b") {
		t.Error("drive/orphan-b should have been deleted")
	}
}

// failingDeleteKeyStorage fails Delete for exactly one key, letting
// every other Storage operation (including every other Delete call)
// pass through — narrower than failingStorage, which fails every call
// to a given method regardless of key.
type failingDeleteKeyStorage struct {
	Storage
	failKey string
}

func (f *failingDeleteKeyStorage) Delete(ctx context.Context, key string) error {
	if key == f.failKey {
		return errors.New("simulated storage backend outage")
	}
	return f.Storage.Delete(ctx, key)
}
