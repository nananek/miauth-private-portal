package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// iUpdateTestServer issues a dedicated write:account-scoped token: the
// shared noteAPITestServer.token (read:account,write:notes) deliberately
// lacks write:account, so POST /api/i/update tests need their own.
func newIUpdateTestServer(t *testing.T) (*noteAPITestServer, string) {
	t.Helper()
	ts := newNoteAPITestServer(t)
	token, ownerID := mustIssueToken(t, ts.Server, "i-update-setup", "write:account")
	if ownerID != ts.ownerID {
		t.Fatalf("write:account token owner = %q, want %q (single-owner invariant)", ownerID, ts.ownerID)
	}
	return ts, token
}

func TestHandleAPIIUpdate_UpdatesDisplayName(t *testing.T) {
	ts, token := newIUpdateTestServer(t)

	rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "name": "New Display Name"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var resp meDetailed
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if resp.ID != ts.ownerID {
		t.Errorf("id = %q, want %q", resp.ID, ts.ownerID)
	}
	if resp.Name == nil || *resp.Name != "New Display Name" {
		t.Errorf("name = %v, want %q", resp.Name, "New Display Name")
	}
	if resp.Username != "owner" {
		t.Errorf("username = %q, want unchanged %q", resp.Username, "owner")
	}

	// POST /api/i (a separate read:account-scoped request) must reflect
	// the same persisted value, not just the update response itself.
	getRec := ts.post(t, "/api/i", nil)
	var getResp meDetailed
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode /api/i: %v", err)
	}
	if getResp.Name == nil || *getResp.Name != "New Display Name" {
		t.Errorf("/api/i name = %v, want %q", getResp.Name, "New Display Name")
	}
}

// TestHandleAPIIUpdate_ReflectedInCheckResponse backs Issue #23 PR1's
// acceptance criterion that a fresh POST /api/miauth/{session}/check
// response reflects the updated display name too, not only /api/i.
// mustIssueToken already drives this same start/approve/check flow but
// only returns the token and user ID, so this test repeats it inline to
// inspect the decoded user.name field.
func TestHandleAPIIUpdate_ReflectedInCheckResponse(t *testing.T) {
	ts, token := newIUpdateTestServer(t)
	if rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "name": "Checked Name"}); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}

	const routeSessionID = "post-update-check"
	startReq := httptest.NewRequest(http.MethodGet, "/miauth/"+routeSessionID, nil)
	startRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("GET /miauth/%s = %d %q", routeSessionID, startRec.Code, startRec.Body.String())
	}
	if err := ts.miauth.ApproveSession(t.Context(), routeSessionID); err != nil {
		t.Fatalf("approve session: %v", err)
	}
	checkRec := ts.postRaw(t, "/api/miauth/"+routeSessionID+"/check", "{}")
	if checkRec.Code != http.StatusOK {
		t.Fatalf("check = %d %q", checkRec.Code, checkRec.Body.String())
	}
	var resp checkSuccessResponse
	if err := json.Unmarshal(checkRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode check response: %v; body=%s", err, checkRec.Body.String())
	}
	if !resp.OK {
		t.Fatalf("check did not succeed: %+v", resp)
	}
	if resp.User.Name == nil || *resp.User.Name != "Checked Name" {
		t.Errorf("check user.name = %v, want %q", resp.User.Name, "Checked Name")
	}
}

func TestHandleAPIIUpdate_RejectsUnsupportedField(t *testing.T) {
	ts, token := newIUpdateTestServer(t)
	rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "name": "x", "username": "new-handle"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
	var resp wireErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if resp.Error.Code != "UNSUPPORTED_FEATURE" {
		t.Errorf("code = %q, want UNSUPPORTED_FEATURE", resp.Error.Code)
	}
	if resp.Error.Info["field"] != "username" {
		t.Errorf("info.field = %v, want %q", resp.Error.Info["field"], "username")
	}
}

func TestHandleAPIIUpdate_RequiresName(t *testing.T) {
	ts, token := newIUpdateTestServer(t)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"absent", map[string]any{"i": token}},
		{"null", map[string]any{"i": token, "name": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := ts.post(t, "/api/i/update", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
			}
			var resp wireErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error.Code != "INVALID_PARAM" {
				t.Errorf("code = %q, want INVALID_PARAM", resp.Error.Code)
			}
		})
	}
}

func TestHandleAPIIUpdate_RejectsNonStringName(t *testing.T) {
	ts, token := newIUpdateTestServer(t)
	rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "name": 123})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
}

// uploadTestDriveFile uploads a small valid PNG through POST
// /api/drive/files/create using writeToken, returning the created
// file's id — the same real upload path Aria's INotifier.setAvatarId
// flow uses before ever calling POST /api/i/update (PR0's trace:
// drive.files.create/createAsBinary, then i/update {avatarId}).
func uploadTestDriveFile(t *testing.T, ts *noteAPITestServer, writeToken string) string {
	t.Helper()
	req := multipartDriveCreateRequest(t, map[string]string{"i": writeToken, "name": "avatar.png"}, testPNG(t, 4, 4))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload drive file: status = %d %q", rec.Code, rec.Body.String())
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return got.ID
}

// TestHandleAPIIUpdate_SetsAvatarIdAlone is Issue #77 PR5's core
// regression test: PR0's trace found Aria's avatar-only flow sends
// `{"avatarId": ...}` with no "name" key at all, which the pre-PR5
// "name is always required" shape would have wrongly rejected.
func TestHandleAPIIUpdate_SetsAvatarIdAlone(t *testing.T) {
	ts, token := newIUpdateTestServer(t)
	writeToken, _ := mustIssueToken(t, ts.Server, "avatar-upload", "write:drive")
	fileID := uploadTestDriveFile(t, ts, writeToken)

	rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "avatarId": fileID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantURL := testLocalOrigin + "/files/" + fileID
	if resp["avatarUrl"] != wantURL {
		t.Errorf("avatarUrl = %v, want %q", resp["avatarUrl"], wantURL)
	}

	// The avatar must also appear on a freshly authored note's user
	// projection (resolveUserLite's owner branch), and persist across a
	// separate POST /api/i read. ts.token (not the write:account-only
	// token above) is what carries write:notes.
	createRec := ts.post(t, "/api/notes/create", map[string]any{"i": ts.token, "text": "hello"})
	if createRec.Code != http.StatusOK {
		t.Fatalf("notes/create: %d %s", createRec.Code, createRec.Body.String())
	}
	var createResp struct {
		CreatedNote map[string]any `json:"createdNote"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("decode note: %v", err)
	}
	user, ok := createResp.CreatedNote["user"].(map[string]any)
	if !ok {
		t.Fatalf("user is not an object: %v", createResp.CreatedNote["user"])
	}
	if user["avatarUrl"] != wantURL {
		t.Errorf("note.user.avatarUrl = %v, want %q", user["avatarUrl"], wantURL)
	}

	// token only carries write:account (newIUpdateTestServer), not the
	// read:account POST /api/i itself requires — ts.token does.
	getRec := ts.post(t, "/api/i", map[string]any{"i": ts.token})
	if getRec.Code != http.StatusOK {
		t.Fatalf("/api/i status = %d %q, want %d", getRec.Code, getRec.Body.String(), http.StatusOK)
	}
	var getResp map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode /api/i: %v", err)
	}
	if getResp["avatarUrl"] != wantURL {
		t.Errorf("/api/i avatarUrl = %v, want %q", getResp["avatarUrl"], wantURL)
	}
}

// TestHandleAPIIUpdate_ClearsAvatarIdWithExplicitNull backs PR0's trace:
// Aria sends an explicit `avatarId: null` (not an omitted key) to remove
// the avatar, opting out of its usual null-omission convention
// specifically for this field.
func TestHandleAPIIUpdate_ClearsAvatarIdWithExplicitNull(t *testing.T) {
	ts, token := newIUpdateTestServer(t)
	writeToken, _ := mustIssueToken(t, ts.Server, "avatar-upload-clear", "write:drive")
	fileID := uploadTestDriveFile(t, ts, writeToken)

	if rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "avatarId": fileID}); rec.Code != http.StatusOK {
		t.Fatalf("initial set: %d %s", rec.Code, rec.Body.String())
	}

	rec := ts.postRaw(t, "/api/i/update", `{"i":"`+token+`","avatarId":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["avatarUrl"] != nil {
		t.Errorf("avatarUrl after explicit-null clear = %v, want nil", resp["avatarUrl"])
	}
}

func TestHandleAPIIUpdate_RejectsAvatarIdNotOwned(t *testing.T) {
	ts, token := newIUpdateTestServer(t)
	rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "avatarId": "does-not-exist"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_FILE")
}

func TestHandleAPIIUpdate_ClearsDisplayNameWithEmptyString(t *testing.T) {
	ts, token := newIUpdateTestServer(t)
	if rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "name": "Something"}); rec.Code != http.StatusOK {
		t.Fatalf("initial set: %d %s", rec.Code, rec.Body.String())
	}

	rec := ts.post(t, "/api/i/update", map[string]any{"i": token, "name": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	var resp meDetailed
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != nil {
		t.Errorf("name after clearing = %v, want nil (matching newUserDetailedNotMe's empty-means-null convention)", *resp.Name)
	}
}
