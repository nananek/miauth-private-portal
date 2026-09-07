package domain

import (
	"context"
	"time"
)

// FilePurpose is the files table's closed enum (migration 0022): what a
// stored object is for, not a free-form label. Issue #77 PR3's Misskey-
// compatible Drive API (internal/drive.Service.CreateFile) always
// assigns FilePurposeAttachment to what it uploads — avatar/
// source_favicon/app_icon are each a later PR's own narrower write path
// against this same table, not something a Drive API caller chooses.
type FilePurpose string

const (
	FilePurposeAvatar        FilePurpose = "avatar"
	FilePurposeSourceFavicon FilePurpose = "source_favicon"
	FilePurposeAttachment    FilePurpose = "attachment"
	FilePurposeAppIcon       FilePurpose = "app_icon"
)

// File is one row of the files table: an internal/drive.Storage object's
// metadata, plus (migration 0023, Issue #77 PR3) the Drive-API-specific
// fields no other purpose needs — Name, Comment, IsSensitive, FolderID.
// Width/Height are set together or not at all: every file this service
// currently accepts is a raster image (internal/drive.ValidateImage),
// and Issue #77 v2's "SVG/ベクター画像は許容しない" scope decision applies
// to every Drive upload, not only avatar uploads, so there is no
// non-image File case to design a nil-both convention around yet.
type File struct {
	ID string
	// OwnerActorID is nil for a file with no single owning actor (a
	// future PR4 external-source favicon belongs to a source, not an
	// actor); every file internal/drive.Service.CreateFile creates has
	// one, since Drive API uploads always have a caller.
	OwnerActorID *string
	Purpose      FilePurpose
	MIME         string
	ByteSize     int64
	SHA256       string
	// MD5 is a hex-encoded MD5 digest, stored only for Issue #77 PR3's
	// Drive API wire compatibility: misskey_dart's DriveFile has a
	// required `md5` field (docs/compat/aria-v1.5.11.md's Drive API
	// section) real Misskey historically used for its own client-side
	// dedup, distinct from SHA256's role as this table's own integrity/
	// identity hash. No traced Aria call site reads this value, but the
	// key itself is required on the wire, so internal/drive.Service
	// computes and stores the real algorithm rather than projecting
	// SHA256 under the wrong field name.
	MD5        string
	StorageKey string
	Width      *int
	Height     *int
	// Name is required (misskey_dart's DriveFile.name is a required
	// field, docs/compat/aria-v1.5.11.md's Drive API section) — a
	// caller-supplied name, or internal/drive.Service's own generated
	// fallback when the request omits one.
	Name string
	// Comment and FolderID are both nullable and independently
	// clearable to null after creation (Aria's files/update "move" and
	// "edit comment" actions explicitly send `null` rather than omitting
	// the key — see docs/compat/aria-v1.5.11.md's Drive API section).
	Comment     *string
	IsSensitive bool
	// FolderID is nil for a file at Drive's root. It must name an
	// existing Folder owned by the same OwnerActorID; internal/drive.
	// Service enforces that, not this repository.
	FolderID  *string
	CreatedAt time.Time
}

// FileRepository persists internal/drive.Storage objects' metadata (the
// files table). It knows nothing about Storage itself — internal/drive.
// Service is what keeps a File row and its backing object in sync.
type FileRepository interface {
	Create(ctx context.Context, f File) error
	Get(ctx context.Context, id string) (File, error)
	// ListByOwner returns ownerActorID's files directly inside folderID
	// (nil means Drive's root, not "every folder"), newest-first by
	// (created_at, id) — the same stable cursor shape
	// EntryRepository.ListTimelineDesc uses. before, when non-nil,
	// returns files strictly older than that cursor.
	ListByOwner(ctx context.Context, ownerActorID string, folderID *string, before *Cursor, limit int) ([]File, error)
	// Update overwrites every mutable column (Name, Comment,
	// IsSensitive, FolderID) with f's current values — the partial-
	// update semantics Aria's wire contract requires (a field absent
	// from the request must not change, an explicit null on Comment/
	// FolderID must clear it) are internal/drive.Service's job: it reads
	// the existing File, applies only the fields the request actually
	// named, and calls Update with the merged result. Returns
	// ErrNotFound if f.ID does not exist.
	Update(ctx context.Context, f File) error
	// Delete removes id's row. Deleting an unknown id returns
	// ErrNotFound — unlike internal/drive.Storage.Delete, a caller here
	// always already has the row (an ownership-checked Get precedes
	// every delete in internal/drive.Service), so idempotency is not
	// needed at this layer.
	Delete(ctx context.Context, id string) error
	// SumByteSizeByOwner backs POST /api/drive's usage figure: every
	// file byte ownerActorID owns, across every FolderID and purpose.
	SumByteSizeByOwner(ctx context.Context, ownerActorID string) (int64, error)
	// CountByFolder reports how many files are directly inside
	// folderID (not recursively) — backs DriveFolder.filesCount and
	// folders/delete's "folder must be empty" check.
	CountByFolder(ctx context.Context, folderID string) (int, error)
}

// Folder is one row of the folders table (migration 0023, Issue #77
// PR3): a Drive API concept only — PR0's investigation confirmed Aria's
// drive screen actively uses folder listing, creation, deletion, rename,
// and move, so this is not the "minimal or unsupported" simplification
// plan-77 v2 originally floated before that trace.
type Folder struct {
	ID string
	// OwnerActorID is required, unlike File.OwnerActorID: a folder only
	// ever exists because a Drive API caller created it.
	OwnerActorID string
	Name         string
	// ParentID is nil for a folder at Drive's root.
	ParentID  *string
	CreatedAt time.Time
}

// FolderRepository persists Drive folders. internal/drive.Service is
// responsible for ownership checks, the "cannot move a folder inside its
// own descendant" cycle check, and the "cannot delete a non-empty
// folder" check — none of those are this repository's job.
type FolderRepository interface {
	Create(ctx context.Context, f Folder) error
	Get(ctx context.Context, id string) (Folder, error)
	// ListByOwner returns ownerActorID's folders directly inside
	// parentID (nil means Drive's root), newest-first by
	// (created_at, id), the same stable cursor shape FileRepository.
	// ListByOwner uses.
	ListByOwner(ctx context.Context, ownerActorID string, parentID *string, before *Cursor, limit int) ([]Folder, error)
	// Update overwrites Name and ParentID. Returns ErrNotFound if f.ID
	// does not exist.
	Update(ctx context.Context, f Folder) error
	// Delete removes id's row. Returns ErrNotFound if id does not
	// exist. internal/drive.Service must confirm the folder is empty
	// (CountChildFolders and FileRepository.CountByFolder both zero)
	// before calling this — Delete itself does not check.
	Delete(ctx context.Context, id string) error
	// CountChildFolders reports how many folders are directly inside
	// parentID (not recursively) — backs DriveFolder.foldersCount and
	// the "folder must be empty" delete check.
	CountChildFolders(ctx context.Context, parentID string) (int, error)
}
