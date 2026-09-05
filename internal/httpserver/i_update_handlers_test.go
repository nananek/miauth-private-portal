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
