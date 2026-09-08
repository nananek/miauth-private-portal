package httpserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// TestUsersNotes_FiltersToRequestedActorNewestFirst is Issue #114's core
// assertion: a post authored by a different actor (here, the reserved
// system actor, created directly through timeline.Service rather than
// notes/create — which only ever authors as the owner) must never leak
// into a users/notes response scoped to the owner, even though both
// entries share the same entries table with no other endpoint-level
// filter applied.
func TestUsersNotes_FiltersToRequestedActorNewestFirst(t *testing.T) {
	ts := newNoteAPITestServer(t)

	var ownIDs []string
	for i := 0; i < 2; i++ {
		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "own post"})
		var created createdNoteResponse
		mustDecode(t, rec, &created)
		ownIDs = append(ownIDs, created.CreatedNote.ID) // ownIDs[0] oldest, ownIDs[1] newest
		ts.clock.Advance(time.Minute)
	}
	if _, err := ts.timeline.CreateRoot(t.Context(), domain.EntrySystem, "system post"); err != nil {
		t.Fatalf("create system entry: %v", err)
	}

	rec := ts.post(t, "/api/users/notes", map[string]any{"userId": ts.ownerID})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/notes: %d %s", rec.Code, rec.Body.String())
	}
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 2 || notes[0].ID != ownIDs[1] || notes[1].ID != ownIDs[0] {
		t.Fatalf("notes = %v, want newest-first [%s, %s]", noteIDs(notes), ownIDs[1], ownIDs[0])
	}
}

// TestUsersNotes_UntilIdPaging mirrors
// TestHandleNotesTimeline_NewestFirstAndUntilIdPaging exactly, scoped to
// one actor, confirming the shared untilId cursor contract carries over
// unchanged.
func TestUsersNotes_UntilIdPaging(t *testing.T) {
	ts := newNoteAPITestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		rec := ts.post(t, "/api/notes/create", map[string]any{"text": "post"})
		var created createdNoteResponse
		mustDecode(t, rec, &created)
		ids = append(ids, created.CreatedNote.ID) // ids[0] oldest ... ids[2] newest
		ts.clock.Advance(time.Minute)
	}

	first := ts.post(t, "/api/users/notes", map[string]any{"userId": ts.ownerID, "limit": 2})
	var firstNotes []note
	mustDecode(t, first, &firstNotes)
	if len(firstNotes) != 2 || firstNotes[0].ID != ids[2] || firstNotes[1].ID != ids[1] {
		t.Fatalf("first page ids = %v, want newest-first [%s, %s]", noteIDs(firstNotes), ids[2], ids[1])
	}

	second := ts.post(t, "/api/users/notes", map[string]any{"userId": ts.ownerID, "limit": 2, "untilId": ids[1]})
	var secondNotes []note
	mustDecode(t, second, &secondNotes)
	if len(secondNotes) != 1 || secondNotes[0].ID != ids[0] {
		t.Fatalf("second page ids = %v, want [%s]", noteIDs(secondNotes), ids[0])
	}
}

// TestUsersNotes_UnknownUntilIdReturnsEmptyPage mirrors
// TestHandleNotesTimeline_UnknownUntilIdReturnsEmptyPage: a stale/unknown
// untilId is pagination-loop-safe (an empty page), distinct from userId
// itself being unknown, which is checked first and always denied (see
// TestUsersNotes_UnknownUserIdReturnsNoSuchUser).
func TestUsersNotes_UnknownUntilIdReturnsEmptyPage(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/users/notes", map[string]any{"userId": ts.ownerID, "untilId": "does-not-exist"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 0 {
		t.Errorf("notes = %v, want empty", notes)
	}
}

// TestUsersNotes_ExcludesHidden mirrors
// TestHandleNotesTimeline_ExcludesHidden.
func TestUsersNotes_ExcludesHidden(t *testing.T) {
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

	rec := ts.post(t, "/api/users/notes", map[string]any{"userId": ts.ownerID})
	var notes []note
	mustDecode(t, rec, &notes)
	if len(notes) != 1 || notes[0].ID != visible.CreatedNote.ID {
		t.Errorf("notes = %v, want only %s", noteIDs(notes), visible.CreatedNote.ID)
	}
}

// TestUsersNotes_UnknownUserIdReturnsNoSuchUser covers Issue #114's
// "fail explicitly, never fabricate success" acceptance criterion: an
// id outside the known actor set must never be treated as "a real actor
// with zero notes."
func TestUsersNotes_UnknownUserIdReturnsNoSuchUser(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/users/notes", map[string]any{"userId": "no-such-actor-id"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_USER")
}

// TestUsersNotes_MissingUserIdIsInvalidParam covers userId's "required"
// contract.
func TestUsersNotes_MissingUserIdIsInvalidParam(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/users/notes", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d %s, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
}

// TestUsersNotes_ActorWithNoNotesReturnsEmptyArrayNotNull covers a
// known-but-quiet actor: distinct from the unknown-userId case above,
// this must succeed with an empty (not null) array.
func TestUsersNotes_ActorWithNoNotesReturnsEmptyArrayNotNull(t *testing.T) {
	ts := newNoteAPITestServer(t)
	assistant, err := ts.timeline.GetActorByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}

	rec := ts.post(t, "/api/users/notes", map[string]any{"userId": assistant.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/notes: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Errorf("body = %q, want an empty JSON array, not null", got)
	}
}
