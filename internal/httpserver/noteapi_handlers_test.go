package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleMeta_AnonymousReturnsLocalOriginOnly(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.postRaw(t, "/api/meta", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	var resp metaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if resp.URI != testLocalOrigin {
		t.Errorf("uri = %q, want %q", resp.URI, testLocalOrigin)
	}
	if !resp.Features.MiAuth {
		t.Error("features.miauth = false, want true")
	}
}

func TestHandleEndpoints_ListsOnlyImplementedNeverUpdate(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.postRaw(t, "/api/endpoints", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	var got []string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{
		"meta": true, "endpoints": true, "i": true,
		"notes/create": true, "notes/timeline": true, "notes/show": true,
		"notes/conversation": true, "notes/children": true,
	}
	if len(got) != len(want) {
		t.Errorf("endpoints = %v, want exactly %v", got, want)
	}
	for _, e := range got {
		if !want[e] {
			t.Errorf("unexpected advertised endpoint %q", e)
		}
		if e == "notes/update" {
			t.Error("notes/update must never be advertised (not implemented)")
		}
	}
}

func TestHandleAPII_AlwaysReturnsMeDetailedShape(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/i", nil)
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
	if resp.Username != "owner" {
		t.Errorf("username = %q, want owner", resp.Username)
	}
	if resp.CreatedAt == "" {
		t.Error("createdAt is empty")
	}
	if !resp.IsModerator || !resp.IsAdmin {
		t.Errorf("isModerator/isAdmin = %v/%v, want true/true", resp.IsModerator, resp.IsAdmin)
	}

	// The token-login fallback decodes only {id, username}; confirm both
	// are present as plain non-empty strings in the raw JSON too.
	var minimal struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minimal); err != nil {
		t.Fatalf("decode as fallback shape: %v", err)
	}
	if minimal.ID == "" || minimal.Username == "" {
		t.Errorf("fallback-shape decode incomplete: %+v", minimal)
	}
}

func TestHandleAPII_NotesCountReflectsCreatedNotes(t *testing.T) {
	ts := newNoteAPITestServer(t)

	if rec := ts.post(t, "/api/notes/create", map[string]any{"text": "one"}); rec.Code != http.StatusOK {
		t.Fatalf("create note 1: %d %s", rec.Code, rec.Body.String())
	}
	ts.clock.Advance(time.Minute)
	if rec := ts.post(t, "/api/notes/create", map[string]any{"text": "two"}); rec.Code != http.StatusOK {
		t.Fatalf("create note 2: %d %s", rec.Code, rec.Body.String())
	}

	rec := ts.post(t, "/api/i", nil)
	var resp meDetailed
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NotesCount != 2 {
		t.Errorf("notesCount = %d, want 2", resp.NotesCount)
	}
}

// protectedEndpoints lists every note route requiring a valid token, with
// a minimal valid body, for the shared missing-token/wrong-scope table
// tests below.
var protectedEndpoints = []struct {
	path string
	body map[string]any
}{
	{"/api/i", map[string]any{}},
	{"/api/notes/create", map[string]any{"text": "hello"}},
	{"/api/notes/timeline", map[string]any{}},
	{"/api/notes/show", map[string]any{"noteId": "does-not-exist"}},
	{"/api/notes/conversation", map[string]any{"noteId": "does-not-exist"}},
	{"/api/notes/children", map[string]any{"noteId": "does-not-exist"}},
}

func TestProtectedEndpoints_MissingTokenIsAuthenticationFailed(t *testing.T) {
	ts := newNoteAPITestServer(t)
	for _, ep := range protectedEndpoints {
		t.Run(ep.path, func(t *testing.T) {
			body := map[string]any{}
			for k, v := range ep.body {
				body[k] = v
			}
			body["i"] = "" // present but empty, and never auto-filled by post()
			rec := ts.post(t, ep.path, body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusUnauthorized)
			}
		})
	}
}

func TestProtectedEndpoints_WrongScopeIsAuthenticationFailed(t *testing.T) {
	ts := newNoteAPITestServer(t)
	// read:notes is always granted (internal/miauth/scope.go), so a token
	// that requests nothing beyond it never carries write:notes or
	// read:account, letting this table exercise both.
	limitedToken, _ := mustIssueToken(t, ts.Server, "limited-scope-session", "")

	cases := []struct {
		path string
		body map[string]any
	}{
		{"/api/i", map[string]any{}},                           // needs read:account
		{"/api/notes/create", map[string]any{"text": "hello"}}, // needs write:notes
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			body := map[string]any{"i": limitedToken}
			for k, v := range c.body {
				body[k] = v
			}
			rec := ts.post(t, c.path, body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusUnauthorized)
			}
		})
	}
}

func TestHandleNotesCreate_RootAndReply(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "root text"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create root: %d %s", rec.Code, rec.Body.String())
	}
	var rootResp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rootResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rootResp.CreatedNote.ID == "" {
		t.Fatal("createdNote.id is empty")
	}
	if rootResp.CreatedNote.Text == nil || *rootResp.CreatedNote.Text != "root text" {
		t.Errorf("createdNote.text = %v, want %q", rootResp.CreatedNote.Text, "root text")
	}
	if rootResp.CreatedNote.User.Username != "owner" {
		t.Errorf("createdNote.user.username = %q, want owner", rootResp.CreatedNote.User.Username)
	}
	if rootResp.CreatedNote.ReplyID != nil {
		t.Errorf("createdNote.replyId = %v, want nil for a root note", rootResp.CreatedNote.ReplyID)
	}

	ts.clock.Advance(time.Minute)
	replyRec := ts.post(t, "/api/notes/create", map[string]any{"text": "reply text", "replyId": rootResp.CreatedNote.ID})
	if replyRec.Code != http.StatusOK {
		t.Fatalf("create reply: %d %s", replyRec.Code, replyRec.Body.String())
	}
	var replyResp createdNoteResponse
	if err := json.Unmarshal(replyRec.Body.Bytes(), &replyResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if replyResp.CreatedNote.ReplyID == nil || *replyResp.CreatedNote.ReplyID != rootResp.CreatedNote.ID {
		t.Errorf("createdNote.replyId = %v, want %q", replyResp.CreatedNote.ReplyID, rootResp.CreatedNote.ID)
	}
}

func TestHandleNotesCreate_EmptyTextIsInvalidParam(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": ""})
	assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
}

func TestHandleNotesCreate_UnknownReplyParentIsNoSuchNote(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "reply", "replyId": "does-not-exist"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
}

// TestHandleNotesCreate_ReplyToHiddenOrArchivedParentIsNoSuchNote pins the
// same uniform NO_SUCH_NOTE treatment notes/show, conversation, and
// children give a hidden/archived note ID: replying to one must not
// silently succeed just because timeline.CreateReply's own parent lookup
// does not filter by visibility.
func TestHandleNotesCreate_ReplyToHiddenOrArchivedParentIsNoSuchNote(t *testing.T) {
	ts := newNoteAPITestServer(t)

	t.Run("hidden", func(t *testing.T) {
		createRec := ts.post(t, "/api/notes/create", map[string]any{"text": "parent"})
		var created createdNoteResponse
		mustDecode(t, createRec, &created)
		if err := ts.timeline.SetHidden(t.Context(), created.CreatedNote.ID, true); err != nil {
			t.Fatal(err)
		}

		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "reply", "replyId": created.CreatedNote.ID})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})

	t.Run("archived", func(t *testing.T) {
		createRec := ts.post(t, "/api/notes/create", map[string]any{"text": "parent"})
		var created createdNoteResponse
		mustDecode(t, createRec, &created)
		if err := ts.timeline.SetArchived(t.Context(), created.CreatedNote.ID, true); err != nil {
			t.Fatal(err)
		}

		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "reply", "replyId": created.CreatedNote.ID})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})
}

// TestHandleNotesCreate_MalformedJSONIsInvalidParam exercises the
// handler's own decode failure (a wrong-typed field), not a syntactically
// broken body: RequireScope must itself fully parse the JSON body to
// extract "i" before any handler runs (internal/httpserver/scope_middleware.go),
// so a genuinely malformed body never reaches the handler at all — it
// surfaces as RequireScope's uniform 401 authentication failure instead,
// indistinguishable from a bad token by design (see
// TestProtectedEndpoints_MissingTokenIsAuthenticationFailed for that
// case). A body that is valid JSON but has the wrong type for a known
// field (text as a number here) passes RequireScope's narrower decode
// but still fails the handler's own.
func TestHandleNotesCreate_MalformedJSONIsInvalidParam(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.postRaw(t, "/api/notes/create", `{"i":"`+ts.token+`","text":12345}`)
	assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
}

func TestHandleNotesCreate_UnsupportedFieldsFailExplicitly(t *testing.T) {
	ts := newNoteAPITestServer(t)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"visibleUserIds", map[string]any{"text": "x", "visibleUserIds": []string{"someone"}}},
		{"reactionAcceptance", map[string]any{"text": "x", "reactionAcceptance": "likeOnly"}},
		{"renoteId", map[string]any{"text": "x", "renoteId": "some-note"}},
		{"channelId", map[string]any{"text": "x", "channelId": "some-channel"}},
		{"poll", map[string]any{"text": "x", "poll": map[string]any{"choices": []string{"a", "b"}}}},
		{"scheduledAt", map[string]any{"text": "x", "scheduledAt": 1893456000000}},
		{"fileIds", map[string]any{"text": "x", "fileIds": []string{"file-1"}}},
		{"visibility", map[string]any{"text": "x", "visibility": "followers"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := ts.post(t, "/api/notes/create", c.body)
			assertWireError(t, rec, http.StatusBadRequest, "UNSUPPORTED_FEATURE")
		})
	}
}

func TestHandleNotesShow_SuccessNotFoundHiddenArchived(t *testing.T) {
	ts := newNoteAPITestServer(t)
	createRec := ts.post(t, "/api/notes/create", map[string]any{"text": "shown"})
	var created createdNoteResponse
	mustDecode(t, createRec, &created)

	t.Run("success", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": created.CreatedNote.ID})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
		}
		var n note
		mustDecode(t, rec, &n)
		if n.ID != created.CreatedNote.ID {
			t.Errorf("id = %q, want %q", n.ID, created.CreatedNote.ID)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": "does-not-exist"})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})

	t.Run("missing_note_id", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/show", map[string]any{})
		assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
	})

	t.Run("hidden", func(t *testing.T) {
		if err := ts.timeline.SetHidden(t.Context(), created.CreatedNote.ID, true); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ts.timeline.SetHidden(t.Context(), created.CreatedNote.ID, false) }()
		rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": created.CreatedNote.ID})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})

	t.Run("archived", func(t *testing.T) {
		if err := ts.timeline.SetArchived(t.Context(), created.CreatedNote.ID, true); err != nil {
			t.Fatal(err)
		}
		rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": created.CreatedNote.ID})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})
}

func TestHandleNotesTimeline_NewestFirstAndUntilIdPaging(t *testing.T) {
	ts := newNoteAPITestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "post"})
		var created createdNoteResponse
		mustDecode(t, rec, &created)
		ids = append(ids, created.CreatedNote.ID) // ids[0] oldest ... ids[2] newest
		ts.clock.Advance(time.Minute)
	}

	first := ts.post(t, "/api/notes/timeline", map[string]any{"limit": 2})
	var firstNotes []note
	mustDecode(t, first, &firstNotes)
	if len(firstNotes) != 2 || firstNotes[0].ID != ids[2] || firstNotes[1].ID != ids[1] {
		t.Fatalf("first page ids = %v, want newest-first [%s, %s]", noteIDs(firstNotes), ids[2], ids[1])
	}

	second := ts.post(t, "/api/notes/timeline", map[string]any{"limit": 2, "untilId": ids[1]})
	var secondNotes []note
	mustDecode(t, second, &secondNotes)
	if len(secondNotes) != 1 || secondNotes[0].ID != ids[0] {
		t.Fatalf("second page ids = %v, want [%s]", noteIDs(secondNotes), ids[0])
	}
}

func TestHandleNotesTimeline_UnknownUntilIdReturnsEmptyPage(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/timeline", map[string]any{"untilId": "does-not-exist"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 0 {
		t.Errorf("notes = %v, want empty", notes)
	}
}

func TestHandleNotesTimeline_ExcludesHidden(t *testing.T) {
	ts := newNoteAPITestServer(t)
	visibleRec := ts.post(t, "/api/notes/create", map[string]any{"text": "visible"})
	var visible createdNoteResponse
	mustDecode(t, visibleRec, &visible)
	ts.clock.Advance(time.Minute)
	hiddenRec := ts.post(t, "/api/notes/create", map[string]any{"text": "hidden"})
	var hidden createdNoteResponse
	mustDecode(t, hiddenRec, &hidden)
	if err := ts.timeline.SetHidden(t.Context(), hidden.CreatedNote.ID, true); err != nil {
		t.Fatal(err)
	}

	rec := ts.post(t, "/api/notes/timeline", map[string]any{})
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 1 || notes[0].ID != visible.CreatedNote.ID {
		t.Errorf("notes = %v, want only %s", noteIDs(notes), visible.CreatedNote.ID)
	}
}

func TestHandleNotesConversation_OldestFirstAncestorsExcludingHidden(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rootRec := ts.post(t, "/api/notes/create", map[string]any{"text": "root"})
	var root createdNoteResponse
	mustDecode(t, rootRec, &root)
	ts.clock.Advance(time.Minute)

	child1Rec := ts.post(t, "/api/notes/create", map[string]any{"text": "child1", "replyId": root.CreatedNote.ID})
	var child1 createdNoteResponse
	mustDecode(t, child1Rec, &child1)
	ts.clock.Advance(time.Minute)

	child2Rec := ts.post(t, "/api/notes/create", map[string]any{"text": "child2", "replyId": child1.CreatedNote.ID})
	var child2 createdNoteResponse
	mustDecode(t, child2Rec, &child2)

	rec := ts.post(t, "/api/notes/conversation", map[string]any{"noteId": child2.CreatedNote.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 2 || notes[0].ID != root.CreatedNote.ID || notes[1].ID != child1.CreatedNote.ID {
		t.Fatalf("conversation ids = %v, want oldest-first [%s, %s]", noteIDs(notes), root.CreatedNote.ID, child1.CreatedNote.ID)
	}

	if err := ts.timeline.SetHidden(t.Context(), child1.CreatedNote.ID, true); err != nil {
		t.Fatal(err)
	}
	rec2 := ts.post(t, "/api/notes/conversation", map[string]any{"noteId": child2.CreatedNote.ID})
	var notes2 []note
	mustDecode(t, rec2, &notes2)
	if len(notes2) != 1 || notes2[0].ID != root.CreatedNote.ID {
		t.Errorf("conversation with hidden ancestor = %v, want only [%s]", noteIDs(notes2), root.CreatedNote.ID)
	}
}

func TestHandleNotesConversation_SubjectNotFoundOrHidden(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/notes/conversation", map[string]any{"noteId": "does-not-exist"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")

	createRec := ts.post(t, "/api/notes/create", map[string]any{"text": "subject"})
	var created createdNoteResponse
	mustDecode(t, createRec, &created)
	if err := ts.timeline.SetHidden(t.Context(), created.CreatedNote.ID, true); err != nil {
		t.Fatal(err)
	}
	rec2 := ts.post(t, "/api/notes/conversation", map[string]any{"noteId": created.CreatedNote.ID})
	assertWireError(t, rec2, http.StatusBadRequest, "NO_SUCH_NOTE")
}

func TestHandleNotesChildren_DirectOnlyExcludesHiddenAndPaginates(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rootRec := ts.post(t, "/api/notes/create", map[string]any{"text": "root"})
	var root createdNoteResponse
	mustDecode(t, rootRec, &root)
	ts.clock.Advance(time.Minute)

	var childIDs []string
	for i := 0; i < 3; i++ {
		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "child", "replyId": root.CreatedNote.ID})
		var created createdNoteResponse
		mustDecode(t, rec, &created)
		childIDs = append(childIDs, created.CreatedNote.ID)
		ts.clock.Advance(time.Minute)
	}
	if err := ts.timeline.SetHidden(t.Context(), childIDs[1], true); err != nil {
		t.Fatal(err)
	}

	rec := ts.post(t, "/api/notes/children", map[string]any{"noteId": root.CreatedNote.ID, "depth": 1})
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 2 || notes[0].ID != childIDs[0] || notes[1].ID != childIDs[2] {
		t.Fatalf("children = %v, want oldest-first [%s, %s] (hidden middle child excluded)", noteIDs(notes), childIDs[0], childIDs[2])
	}

	page := ts.post(t, "/api/notes/children", map[string]any{"noteId": root.CreatedNote.ID, "untilId": childIDs[0], "limit": 10})
	var pageNotes []note
	mustDecode(t, page, &pageNotes)
	if len(pageNotes) != 1 || pageNotes[0].ID != childIDs[2] {
		t.Fatalf("page after %s = %v, want [%s]", childIDs[0], noteIDs(pageNotes), childIDs[2])
	}
}

// TestHandleNotesChildren_UnknownUntilIdReturnsEmptyPage mirrors
// TestHandleNotesTimeline_UnknownUntilIdReturnsEmptyPage: a genuinely
// stale/unknown untilId (never one of this note's children at all) must
// yield an empty page, not restart from the first page — otherwise a
// client that pages by repeating the last-seen ID could loop forever.
func TestHandleNotesChildren_UnknownUntilIdReturnsEmptyPage(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rootRec := ts.post(t, "/api/notes/create", map[string]any{"text": "root"})
	var root createdNoteResponse
	mustDecode(t, rootRec, &root)
	ts.clock.Advance(time.Minute)
	ts.post(t, "/api/notes/create", map[string]any{"text": "child", "replyId": root.CreatedNote.ID})

	rec := ts.post(t, "/api/notes/children", map[string]any{"noteId": root.CreatedNote.ID, "untilId": "does-not-exist"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 0 {
		t.Errorf("notes = %v, want empty", noteIDs(notes))
	}
}

// TestHandleNotesChildren_UntilIdOfSinceHiddenChildStillResumes covers the
// case TestHandleNotesChildren_UnknownUntilIdReturnsEmptyPage's old
// (incorrect) doc comment used to conflate with a truly unknown untilId:
// an anchor that was visible when the client fetched it, but has since
// been hidden, must still resolve its position from ListChildren's full
// (archived/hidden-inclusive) result and page through to later visible
// children — not be treated as unknown and short-circuit to an empty
// page, which would silently strand every remaining child forever.
func TestHandleNotesChildren_UntilIdOfSinceHiddenChildStillResumes(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rootRec := ts.post(t, "/api/notes/create", map[string]any{"text": "root"})
	var root createdNoteResponse
	mustDecode(t, rootRec, &root)
	ts.clock.Advance(time.Minute)

	firstRec := ts.post(t, "/api/notes/create", map[string]any{"text": "first child", "replyId": root.CreatedNote.ID})
	var first createdNoteResponse
	mustDecode(t, firstRec, &first)
	ts.clock.Advance(time.Minute)

	secondRec := ts.post(t, "/api/notes/create", map[string]any{"text": "second child", "replyId": root.CreatedNote.ID})
	var second createdNoteResponse
	mustDecode(t, secondRec, &second)

	if err := ts.timeline.SetHidden(t.Context(), first.CreatedNote.ID, true); err != nil {
		t.Fatal(err)
	}

	rec := ts.post(t, "/api/notes/children", map[string]any{"noteId": root.CreatedNote.ID, "untilId": first.CreatedNote.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 1 || notes[0].ID != second.CreatedNote.ID {
		t.Fatalf("page after since-hidden %s = %v, want [%s]", first.CreatedNote.ID, noteIDs(notes), second.CreatedNote.ID)
	}
}

func TestHandleNotesChildren_SubjectNotFound(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/children", map[string]any{"noteId": "does-not-exist"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
}

func noteIDs(notes []note) []string {
	ids := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.ID
	}
	return ids
}

func mustDecode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
}

func assertWireError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Errorf("status = %d %q, want %d", rec.Code, rec.Body.String(), wantStatus)
	}
	var resp wireErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q (body=%s)", resp.Error.Code, wantCode, rec.Body.String())
	}
	if resp.Error.ID == "" || resp.Error.Message == "" {
		t.Errorf("error.id/message must be non-empty: %+v", resp.Error)
	}
}
