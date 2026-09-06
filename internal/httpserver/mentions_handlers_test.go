package httpserver

import (
	"net/http"
	"testing"
	"time"
)

// TestHandleNotesMentions_ReturnsOnlyPostsThatMentionOwner pins the
// A案 real-detection behavior (Issue #23 PR5, owner-confirmed
// 2026-09-06): a user_post whose body contains "@" + the owner's own
// username shows up here; one that does not never does.
func TestHandleNotesMentions_ReturnsOnlyPostsThatMentionOwner(t *testing.T) {
	ts := newNoteAPITestServer(t)

	mentioningRec := ts.post(t, "/api/notes/create", map[string]any{"text": "hey @owner check this out"})
	var mentioning createdNoteResponse
	mustDecode(t, mentioningRec, &mentioning)

	plainRec := ts.post(t, "/api/notes/create", map[string]any{"text": "just a normal post"})
	var plain createdNoteResponse
	mustDecode(t, plainRec, &plain)

	rec := ts.post(t, "/api/notes/mentions", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got []note
	mustDecode(t, rec, &got)
	if len(got) != 1 || got[0].ID != mentioning.CreatedNote.ID {
		t.Fatalf("mentions = %v, want exactly [%s] (not %s)", noteIDs(got), mentioning.CreatedNote.ID, plain.CreatedNote.ID)
	}
}

// TestHandleNotesMentions_DirectTabVisibilitySpecifiedAlwaysEmpty pins
// docs/compat/aria-v1.5.11.md's trace finding: the "Direct" tab's
// visibility: "specified" filter can never match any note this service
// ever creates (everything is "public"), so it always short-circuits to
// an empty page even when a real self-mention exists.
func TestHandleNotesMentions_DirectTabVisibilitySpecifiedAlwaysEmpty(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "hey @owner"})
	var created createdNoteResponse
	mustDecode(t, rec, &created)

	directRec := ts.post(t, "/api/notes/mentions", map[string]any{"visibility": "specified"})
	if directRec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", directRec.Code, directRec.Body.String(), http.StatusOK)
	}
	var got []note
	mustDecode(t, directRec, &got)
	if len(got) != 0 {
		t.Errorf("mentions with visibility=specified = %v, want empty", noteIDs(got))
	}
}

func TestHandleNotesMentions_NewestFirstAndUntilIdPaging(t *testing.T) {
	ts := newNoteAPITestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ping @owner"})
		var created createdNoteResponse
		mustDecode(t, rec, &created)
		ids = append(ids, created.CreatedNote.ID) // ids[0] oldest ... ids[2] newest
		ts.clock.Advance(time.Minute)
	}

	first := ts.post(t, "/api/notes/mentions", map[string]any{"limit": 2})
	var firstNotes []note
	mustDecode(t, first, &firstNotes)
	if len(firstNotes) != 2 || firstNotes[0].ID != ids[2] || firstNotes[1].ID != ids[1] {
		t.Fatalf("first page ids = %v, want newest-first [%s, %s]", noteIDs(firstNotes), ids[2], ids[1])
	}

	second := ts.post(t, "/api/notes/mentions", map[string]any{"limit": 2, "untilId": ids[1]})
	var secondNotes []note
	mustDecode(t, second, &secondNotes)
	if len(secondNotes) != 1 || secondNotes[0].ID != ids[0] {
		t.Fatalf("second page ids = %v, want [%s]", noteIDs(secondNotes), ids[0])
	}
}

func TestHandleNotesMentions_UnknownUntilIdReturnsEmptyPage(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/mentions", map[string]any{"untilId": "does-not-exist"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 0 {
		t.Errorf("notes = %v, want empty", notes)
	}
}

func TestHandleNotesMentions_ExcludesHiddenNote(t *testing.T) {
	ts := newNoteAPITestServer(t)
	visibleRec := ts.post(t, "/api/notes/create", map[string]any{"text": "visible @owner"})
	var visible createdNoteResponse
	mustDecode(t, visibleRec, &visible)
	ts.clock.Advance(time.Minute)
	hiddenRec := ts.post(t, "/api/notes/create", map[string]any{"text": "hidden @owner"})
	var hidden createdNoteResponse
	mustDecode(t, hiddenRec, &hidden)
	if err := ts.timeline.SetHidden(t.Context(), hidden.CreatedNote.ID, true); err != nil {
		t.Fatal(err)
	}

	rec := ts.post(t, "/api/notes/mentions", map[string]any{})
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 1 || notes[0].ID != visible.CreatedNote.ID {
		t.Errorf("notes = %v, want only %s", noteIDs(notes), visible.CreatedNote.ID)
	}
}

func TestHandleNotesMentions_MalformedBodyIsInvalidParam(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.postRaw(t, "/api/notes/mentions", `{"i":"`+ts.token+`","limit":"not-a-number"}`)
	assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
}
