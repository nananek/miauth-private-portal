package drive

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
)

// Config bounds one Service's upload behavior. cmd/server builds it from
// internal/config.DriveConfig (Issue #77 PR1).
type Config struct {
	MaxFileBytes   int64
	MaxImageWidth  int
	MaxImageHeight int
	// CapacityBytes is what POST /api/drive reports as capacity —
	// cosmetic only (Aria's drive-stats screen renders a used/total
	// bar). This service enforces no real per-owner quota; MaxFileBytes
	// is the only upload-size bound that actually rejects anything.
	CapacityBytes int64
	// UploadFromURLTimeout bounds UploadFromURL's fetch. Zero means
	// defaultUploadFromURLTimeout.
	UploadFromURLTimeout time.Duration
}

const defaultUploadFromURLTimeout = 30 * time.Second

// Sentinel errors Service returns beyond domain.ErrNotFound (an unknown
// or not-owned file/folder — this package never distinguishes the two,
// the same generic-denial stance internal/httpserver's writeNoSuchNote
// already takes for notes) and ErrInvalidImage/ErrKeyExists from
// validate.go/storage.go.
var (
	// ErrFileTooLarge reports that an upload exceeds Config.MaxFileBytes.
	ErrFileTooLarge = errors.New("drive: file exceeds the configured maximum size")
	// ErrFolderNotEmpty reports that DeleteFolder was asked to remove a
	// folder that still directly contains a file or subfolder — Misskey
	// itself rejects this rather than cascading the delete, and Aria's
	// drive screen never attempts to delete a non-empty folder, but this
	// service rejects it explicitly regardless (AGENTS.md: unsupported/
	// unsafe operations must fail explicitly, not silently cascade).
	ErrFolderNotEmpty = errors.New("drive: folder is not empty")
	// ErrFolderCycle reports that a folder move would make a folder its
	// own descendant.
	ErrFolderCycle = errors.New("drive: cannot move a folder inside its own descendant")
	// ErrInvalidUploadURL reports that UploadFromURL's url did not even
	// parse as a request-able URL (a malformed value, not a policy
	// rejection — see safehttp.ErrPolicyViolation for that).
	ErrInvalidUploadURL = errors.New("drive: invalid upload-from-url URL")
	// ErrUploadFromURLFailed reports that fetching UploadFromURL's url
	// failed — a network error, a non-2xx response, an SSRF policy
	// rejection (errors.Is(err, safehttp.ErrPolicyViolation)), or a body
	// exceeding Config.MaxFileBytes.
	ErrUploadFromURLFailed = errors.New("drive: upload from URL failed")
	// ErrFileAttached reports that DeleteFile was asked to remove a file
	// still referenced by one or more entry_files rows (Issue #77 PR6).
	// Like ErrFolderNotEmpty, this service rejects the delete explicitly
	// rather than cascading it (which would silently detach a file from
	// an existing post's fileIds) or letting SQLite's own foreign-key
	// enforcement surface as an unhandled constraint-violation error
	// (AGENTS.md: unsupported/unsafe operations must fail explicitly).
	ErrFileAttached = errors.New("drive: file is attached to one or more notes")
)

// Service implements Issue #77 PR3's Drive business rules — ownership,
// folder containment, and keeping a files/folders row and its Storage
// object in sync — on top of internal/drive.Storage and PR1's raster-
// image validation. It depends on internal/domain but never on HTTP or
// the Misskey wire format; internal/httpserver's drive_handlers.go is
// what translates between the two.
type Service struct {
	storage    Storage
	httpClient *safehttp.Client
	repos      domain.Repos
	cfg        Config
	now        func() time.Time
}

// NewService builds a Service. httpClient backs UploadFromURL only
// (internal/ingest/safehttp.NewClient(...) in production; a test passes
// one built with safehttp.Config.AllowIPForTesting, mirroring
// internal/ingest/rss.NewAdapter's same constructor-injection shape).
func NewService(storage Storage, httpClient *safehttp.Client, repos domain.Repos, cfg Config) *Service {
	return &Service{
		storage:    storage,
		httpClient: httpClient,
		repos:      repos,
		cfg:        cfg,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// maxFolderDepth bounds checkNoCycle's ancestor walk. A real folder tree
// this deployment's single owner builds by hand will never come
// remotely close to it; it exists only so a corrupted parent_id chain
// (which nothing in this schema can otherwise happen) fails a move
// request instead of looping forever.
const maxFolderDepth = 1000

// getOwnedFile fetches fileID, returning domain.ErrNotFound both when it
// does not exist and when it exists but is not ownerActorID's — the same
// "do not let a denial distinguish the two" stance
// writeNoSuchNote/writeAuthenticationFailed already take elsewhere in
// this service.
func (s *Service) getOwnedFile(ctx context.Context, ownerActorID, fileID string) (domain.File, error) {
	f, err := s.repos.Files.Get(ctx, fileID)
	if err != nil {
		return domain.File{}, err
	}
	if f.OwnerActorID == nil || *f.OwnerActorID != ownerActorID {
		return domain.File{}, domain.ErrNotFound
	}
	return f, nil
}

func (s *Service) getOwnedFolder(ctx context.Context, ownerActorID, folderID string) (domain.Folder, error) {
	f, err := s.repos.Folders.Get(ctx, folderID)
	if err != nil {
		return domain.Folder{}, err
	}
	if f.OwnerActorID != ownerActorID {
		return domain.Folder{}, domain.ErrNotFound
	}
	return f, nil
}

// CreateFileInput is a fully-read, size-bounded upload: by the time this
// reaches Service, internal/httpserver has already read the multipart
// part (or UploadFromURL has already fetched the URL) to completion, so
// Storage.Put (which needs a known size up front) and sha256 hashing
// both have everything they need without re-reading anything.
type CreateFileInput struct {
	OwnerActorID string
	// FolderID, when non-nil, must name a folder OwnerActorID owns.
	FolderID    *string
	Name        string
	Comment     *string
	IsSensitive bool
	Data        []byte
}

// CreateFile validates in.Data as a raster image (ValidateImage — Issue
// #77 v2's "SVG/ベクター画像は許容しない" scope decision applies to every
// Drive upload, not only avatars, so there is no non-image path here),
// stores it, and records its metadata. Unlike real Misskey, this never
// performs content-hash deduplication: Aria's only two upload call
// sites (DriveFilesNotifier.upload/uploadBinary) always send
// `force: true`, which even real Misskey's own dedup check would honor
// as "skip it," so there is no observed call path this behavior would
// ever change.
func (s *Service) CreateFile(ctx context.Context, in CreateFileInput) (domain.File, error) {
	if in.FolderID != nil {
		if _, err := s.getOwnedFolder(ctx, in.OwnerActorID, *in.FolderID); err != nil {
			return domain.File{}, err
		}
	}
	ownerActorID := in.OwnerActorID
	return s.storeValidatedImage(ctx, storeValidatedImageInput{
		OwnerActorID: &ownerActorID,
		Purpose:      domain.FilePurposeAttachment,
		FolderID:     in.FolderID,
		Name:         in.Name,
		Comment:      in.Comment,
		IsSensitive:  in.IsSensitive,
		Data:         in.Data,
	})
}

// CreateSystemFile stores data as a files row with no owning actor and
// no folder — infrastructure-authored files no Drive API caller
// uploaded, currently only Issue #77 PR4/PR5's per-source favicon fetch
// (internal/ingest/favicon, called from cmd/server). purpose must not be
// FilePurposeAttachment: that value specifically means "a Drive API
// caller uploaded this," which CreateFile is what performs (it also has
// ownership/folder semantics CreateSystemFile deliberately does not).
func (s *Service) CreateSystemFile(ctx context.Context, purpose domain.FilePurpose, name string, data []byte) (domain.File, error) {
	if purpose == domain.FilePurposeAttachment {
		return domain.File{}, errors.New("drive: CreateSystemFile must not be used for attachment-purpose files")
	}
	return s.storeValidatedImage(ctx, storeValidatedImageInput{
		Purpose: purpose,
		Name:    name,
		Data:    data,
	})
}

// storeValidatedImageInput is CreateFile/CreateSystemFile's shared
// argument shape once each has resolved its own purpose/ownership rules
// — see storeValidatedImage's doc comment.
type storeValidatedImageInput struct {
	OwnerActorID *string
	Purpose      domain.FilePurpose
	FolderID     *string
	Name         string
	Comment      *string
	IsSensitive  bool
	Data         []byte
}

// storeValidatedImage is CreateFile and CreateSystemFile's shared body:
// validate as a raster image (ValidateImage — Issue #77 v2's "SVG/ベク
// ター画像は許容しない" scope decision applies to every file this package
// ever stores, not only Drive API uploads), store the bytes, and record
// the files row. Unlike real Misskey's Drive API, this never performs
// content-hash deduplication for CreateFile's own reasons (see that
// method's doc comment); CreateSystemFile has no client-facing "force"
// concept to begin with.
func (s *Service) storeValidatedImage(ctx context.Context, in storeValidatedImageInput) (domain.File, error) {
	if int64(len(in.Data)) > s.cfg.MaxFileBytes {
		return domain.File{}, ErrFileTooLarge
	}
	info, err := ValidateImage(in.Data, s.cfg.MaxImageWidth, s.cfg.MaxImageHeight)
	if err != nil {
		return domain.File{}, err
	}

	id := domain.NewID()
	sha256Sum := sha256.Sum256(in.Data)
	// MD5 is computed only for the DriveFile wire type's required `md5`
	// field (domain.File.MD5's doc comment) — not used for this table's
	// own identity/integrity hash, which stays SHA256.
	md5Sum := md5.Sum(in.Data)
	width, height := info.Width, info.Height
	f := domain.File{
		ID:           id,
		OwnerActorID: in.OwnerActorID,
		Purpose:      in.Purpose,
		MIME:         MIMEForFormat(info.Format),
		ByteSize:     int64(len(in.Data)),
		SHA256:       hex.EncodeToString(sha256Sum[:]),
		MD5:          hex.EncodeToString(md5Sum[:]),
		StorageKey:   "drive/" + id,
		Width:        &width,
		Height:       &height,
		Name:         defaultString(in.Name, defaultFileName(info.Format)),
		Comment:      in.Comment,
		IsSensitive:  in.IsSensitive,
		FolderID:     in.FolderID,
		CreatedAt:    s.now(),
	}

	if err := s.storage.Put(ctx, f.StorageKey, bytes.NewReader(in.Data), f.ByteSize); err != nil {
		return domain.File{}, fmt.Errorf("drive: store file: %w", err)
	}
	if err := s.repos.Files.Create(ctx, f); err != nil {
		// Best-effort cleanup: a files row failing to insert after the
		// object was already stored would otherwise leave a
		// permanently orphaned object under a key no row ever
		// references. Its own failure is not itself a reason to fail
		// this call differently — PR7's orphan GC is the safety net for
		// whatever this cleanup does not catch.
		_ = s.storage.Delete(ctx, f.StorageKey)
		return domain.File{}, fmt.Errorf("drive: create file row: %w", err)
	}
	return f, nil
}

// UploadFromURLInput mirrors misskey_dart's DriveFilesUploadFromUrlRequest
// (docs/compat/aria-v1.5.11.md's Drive API section); Marker is accepted
// on the wire but has no effect here — no Aria call site ever sets it
// (it is real Misskey's pagination cursor for a since-removed batch
// upload feature), so internal/httpserver never even decodes it into
// this struct.
type UploadFromURLInput struct {
	OwnerActorID string
	FolderID     *string
	URL          string
	Comment      *string
	IsSensitive  bool
}

// UploadFromURL fetches URL through internal/ingest/safehttp (the same
// SSRF-protected client RSS/favicon fetching uses — AGENTS.md: "External
// fetchers require fixed schemes, host validation, redirect limits, and
// SSRF protections"), then validates and stores it exactly like
// CreateFile. Real Misskey performs this asynchronously (the endpoint
// returns immediately and the file appears in the drive later); Aria's
// only call site (DriveFilesNotifier.uploadFromUrl) declares the method
// Future<void> and never inspects the response, so completing the fetch
// synchronously before responding is wire-compatible and needs no new
// durable-job machinery this deployment's scale does not otherwise
// require.
func (s *Service) UploadFromURL(ctx context.Context, in UploadFromURLInput) (domain.File, error) {
	if in.FolderID != nil {
		if _, err := s.getOwnedFolder(ctx, in.OwnerActorID, *in.FolderID); err != nil {
			return domain.File{}, err
		}
	}

	timeout := s.cfg.UploadFromURLTimeout
	if timeout <= 0 {
		timeout = defaultUploadFromURLTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, in.URL, nil)
	if err != nil {
		return domain.File{}, fmt.Errorf("%w: %v", ErrInvalidUploadURL, err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return domain.File{}, fmt.Errorf("%w: %v", ErrUploadFromURLFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return domain.File{}, fmt.Errorf("%w: upstream returned status %d", ErrUploadFromURLFailed, resp.StatusCode)
	}
	data, err := safehttp.ReadLimited(resp.Body, s.cfg.MaxFileBytes)
	if err != nil {
		return domain.File{}, fmt.Errorf("%w: %v", ErrUploadFromURLFailed, err)
	}

	return s.CreateFile(ctx, CreateFileInput{
		OwnerActorID: in.OwnerActorID,
		FolderID:     in.FolderID,
		Name:         fileNameFromURL(in.URL),
		Comment:      in.Comment,
		IsSensitive:  in.IsSensitive,
		Data:         data,
	})
}

// ListFiles returns ownerActorID's files directly inside folderID
// (nil means Drive's root), newest first. untilFileID, when non-empty,
// pages to files strictly older than that file — an untilFileID naming
// a file that no longer exists returns an empty page rather than an
// error, matching handleNotesTimeline's existing "stale untilId" fallback.
func (s *Service) ListFiles(ctx context.Context, ownerActorID string, folderID *string, untilFileID *string, limit int) ([]domain.File, error) {
	before, ok, err := s.fileCursor(ctx, untilFileID)
	if err != nil || !ok {
		return nil, err
	}
	return s.repos.Files.ListByOwner(ctx, ownerActorID, folderID, before, limit)
}

func (s *Service) fileCursor(ctx context.Context, untilFileID *string) (*domain.Cursor, bool, error) {
	if untilFileID == nil || *untilFileID == "" {
		return nil, true, nil
	}
	anchor, err := s.repos.Files.Get(ctx, *untilFileID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}, true, nil
}

// ShowFile returns fileID, or domain.ErrNotFound if it does not exist or
// is not ownerActorID's.
func (s *Service) ShowFile(ctx context.Context, ownerActorID, fileID string) (domain.File, error) {
	return s.getOwnedFile(ctx, ownerActorID, fileID)
}

// FileUpdate is a partial update to a file's mutable metadata. A nil
// field means "leave unchanged" — the outer pointer of Comment/FolderID
// distinguishes that from "field present" (non-nil), and the inner
// pointer distinguishes an explicit null (clear it) from a real value,
// matching Aria's own explicit-null-clears convention for these two
// fields (docs/compat/aria-v1.5.11.md's Drive API section:
// DriveFilesNotifier.move and .updateComment both opt out of the
// client's usual null-omission rule specifically so they can send an
// explicit null). Name/IsSensitive never need that third state: every
// observed Aria call site that sets one always sets it to a real value.
type FileUpdate struct {
	Name        *string
	IsSensitive *bool
	Comment     **string
	FolderID    **string
}

func (s *Service) UpdateFile(ctx context.Context, ownerActorID, fileID string, upd FileUpdate) (domain.File, error) {
	f, err := s.getOwnedFile(ctx, ownerActorID, fileID)
	if err != nil {
		return domain.File{}, err
	}
	if upd.Name != nil {
		f.Name = *upd.Name
	}
	if upd.IsSensitive != nil {
		f.IsSensitive = *upd.IsSensitive
	}
	if upd.Comment != nil {
		f.Comment = *upd.Comment
	}
	if upd.FolderID != nil {
		newFolderID := *upd.FolderID
		if newFolderID != nil {
			if _, err := s.getOwnedFolder(ctx, ownerActorID, *newFolderID); err != nil {
				return domain.File{}, err
			}
		}
		f.FolderID = newFolderID
	}
	if err := s.repos.Files.Update(ctx, f); err != nil {
		return domain.File{}, err
	}
	return f, nil
}

// DeleteFile removes fileID's row and best-effort deletes its backing
// object. A failed object delete does not fail this call: the row (this
// service's source of truth for "does this file exist") is already
// gone, and PR7's orphan GC exists specifically to reconcile whatever a
// best-effort delete here does not catch.
func (s *Service) DeleteFile(ctx context.Context, ownerActorID, fileID string) error {
	f, err := s.getOwnedFile(ctx, ownerActorID, fileID)
	if err != nil {
		return err
	}
	attached, err := s.repos.EntryFiles.CountByFile(ctx, f.ID)
	if err != nil {
		return err
	}
	if attached > 0 {
		return ErrFileAttached
	}
	if err := s.repos.Files.Delete(ctx, f.ID); err != nil {
		return err
	}
	_ = s.storage.Delete(ctx, f.StorageKey)
	return nil
}

// ValidateAttachmentFiles reports domain.ErrNotFound if any of fileIDs
// does not exist, is not owned by ownerActorID, or is not
// domain.FilePurposeAttachment — never distinguishing which of the
// three, the same generic-denial stance getOwnedFile already takes.
// internal/httpserver calls this before creating a note with fileIds, so
// a bad ID fails the request before any entries/entry_files row is ever
// written (Issue #77 PR6).
func (s *Service) ValidateAttachmentFiles(ctx context.Context, ownerActorID string, fileIDs []string) error {
	for _, id := range fileIDs {
		f, err := s.getOwnedFile(ctx, ownerActorID, id)
		if err != nil {
			return err
		}
		if f.Purpose != domain.FilePurposeAttachment {
			return domain.ErrNotFound
		}
	}
	return nil
}

// OpenFile opens fileID for the anonymous, unauthenticated GET /files/
// route (internal/httpserver/drive_handlers.go): PR0's trace found
// Aria's image cache manager issues "an ordinary GET with no special
// headers" for DriveFile.url, so this route — and OpenFile, its only
// caller — takes no ownerActorID and performs no ownership check.
// fileID's own unguessability (domain.NewID's 128 bits of crypto/rand)
// is the access control, the same "unauthenticated but unguessable
// media URL" design every Misskey/Mastodon-shaped service already uses
// in production.
func (s *Service) OpenFile(ctx context.Context, fileID string) (domain.File, io.ReadCloser, error) {
	f, err := s.repos.Files.Get(ctx, fileID)
	if err != nil {
		return domain.File{}, nil, err
	}
	rc, err := s.storage.Get(ctx, f.StorageKey)
	if err != nil {
		return domain.File{}, nil, err
	}
	return f, rc, nil
}

// Stats backs POST /api/drive: capacity is Config.CapacityBytes
// (cosmetic, see its doc comment); usage sums every byte ownerActorID
// owns across every folder and purpose.
func (s *Service) Stats(ctx context.Context, ownerActorID string) (capacity, usage int64, err error) {
	usage, err = s.repos.Files.SumByteSizeByOwner(ctx, ownerActorID)
	if err != nil {
		return 0, 0, err
	}
	return s.cfg.CapacityBytes, usage, nil
}

// CreateFolder creates a folder. An empty name is replaced with a fixed
// default rather than rejected — Aria's DriveFoldersNotifier.create
// accepts a null name from its own "new folder" dialog and real Misskey
// applies a default server-side rather than erroring, so this mirrors
// that rather than inventing a validation error no traced client path
// exercises.
func (s *Service) CreateFolder(ctx context.Context, ownerActorID, name string, parentID *string) (domain.Folder, error) {
	if parentID != nil {
		if _, err := s.getOwnedFolder(ctx, ownerActorID, *parentID); err != nil {
			return domain.Folder{}, err
		}
	}
	f := domain.Folder{
		ID:           domain.NewID(),
		OwnerActorID: ownerActorID,
		Name:         defaultString(name, "Untitled Folder"),
		ParentID:     parentID,
		CreatedAt:    s.now(),
	}
	if err := s.repos.Folders.Create(ctx, f); err != nil {
		return domain.Folder{}, err
	}
	return f, nil
}

// ShowFolder returns folderID plus its direct child counts (misskey_dart's
// DriveFolder.foldersCount/filesCount — both nullable on the wire, but
// cheap enough for a single-folder detail view to always populate).
func (s *Service) ShowFolder(ctx context.Context, ownerActorID, folderID string) (folder domain.Folder, foldersCount, filesCount int, err error) {
	folder, err = s.getOwnedFolder(ctx, ownerActorID, folderID)
	if err != nil {
		return domain.Folder{}, 0, 0, err
	}
	if foldersCount, err = s.repos.Folders.CountChildFolders(ctx, folderID); err != nil {
		return domain.Folder{}, 0, 0, err
	}
	if filesCount, err = s.repos.Files.CountByFolder(ctx, folderID); err != nil {
		return domain.Folder{}, 0, 0, err
	}
	return folder, foldersCount, filesCount, nil
}

// ListFolders returns ownerActorID's folders directly inside parentID
// (nil means Drive's root), newest first — the same untilFolderID
// stale-cursor-is-an-empty-page contract ListFiles has.
func (s *Service) ListFolders(ctx context.Context, ownerActorID string, parentID *string, untilFolderID *string, limit int) ([]domain.Folder, error) {
	before, ok, err := s.folderCursor(ctx, untilFolderID)
	if err != nil || !ok {
		return nil, err
	}
	return s.repos.Folders.ListByOwner(ctx, ownerActorID, parentID, before, limit)
}

func (s *Service) folderCursor(ctx context.Context, untilFolderID *string) (*domain.Cursor, bool, error) {
	if untilFolderID == nil || *untilFolderID == "" {
		return nil, true, nil
	}
	anchor, err := s.repos.Folders.Get(ctx, *untilFolderID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}, true, nil
}

// FolderUpdate is FileUpdate's counterpart for folders. ParentID is a
// double-pointer for the same reason FileUpdate.FolderID is: Aria's
// DriveFoldersNotifier.move sends an explicit null to move a folder to
// Drive's root, opting out of the client's usual null-omission rule.
type FolderUpdate struct {
	Name     *string
	ParentID **string
}

func (s *Service) UpdateFolder(ctx context.Context, ownerActorID, folderID string, upd FolderUpdate) (domain.Folder, error) {
	f, err := s.getOwnedFolder(ctx, ownerActorID, folderID)
	if err != nil {
		return domain.Folder{}, err
	}
	if upd.Name != nil {
		f.Name = *upd.Name
	}
	if upd.ParentID != nil {
		newParentID := *upd.ParentID
		if newParentID != nil {
			if *newParentID == folderID {
				return domain.Folder{}, ErrFolderCycle
			}
			parent, err := s.getOwnedFolder(ctx, ownerActorID, *newParentID)
			if err != nil {
				return domain.Folder{}, err
			}
			if err := s.checkNoCycle(ctx, folderID, parent); err != nil {
				return domain.Folder{}, err
			}
		}
		f.ParentID = newParentID
	}
	if err := s.repos.Folders.Update(ctx, f); err != nil {
		return domain.Folder{}, err
	}
	return f, nil
}

// checkNoCycle walks parent's own ancestor chain, failing with
// ErrFolderCycle if folderID ever appears in it — moving folderID under
// one of its own descendants would otherwise create a cycle no listing
// or deletion query in this package could ever terminate on.
func (s *Service) checkNoCycle(ctx context.Context, folderID string, parent domain.Folder) error {
	cur := parent
	for range maxFolderDepth {
		if cur.ParentID == nil {
			return nil
		}
		if *cur.ParentID == folderID {
			return ErrFolderCycle
		}
		next, err := s.repos.Folders.Get(ctx, *cur.ParentID)
		if err != nil {
			return err
		}
		cur = next
	}
	// The bound above is far beyond any tree this deployment's single
	// owner could build by hand; reaching it means something is
	// malformed (or, in principle, a very deep chain) — either way,
	// refusing the move is safer than looping further.
	return ErrFolderCycle
}

// DeleteFolder removes folderID, or returns ErrFolderNotEmpty if it
// still directly contains a file or subfolder (see ErrFolderNotEmpty's
// doc comment).
func (s *Service) DeleteFolder(ctx context.Context, ownerActorID, folderID string) error {
	f, err := s.getOwnedFolder(ctx, ownerActorID, folderID)
	if err != nil {
		return err
	}
	childFolders, err := s.repos.Folders.CountChildFolders(ctx, f.ID)
	if err != nil {
		return err
	}
	childFiles, err := s.repos.Files.CountByFolder(ctx, f.ID)
	if err != nil {
		return err
	}
	if childFolders > 0 || childFiles > 0 {
		return ErrFolderNotEmpty
	}
	return s.repos.Folders.Delete(ctx, f.ID)
}

func defaultString(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// formatExtension names the file extension CreateFile's own generated
// default filename uses when a caller supplies none — cosmetic only, it
// is never used to determine or validate the file's actual format
// (ValidateImage's decode already did that).
var formatExtension = map[string]string{"png": "png", "jpeg": "jpg", "webp": "webp"}

func defaultFileName(format string) string {
	return "untitled." + formatExtension[format]
}

// fileNameFromURL derives a best-effort filename from an upload-from-url
// source URL's own path, falling back to "" (CreateFile's own
// defaultFileName then applies) for anything that does not look like one
// — a query-only URL, a path ending in "/", or an unparseable value.
func fileNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "" || base == "." || base == "/" {
		return ""
	}
	return base
}
