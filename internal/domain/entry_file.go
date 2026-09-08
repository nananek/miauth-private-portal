package domain

import "context"

// EntryFile is one row of Issue #77 PR6's entry_files table: a many-to-
// many join between an entry and the Drive files attached to it, in
// Aria's own compose-order Position (docs/compat/aria-v1.5.11.md's PR0
// trace: notes/create's fileIds is order-preserving, matching
// AttachesNotifier.reorder's user-visible attachment order — this
// service must preserve it as entry_files.Position). PR0 also corrected
// plan-77 v2's tentative "1:1 might be enough" framing: a single files
// row can be attached to more than one entry (AttachedNotesNotifier
// exists specifically to answer "which notes is this file attached to"
// as a one-to-many query), so this must be a genuine many-to-many join,
// not a 1:1 pointer on either table.
type EntryFile struct {
	EntryID  string
	FileID   string
	Position int
}

// EntryFileRepository persists which files are attached to which entry.
// notes/update is not implemented and no traced Aria call site ever
// edits an existing note's attachments (docs/compat/aria-v1.5.11.md), so
// attachments are set at most once per entry, at creation time — there
// is no Update, only Create.
type EntryFileRepository interface {
	// Create links entryID to each of fileIDs, in order: fileIDs[0] at
	// Position 0, fileIDs[1] at Position 1, and so on. Called at most
	// once per entry, inside the same transaction that creates entryID's
	// own entries row (internal/timeline.Service's EntryHook mechanism).
	Create(ctx context.Context, entryID string, fileIDs []string) error
	// ListFilesByEntry returns entryID's attached files, in attachment
	// order (Position ascending) — backing note.fileIds/note.files. An
	// entry with no attachments returns an empty, non-nil slice.
	ListFilesByEntry(ctx context.Context, entryID string) ([]File, error)
	// CountByFile reports how many entries reference fileID.
	// internal/drive.Service.DeleteFile checks this before deleting a
	// files row — the same "cannot remove something still referenced"
	// rule FolderRepository.CountByFolder/CountChildFolders enforce
	// before a folder delete.
	CountByFile(ctx context.Context, fileID string) (int, error)
	// ListEntriesByFile returns, in no particular guaranteed order, the
	// entries fileID is attached to — backing POST
	// /api/drive/files/attached-notes (docs/compat/aria-v1.5.11.md's PR0
	// trace: this is a genuine one-to-many query, not the always-[]
	// placeholder PR3 shipped before this table existed).
	ListEntriesByFile(ctx context.Context, fileID string) ([]Entry, error)
}
