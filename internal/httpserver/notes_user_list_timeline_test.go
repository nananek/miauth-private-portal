package httpserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// timelineListsToken issues a token carrying both the users/lists/*
// scopes (to create the list and push a member) and read:notes (to call
// notes/user-list-timeline itself) — the same combination Aria's fixed
// permission list grants together in practice.
func timelineListsToken(t *testing.T, ts *noteAPITestServer, sessionID string) string {
	t.Helper()
	token, _ := mustIssueToken(t, ts.Server, sessionID, "read:account,write:account,write:notes")
	return token
}

// mustCreateNote posts text through POST /api/notes/create and returns
// the created note's ID, unwrapping createdNoteResponse's
// {"createdNote": {...}} envelope so this file's tests can read a note
// ID in one line.
func mustCreateNote(t *testing.T, ts *noteAPITestServer, token, text string) string {
	t.Helper()
	rec := ts.post(t, "/api/notes/create", map[string]any{"i": token, "text": text})
	if rec.Code != http.StatusOK {
		t.Fatalf("notes/create: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponseForTest
	mustDecode(t, rec, &resp)
	return resp.CreatedNote.ID
}

func TestNotesUserListTimeline_ReturnsOnlyMemberAuthoredNotesNewestFirst(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := timelineListsToken(t, ts, "ul-timeline-basic")

	// A system-authored entry, created through timeline.Service directly
	// (not POST /api/notes/create, which only ever authors as the owner):
	// the system actor is never pushed onto the list below, so its
	// presence in results would prove membership filtering broken. A
	// post's own timing relative to when its author joined the list is
	// deliberately irrelevant here — real Misskey's user list timeline
	// filters by current membership only, not by post-vs-join order.
	if _, err := ts.timeline.CreateRoot(t.Context(), domain.EntrySystem, "not in the list", nil); err != nil {
		t.Fatalf("create non-member entry: %v", err)
	}

	var created userListResponse
	mustDecode(t, ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "l"}), &created)

	if rec := ts.post(t, "/api/users/lists/push", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID}); rec.Code != http.StatusNoContent {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}

	ts.clock.Advance(time.Second)
	olderID := mustCreateNote(t, ts, token, "older member post")
	ts.clock.Advance(time.Second)
	newerID := mustCreateNote(t, ts, token, "newer member post")

	rec := ts.post(t, "/api/notes/user-list-timeline", map[string]any{"i": token, "listId": created.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("notes/user-list-timeline: %d %s", rec.Code, rec.Body.String())
	}
	var notes []noteResponseForTest
	mustDecode(t, rec, &notes)
	if len(notes) != 2 || notes[0].ID != newerID || notes[1].ID != olderID {
		t.Fatalf("notes = %v, want newest-first [%s, %s] (non-member's post excluded)", noteIDsForTest(notes), newerID, olderID)
	}
}

func TestNotesUserListTimeline_EmptyListReturnsEmptyPage(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := timelineListsToken(t, ts, "ul-timeline-empty")

	mustCreateNote(t, ts, token, "some post")

	var created userListResponse
	mustDecode(t, ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "empty"}), &created)

	rec := ts.post(t, "/api/notes/user-list-timeline", map[string]any{"i": token, "listId": created.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("notes/user-list-timeline: %d %s", rec.Code, rec.Body.String())
	}
	var notes []noteResponseForTest
	mustDecode(t, rec, &notes)
	if len(notes) != 0 {
		t.Errorf("notes = %v, want empty for a zero-member list", notes)
	}
}

func TestNotesUserListTimeline_PullStopsFutureMemberPostsFromAppearing(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := timelineListsToken(t, ts, "ul-timeline-pull")

	var created userListResponse
	mustDecode(t, ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "l"}), &created)
	if rec := ts.post(t, "/api/users/lists/push", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID}); rec.Code != http.StatusNoContent {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}
	mustCreateNote(t, ts, token, "before pull")

	if rec := ts.post(t, "/api/users/lists/pull", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID}); rec.Code != http.StatusNoContent {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body.String())
	}
	mustCreateNote(t, ts, token, "after pull")

	rec := ts.post(t, "/api/notes/user-list-timeline", map[string]any{"i": token, "listId": created.ID})
	var notes []noteResponseForTest
	mustDecode(t, rec, &notes)
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want empty: pulled member's earlier post must also stop counting toward this list", notes)
	}
}

func TestNotesUserListTimeline_UntilIDPagesToOlderEntries(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := timelineListsToken(t, ts, "ul-timeline-paging")

	var created userListResponse
	mustDecode(t, ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "l"}), &created)
	if rec := ts.post(t, "/api/users/lists/push", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID}); rec.Code != http.StatusNoContent {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}

	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, mustCreateNote(t, ts, token, "post")) // ids[0] oldest, ids[2] newest
		ts.clock.Advance(time.Second)
	}

	firstRec := ts.post(t, "/api/notes/user-list-timeline", map[string]any{"i": token, "listId": created.ID, "limit": 2})
	var first []noteResponseForTest
	mustDecode(t, firstRec, &first)
	if len(first) != 2 || first[0].ID != ids[2] || first[1].ID != ids[1] {
		t.Fatalf("first page = %v, want newest-first [%s, %s]", noteIDsForTest(first), ids[2], ids[1])
	}

	secondRec := ts.post(t, "/api/notes/user-list-timeline", map[string]any{"i": token, "listId": created.ID, "untilId": first[1].ID})
	var second []noteResponseForTest
	mustDecode(t, secondRec, &second)
	if len(second) != 1 || second[0].ID != ids[0] {
		t.Fatalf("second page = %v, want [%s]", noteIDsForTest(second), ids[0])
	}
}

func TestNotesUserListTimeline_UnknownListIsNoSuchList(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := timelineListsToken(t, ts, "ul-timeline-unknown-list")

	rec := ts.post(t, "/api/notes/user-list-timeline", map[string]any{"i": token, "listId": "does-not-exist"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_LIST")
}

func TestNotesUserListTimeline_RequiresListID(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := timelineListsToken(t, ts, "ul-timeline-requires-listid")

	rec := ts.post(t, "/api/notes/user-list-timeline", map[string]any{"i": token})
	assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
}

// noteResponseForTest decodes just the ID field this file's tests need
// out of a wire note — mirrors how other note-API test files (e.g.
// noteapi_handlers_test.go) avoid importing the unexported `note` type
// symbol chains by decoding into a narrow local struct.
type noteResponseForTest struct {
	ID string `json:"id"`
}

// createdNoteResponseForTest decodes POST /api/notes/create's
// {"createdNote": {...}} envelope (createdNoteResponse in
// noteapi_handlers.go).
type createdNoteResponseForTest struct {
	CreatedNote noteResponseForTest `json:"createdNote"`
}

func noteIDsForTest(notes []noteResponseForTest) []string {
	ids := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.ID
	}
	return ids
}
