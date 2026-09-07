package httpserver

import (
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// TestHandleNotesCreate_AttachesFilesInRequestOrder is Issue #77 PR6's
// core contract test: docs/compat/aria-v1.5.11.md's PR0 trace found
// fileIds is order-preserving (matching AttachesNotifier.reorder's
// user-visible attachment order), so note.fileIds/note.files must come
// back in exactly the order the request named them, not creation order
// or any other stable-but-different ordering.
func TestHandleNotesCreate_AttachesFilesInRequestOrder(t *testing.T) {
	ts := newNoteAPITestServer(t)
	writeToken, _ := mustIssueToken(t, ts.Server, "attach-order-write", "write:drive")
	first := uploadTestDriveFile(t, ts, writeToken)
	second := uploadTestDriveFile(t, ts, writeToken)

	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "two files", "fileIds": []string{second, first}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got struct {
		CreatedNote struct {
			FileIDs []string         `json:"fileIds"`
			Files   []map[string]any `json:"files"`
		} `json:"createdNote"`
	}
	mustDecode(t, rec, &got)

	if len(got.CreatedNote.FileIDs) != 2 || got.CreatedNote.FileIDs[0] != second || got.CreatedNote.FileIDs[1] != first {
		t.Fatalf("fileIds = %v, want [%s, %s]", got.CreatedNote.FileIDs, second, first)
	}
	if len(got.CreatedNote.Files) != 2 || got.CreatedNote.Files[0]["id"] != second || got.CreatedNote.Files[1]["id"] != first {
		t.Fatalf("files = %v, want ids [%s, %s] in that order", got.CreatedNote.Files, second, first)
	}
	if got.CreatedNote.Files[0]["url"] == nil || got.CreatedNote.Files[0]["url"] == "" {
		t.Errorf("files[0].url is empty, want the absolute /files/{id} URL")
	}
}

// TestHandleNotesCreate_OmittedFileIdsStaysEmpty covers the common case
// PR0's trace found: an attachment-less post omits the fileIds key
// entirely rather than sending fileIds: [] — this service must still
// return the usual explicit empty arrays, not error or omit them itself.
func TestHandleNotesCreate_OmittedFileIdsStaysEmpty(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "no attachment"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got struct {
		CreatedNote struct {
			FileIDs []string `json:"fileIds"`
			Files   []any    `json:"files"`
		} `json:"createdNote"`
	}
	mustDecode(t, rec, &got)
	if len(got.CreatedNote.FileIDs) != 0 || len(got.CreatedNote.Files) != 0 {
		t.Errorf("fileIds/files = %v/%v, want both empty", got.CreatedNote.FileIDs, got.CreatedNote.Files)
	}
}

func TestHandleNotesCreate_RejectsUnknownFileID(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "x", "fileIds": []string{"does-not-exist"}})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_FILE")
}

// TestHandleNotesCreate_RejectsNonAttachmentPurposeFileID covers
// plan-77 v2 §2.3's second validation rule (not just ownership, but
// purpose = 'attachment'): an owner's own avatar_file_id, for instance,
// must not be attachable to a note through this path.
func TestHandleNotesCreate_RejectsNonAttachmentPurposeFileID(t *testing.T) {
	ts := newNoteAPITestServer(t)
	systemFile, err := ts.drive.CreateSystemFile(t.Context(), domain.FilePurposeSourceFavicon, "favicon.png", testPNG(t, 4, 4))
	if err != nil {
		t.Fatalf("CreateSystemFile: %v", err)
	}
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "x", "fileIds": []string{systemFile.ID}})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_FILE")
}

// TestHandleNotesCreate_SameFileAttachableToMultipleNotes corroborates
// PR0's "a single drive file can be attached to more than one note"
// finding all the way through the HTTP layer, not just the repository
// (see internal/storage/sqlite's own
// TestEntryFileRepository_SameFileAttachedToMultipleEntries).
func TestHandleNotesCreate_SameFileAttachableToMultipleNotes(t *testing.T) {
	ts := newNoteAPITestServer(t)
	writeToken, _ := mustIssueToken(t, ts.Server, "reuse-file-write", "write:drive")
	fileID := uploadTestDriveFile(t, ts, writeToken)

	for i := 0; i < 2; i++ {
		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "reuse", "fileIds": []string{fileID}})
		if rec.Code != http.StatusOK {
			t.Fatalf("create %d: status = %d %q, want %d", i, rec.Code, rec.Body.String(), http.StatusOK)
		}
	}
}
