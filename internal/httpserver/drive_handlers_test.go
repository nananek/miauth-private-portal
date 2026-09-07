package httpserver

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// multipartDriveCreateRequest builds a POST /api/drive/files/create
// request the way misskey_dart's ApiService.postWithFile/postWithBinary
// do (docs/compat/aria-v1.5.11.md's Drive API section): every field as
// its own form part, plus a "file" part, plus "i" as a form field rather
// than the JSON body every other Drive route reads it from.
func multipartDriveCreateRequest(t *testing.T, fields map[string]string, fileData []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if fileData != nil {
		part, err := w.CreateFormFile("file", "upload.png")
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(fileData); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/drive/files/create", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestDrive_FilesCreate_SucceedsAndIsListable(t *testing.T) {
	ts := newDriveTestServer(t)
	req := multipartDriveCreateRequest(t, map[string]string{"i": ts.tokenWrite, "name": "a.png", "comment": "hello"}, testPNG(t, 8, 8))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got map[string]any
	mustDecode(t, rec, &got)
	if got["name"] != "a.png" {
		t.Errorf("name = %v, want a.png", got["name"])
	}
	if got["comment"] != "hello" {
		t.Errorf("comment = %v, want hello", got["comment"])
	}
	if got["isSensitive"] != false {
		t.Errorf("isSensitive = %v, want false", got["isSensitive"])
	}
	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is not an object: %v", got["properties"])
	}
	if props["width"] != float64(8) || props["height"] != float64(8) {
		t.Errorf("properties = %v, want width=8 height=8", props)
	}
	url, _ := got["url"].(string)
	if url == "" {
		t.Fatal("url is empty")
	}

	// The file must now be servable at its own url and listable.
	getReq := httptest.NewRequest(http.MethodGet, url[len(testLocalOrigin):], nil)
	getRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Errorf("GET %s = %d, want %d", url, getRec.Code, http.StatusOK)
	}
	if ct := getRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}

	listRec := ts.post(t, "/api/drive/files", ts.tokenRead, nil)
	var files []map[string]any
	mustDecode(t, listRec, &files)
	if len(files) != 1 {
		t.Fatalf("listed %d files, want 1", len(files))
	}
}

func TestDrive_FilesCreate_MissingTokenIsAuthenticationFailed(t *testing.T) {
	ts := newDriveTestServer(t)
	req := multipartDriveCreateRequest(t, map[string]string{}, testPNG(t, 4, 4))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestDrive_FilesCreate_WrongScopeTokenIsAuthenticationFailed(t *testing.T) {
	ts := newDriveTestServer(t)
	// tokenRead only carries read:drive; files/create requires write:drive.
	req := multipartDriveCreateRequest(t, map[string]string{"i": ts.tokenRead}, testPNG(t, 4, 4))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestDrive_FilesCreate_RejectsSVG(t *testing.T) {
	ts := newDriveTestServer(t)
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`)
	req := multipartDriveCreateRequest(t, map[string]string{"i": ts.tokenWrite}, svg)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
}

func TestDrive_FilesCreate_RejectsMissingFile(t *testing.T) {
	ts := newDriveTestServer(t)
	req := multipartDriveCreateRequest(t, map[string]string{"i": ts.tokenWrite}, nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestDrive_FilesCreate_RejectsOversizedFile(t *testing.T) {
	ts := newDriveTestServer(t)
	ts.driveMaxFileBytes = 10 // shrink the handler's own bound directly
	req := multipartDriveCreateRequest(t, map[string]string{"i": ts.tokenWrite}, testPNG(t, 50, 50))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
}

func TestDrive_Stats(t *testing.T) {
	ts := newDriveTestServer(t)
	rec := ts.post(t, "/api/drive", ts.tokenRead, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got driveResponse
	mustDecode(t, rec, &got)
	if got.Capacity != 1<<30 {
		t.Errorf("capacity = %d, want %d", got.Capacity, int64(1)<<30)
	}
}

func createTestDriveFile(t *testing.T, ts *driveTestServer, name string) string {
	t.Helper()
	req := multipartDriveCreateRequest(t, map[string]string{"i": ts.tokenWrite, "name": name}, testPNG(t, 4, 4))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create file %q: status = %d %q", name, rec.Code, rec.Body.String())
	}
	var got struct {
		ID string `json:"id"`
	}
	mustDecode(t, rec, &got)
	return got.ID
}

func TestDrive_FilesShow_NotFound(t *testing.T) {
	ts := newDriveTestServer(t)
	rec := ts.post(t, "/api/drive/files/show", ts.tokenRead, map[string]any{"fileId": "does-not-exist"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_FILE")
}

func TestDrive_FilesUpdate_PartialUpdateOverWire(t *testing.T) {
	ts := newDriveTestServer(t)
	fileID := createTestDriveFile(t, ts, "orig.png")

	rec := ts.post(t, "/api/drive/files/update", ts.tokenWrite, map[string]any{"fileId": fileID, "name": "renamed.png"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got map[string]any
	mustDecode(t, rec, &got)
	if got["name"] != "renamed.png" {
		t.Errorf("name = %v, want renamed.png", got["name"])
	}

	// An explicit-null comment (raw JSON, not encoding/json's omitted-
	// field marshaling) must clear it.
	rec = ts.postRawJSON(t, "/api/drive/files/update", `{"i":"`+ts.tokenWrite+`","fileId":"`+fileID+`","comment":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	mustDecode(t, rec, &got)
	if got["comment"] != nil {
		t.Errorf("comment = %v, want nil after explicit-null update", got["comment"])
	}
	if got["name"] != "renamed.png" {
		t.Errorf("name = %v, want renamed.png to survive the comment-only update", got["name"])
	}
}

// postRawJSON is like (*driveTestServer).post but sends body verbatim,
// for tests that need to send a literal JSON null the map[string]any +
// encoding/json path (which omits a nil interface{} value as an absent
// key, not JSON null) cannot express.
func (ts *driveTestServer) postRawJSON(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	return rec
}

func TestDrive_FilesDelete(t *testing.T) {
	ts := newDriveTestServer(t)
	fileID := createTestDriveFile(t, ts, "todelete.png")

	rec := ts.post(t, "/api/drive/files/delete", ts.tokenWrite, map[string]any{"fileId": fileID})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusNoContent)
	}

	showRec := ts.post(t, "/api/drive/files/show", ts.tokenRead, map[string]any{"fileId": fileID})
	assertWireError(t, showRec, http.StatusBadRequest, "NO_SUCH_FILE")
}

// There is no HTTP-layer equivalent of "a different owner's token" to
// test against: this is a single-owner deployment where every MiAuth-
// issued token authenticates the same singleton owner actor
// (domain.ActorOwner's unique-index constraint) — mustIssueToken can
// only ever mint another token for that same actor, never a genuinely
// different one. The ownership check itself (getOwnedFile/getOwnedFolder)
// is covered where a genuinely distinct actor is actually constructible:
// internal/drive/service_test.go's TestService_DeleteFile_NotOwnedIsNotFoundAndLeavesFileIntact
// and its FileUpdate/Folder counterparts, built directly against
// repositories rather than through MiAuth.

func TestDrive_FilesAttachedNotes_AlwaysEmpty(t *testing.T) {
	ts := newDriveTestServer(t)
	fileID := createTestDriveFile(t, ts, "a.png")
	rec := ts.post(t, "/api/drive/files/attached-notes", ts.tokenRead, map[string]any{"fileId": fileID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var notes []map[string]any
	mustDecode(t, rec, &notes)
	if len(notes) != 0 {
		t.Errorf("attached-notes = %v, want an empty array (Issue #77 PR6 not implemented yet)", notes)
	}
}

func TestDrive_UploadFromUrl(t *testing.T) {
	ts := newDriveTestServer(t)
	png := testPNG(t, 5, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(png)
	}))
	defer upstream.Close()

	rec := ts.post(t, "/api/drive/files/upload-from-url", ts.tokenWrite, map[string]any{"url": upstream.URL + "/x.png"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusNoContent)
	}

	listRec := ts.post(t, "/api/drive/files", ts.tokenRead, nil)
	var files []map[string]any
	mustDecode(t, listRec, &files)
	if len(files) != 1 || files[0]["name"] != "x.png" {
		t.Fatalf("files after upload-from-url = %v, want one file named x.png", files)
	}
}

func TestDrive_Folders_CreateShowUpdateDelete(t *testing.T) {
	ts := newDriveTestServer(t)

	createRec := ts.post(t, "/api/drive/folders/create", ts.tokenWrite, map[string]any{"name": "Photos"})
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status = %d %q, want %d", createRec.Code, createRec.Body.String(), http.StatusOK)
	}
	var folder map[string]any
	mustDecode(t, createRec, &folder)
	folderID, _ := folder["id"].(string)
	if folderID == "" || folder["name"] != "Photos" {
		t.Fatalf("created folder = %v", folder)
	}

	showRec := ts.post(t, "/api/drive/folders/show", ts.tokenRead, map[string]any{"folderId": folderID})
	var shown map[string]any
	mustDecode(t, showRec, &shown)
	if shown["foldersCount"] != float64(0) || shown["filesCount"] != float64(0) {
		t.Errorf("show counts = %v, want 0/0", shown)
	}

	updateRec := ts.post(t, "/api/drive/folders/update", ts.tokenWrite, map[string]any{"folderId": folderID, "name": "Renamed"})
	var updated map[string]any
	mustDecode(t, updateRec, &updated)
	if updated["name"] != "Renamed" {
		t.Errorf("name after update = %v, want Renamed", updated["name"])
	}

	deleteRec := ts.post(t, "/api/drive/folders/delete", ts.tokenWrite, map[string]any{"folderId": folderID})
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d %q, want %d", deleteRec.Code, deleteRec.Body.String(), http.StatusNoContent)
	}
	showAgainRec := ts.post(t, "/api/drive/folders/show", ts.tokenRead, map[string]any{"folderId": folderID})
	assertWireError(t, showAgainRec, http.StatusBadRequest, "NO_SUCH_FOLDER")
}

func TestDrive_FoldersDelete_NonEmptyIsRejected(t *testing.T) {
	ts := newDriveTestServer(t)
	createRec := ts.post(t, "/api/drive/folders/create", ts.tokenWrite, map[string]any{"name": "Photos"})
	var folder map[string]any
	mustDecode(t, createRec, &folder)
	folderID, _ := folder["id"].(string)

	req := multipartDriveCreateRequest(t, map[string]string{"i": ts.tokenWrite, "folderId": folderID}, testPNG(t, 4, 4))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create file in folder: status = %d %q", rec.Code, rec.Body.String())
	}

	deleteRec := ts.post(t, "/api/drive/folders/delete", ts.tokenWrite, map[string]any{"folderId": folderID})
	assertWireError(t, deleteRec, http.StatusBadRequest, "FOLDER_NOT_EMPTY")
}

func TestFilesShow_UnknownIDIs404(t *testing.T) {
	ts := newDriveTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/files/does-not-exist", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDrive_EndpointsListsDriveButNotMoveBulkOrUnusedOnes(t *testing.T) {
	ts := newDriveTestServer(t)
	rec := httptest.NewRequest(http.MethodPost, "/api/endpoints", nil)
	res := httptest.NewRecorder()
	ts.Handler().ServeHTTP(res, rec)
	var got []string
	mustDecode(t, res, &got)

	want := map[string]bool{
		"drive": false, "drive/files": false, "drive/files/create": false, "drive/files/show": false,
		"drive/files/update": false, "drive/files/delete": false, "drive/files/upload-from-url": false,
		"drive/files/attached-notes": false, "drive/folders": false, "drive/folders/create": false,
		"drive/folders/show": false, "drive/folders/update": false, "drive/folders/delete": false,
	}
	forbidden := map[string]bool{
		"drive/files/move-bulk": true, "drive/stream": true, "drive/files/find": true,
		"drive/files/check-existence": true, "drive/files/find-by-hash": true, "drive/folders/find": true,
	}
	for _, name := range got {
		if _, ok := want[name]; ok {
			want[name] = true
		}
		if forbidden[name] {
			t.Errorf("/api/endpoints advertises %q, which must never be implemented", name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("/api/endpoints is missing %q", name)
		}
	}
}
