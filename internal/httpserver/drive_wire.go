package httpserver

import (
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// driveFileProperties projects domain.File.Width/Height into
// misskey_dart's DriveFileProperties (docs/compat/aria-v1.5.11.md's
// Drive API section): itself always present on the wire, but every
// field inside it is nullable. Width/Height are always both non-nil in
// practice — every file this service accepts is a raster image (Issue
// #77 v2's raster-only scope decision, internal/drive.ValidateImage).
// Orientation and AvgColor are always null: neither is derived from
// anything ValidateImage or Storage compute, and no traced Aria call
// site reads them.
type driveFileProperties struct {
	Width       *int    `json:"width"`
	Height      *int    `json:"height"`
	Orientation *int    `json:"orientation"`
	AvgColor    *string `json:"avgColor"`
}

// driveFile is the Misskey-compatible projection of one domain.File
// (docs/compat/aria-v1.5.11.md's Drive API section's traced
// misskey_dart DriveFile). Required: id, createdAt, name, type, md5,
// size, isSensitive, properties, url. Nullable, and always null here:
// Blurhash and ThumbnailURL (this service generates neither — PR0's
// trace confirms Aria's media widgets fall back to url when
// thumbnailUrl is null) and Folder (Aria never reads it directly off a
// DriveFile; FolderID alone is enough for its own provider state, and
// nesting it would cost an extra query this service does not otherwise
// need). User is always null for the same reason Folder is; UserID
// itself is populated since it is already in hand.
type driveFile struct {
	ID           string              `json:"id"`
	CreatedAt    string              `json:"createdAt"`
	Name         string              `json:"name"`
	Type         string              `json:"type"`
	MD5          string              `json:"md5"`
	Size         int64               `json:"size"`
	IsSensitive  bool                `json:"isSensitive"`
	Blurhash     *string             `json:"blurhash"`
	Properties   driveFileProperties `json:"properties"`
	URL          string              `json:"url"`
	ThumbnailURL *string             `json:"thumbnailUrl"`
	Comment      *string             `json:"comment"`
	FolderID     *string             `json:"folderId"`
	Folder       *driveFolder        `json:"folder"`
	UserID       *string             `json:"userId"`
	User         *userLite           `json:"user"`
}

// projectDriveFile projects f, using s.localOrigin to build an absolute
// GET /files/{id} URL — PR0's trace found Aria's image cache manager
// consumes DriveFile.url as a plain HTTP(S) URL with an ordinary GET, no
// special headers, which is exactly what that anonymous route serves
// (drive_handlers.go's handleFilesShow).
func (s *Server) projectDriveFile(f domain.File) driveFile {
	return driveFile{
		ID:          f.ID,
		CreatedAt:   f.CreatedAt.UTC().Format(time.RFC3339),
		Name:        f.Name,
		Type:        f.MIME,
		MD5:         f.MD5,
		Size:        f.ByteSize,
		IsSensitive: f.IsSensitive,
		Properties: driveFileProperties{
			Width:  f.Width,
			Height: f.Height,
		},
		URL:      s.localOrigin + "/files/" + f.ID,
		Comment:  f.Comment,
		FolderID: f.FolderID,
		UserID:   f.OwnerActorID,
	}
}

// driveFolder is the Misskey-compatible projection of one domain.Folder
// (misskey_dart's DriveFolder: id/createdAt/name required; parentId/
// parent/foldersCount/filesCount nullable). Parent is always null —
// like driveFile.Folder, no traced Aria call site reads it, and nesting
// it recursively would have no natural depth bound. FoldersCount/
// FilesCount are populated only by projectDriveFolderWithCounts
// (drive/folders/show, a single-folder detail view where two more COUNT
// queries are cheap); every list/create/update projection leaves them
// null rather than paying an extra query per row.
type driveFolder struct {
	ID           string       `json:"id"`
	CreatedAt    string       `json:"createdAt"`
	Name         string       `json:"name"`
	ParentID     *string      `json:"parentId"`
	Parent       *driveFolder `json:"parent"`
	FoldersCount *int         `json:"foldersCount"`
	FilesCount   *int         `json:"filesCount"`
}

func projectDriveFolder(f domain.Folder) driveFolder {
	return driveFolder{
		ID:        f.ID,
		CreatedAt: f.CreatedAt.UTC().Format(time.RFC3339),
		Name:      f.Name,
		ParentID:  f.ParentID,
	}
}

func projectDriveFolderWithCounts(f domain.Folder, foldersCount, filesCount int) driveFolder {
	d := projectDriveFolder(f)
	d.FoldersCount = &foldersCount
	d.FilesCount = &filesCount
	return d
}

// driveResponse is POST /api/drive's body (misskey_dart's DriveResponse:
// capacity/usage both required ints). See internal/drive.Config.
// CapacityBytes's doc comment for why Capacity is cosmetic only.
type driveResponse struct {
	Capacity int64 `json:"capacity"`
	Usage    int64 `json:"usage"`
}

// driveFilesRequest mirrors misskey_dart's DriveFilesRequest. Only
// FolderID/UntilID/Limit ever have an effect: PR0's trace found Aria's
// only call site (DriveFilesNotifier._fetchFiles) never sets SinceID,
// SinceDate, UntilDate, Type, or Sort, so this service accepts and
// decodes them (rather than rejecting an otherwise-valid request over an
// unused field) without guessing at filter/sort semantics no observed
// caller exercises.
type driveFilesRequest struct {
	Limit    *int    `json:"limit"`
	SinceID  *string `json:"sinceId"`
	UntilID  *string `json:"untilId"`
	FolderID *string `json:"folderId"`
	Type     *string `json:"type"`
	Sort     *string `json:"sort"`
}

// driveFilesShowRequest mirrors misskey_dart's DriveFilesShowRequest
// ("どちらか必須" — either FileID or URL). Only FileID is implemented:
// no traced Aria call site (DriveFileNotifier.build) ever sends URL
// instead, so a URL-only request is rejected explicitly (writeInvalidParam)
// rather than this service guessing at a lookup-by-URL implementation no
// observed caller exercises.
type driveFilesShowRequest struct {
	FileID *string `json:"fileId"`
	URL    *string `json:"url"`
}

type driveFilesDeleteRequest struct {
	FileID string `json:"fileId"`
}

// driveFilesUploadFromUrlRequest mirrors misskey_dart's
// DriveFilesUploadFromUrlRequest. Marker is real Misskey's cursor for a
// since-removed batch-upload feature no traced Aria call site sets; it
// is not even decoded here.
type driveFilesUploadFromUrlRequest struct {
	URL         string  `json:"url"`
	FolderID    *string `json:"folderId"`
	IsSensitive *bool   `json:"isSensitive"`
	Comment     *string `json:"comment"`
}

// driveFilesAttachedNotesRequest mirrors misskey_dart's
// DriveFilesAttachedNotesRequest. Limit/SinceID/UntilID/SinceDate/
// UntilDate are decoded but unused: no traced Aria call site
// (DriveFileNotifier's attached-notes view) ever sets them, and Issue
// #77 PR6's handleDriveFilesAttachedNotes returns every attached note in
// a single unpaginated page rather than guessing at filter/sort
// semantics no observed caller exercises.
type driveFilesAttachedNotesRequest struct {
	FileID string `json:"fileId"`
}

type driveFoldersRequest struct {
	Limit    *int    `json:"limit"`
	UntilID  *string `json:"untilId"`
	FolderID *string `json:"folderId"`
}

type driveFoldersCreateRequest struct {
	Name     *string `json:"name"`
	ParentID *string `json:"parentId"`
}

type driveFoldersDeleteRequest struct {
	FolderID string `json:"folderId"`
}

type driveFoldersShowRequest struct {
	FolderID string `json:"folderId"`
}
