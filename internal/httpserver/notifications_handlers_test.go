package httpserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// mustIssueNotificationsToken issues a token carrying write:notes (to set
// up root notes to reply to) and read:notifications (to call POST
// /api/i/notifications itself) for a unique routeSessionID.
func mustIssueNotificationsToken(t *testing.T, ts *noteAPITestServer, routeSessionID string) string {
	t.Helper()
	token, _ := mustIssueToken(t, ts.Server, routeSessionID, "write:notes,read:notifications")
	return token
}

// mustCreateGeneratedReplyForTest drives timeline.Service.CreateGeneratedReply
// directly (mirroring reactions_handlers_test.go's
// TestHandleNotesReactionsCreate_AllowsReactingToNonOwnAuthoredNote
// fixture pattern), since Issue #9's job pipeline — not this HTTP
// layer — is what normally calls it after a provider response.
func mustCreateGeneratedReplyForTest(t *testing.T, ts *noteAPITestServer, targetEntryID string, kind domain.EntryKind, body string) domain.Entry {
	t.Helper()
	gen := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: targetEntryID, Kind: domain.GenerationReply,
		Provider: "test-provider", Model: "test-model", PromptVersion: "test-v1",
		Status: domain.GenerationPending, RequestedAt: ts.clock.Now(),
	}
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	entry, err := ts.timeline.CreateGeneratedReply(t.Context(), targetEntryID, kind, body, gen.ID, nil, nil)
	if err != nil {
		t.Fatalf("CreateGeneratedReply: %v", err)
	}
	return entry
}

// mustCreateExternalSourceForTest creates a fresh "rss" ExternalSource
// (mirroring internal/timeline/service_test.go's own fixture of the same
// name). A unique URI per call keeps external_sources' UNIQUE(kind, uri)
// constraint from rejecting a test that needs more than one source.
func mustCreateExternalSourceForTest(t *testing.T, ts *noteAPITestServer) domain.ExternalSource {
	t.Helper()
	source := domain.ExternalSource{ID: domain.NewID(), Kind: "rss", URI: "https://example.com/" + domain.NewID(), CreatedAt: ts.clock.Now()}
	if err := ts.db.ExternalSources.Create(t.Context(), source); err != nil {
		t.Fatalf("create external source: %v", err)
	}
	return source
}

// mustCreateExternalEntryForTest drives timeline.Service.CreateExternalEntry
// directly against source (mirroring internal/timeline/service_test.go's
// own fixture pattern), since Issue #11/#12's ingestion job pipeline —
// not this HTTP layer — is what normally calls it. Callers that need
// several items sharing dedupe/redelivery behavior must pass the same
// source and ExternalID/DedupeKey.
func mustCreateExternalEntryForTest(t *testing.T, ts *noteAPITestServer, source domain.ExternalSource, dedupeKey, body string) (domain.Entry, bool) {
	t.Helper()
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: dedupeKey, DedupeKey: dedupeKey}
	entry, created, err := ts.timeline.CreateExternalEntry(t.Context(), domain.EntryNews, item, body)
	if err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}
	return entry, created
}

// TestHandleAPINotifications_ReplyNotificationIncludesFullNote pins
// plan-issue-23 §1 PR6's "reply" mapping: a "reply" notification's `note`
// field is the full wire Note for the generated reply itself (not its
// target), matching notification_widget.dart's
// NoteWidget(noteId: note.id) rendering.
func TestHandleAPINotifications_ReplyNotificationIncludesFullNote(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := mustIssueNotificationsToken(t, ts, "notifications-reply-session")

	rootRec := ts.post(t, "/api/notes/create", map[string]any{"i": token, "text": "root"})
	var root createdNoteResponse
	mustDecode(t, rootRec, &root)

	reply := mustCreateGeneratedReplyForTest(t, ts, root.CreatedNote.ID, domain.EntryLLMReply, "generated reply body")

	rec := ts.post(t, "/api/i/notifications", map[string]any{"i": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got []notificationResponse
	mustDecode(t, rec, &got)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Type != "reply" {
		t.Errorf("type = %q, want %q", got[0].Type, "reply")
	}
	if got[0].ID == "" || got[0].CreatedAt == "" {
		t.Errorf("id/createdAt must be non-empty: %+v", got[0])
	}
	if got[0].Note == nil || got[0].Note.ID != reply.ID {
		t.Fatalf("note = %v, want the generated reply %q", got[0].Note, reply.ID)
	}
	if got[0].Note.User.Username != "assistant" {
		t.Errorf("note.user.username = %q, want assistant", got[0].Note.User.Username)
	}
	if got[0].Body != nil {
		t.Errorf("body = %v, want nil for a reply notification", got[0].Body)
	}
}

// TestHandleAPINotifications_AppNotificationIncludesBody pins the "app"
// mapping for a genuinely new ingested news/mail item: `body` carries the
// related entry's text verbatim and `note` is left unset.
func TestHandleAPINotifications_AppNotificationIncludesBody(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := mustIssueNotificationsToken(t, ts, "notifications-app-session")

	source := mustCreateExternalSourceForTest(t, ts)
	entry, created := mustCreateExternalEntryForTest(t, ts, source, "dedupe-1", "breaking news body")
	if !created {
		t.Fatal("created = false, want true for a brand new item")
	}

	rec := ts.post(t, "/api/i/notifications", map[string]any{"i": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got []notificationResponse
	mustDecode(t, rec, &got)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Type != "app" {
		t.Errorf("type = %q, want %q", got[0].Type, "app")
	}
	if got[0].Body == nil || *got[0].Body != entry.Body {
		t.Errorf("body = %v, want %q", got[0].Body, entry.Body)
	}
	if got[0].Note != nil {
		t.Errorf("note = %v, want nil for an app notification", got[0].Note)
	}
}

// TestHandleAPINotifications_DuplicateExternalDeliveryDoesNotDuplicate
// pins that a redelivered (dedupe-matched) external item never produces
// a second notification, mirroring CreateExternalEntry's own
// created=false safety net.
func TestHandleAPINotifications_DuplicateExternalDeliveryDoesNotDuplicate(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := mustIssueNotificationsToken(t, ts, "notifications-dedupe-session")

	source := mustCreateExternalSourceForTest(t, ts)
	mustCreateExternalEntryForTest(t, ts, source, "dedupe-1", "first delivery")
	if _, created := mustCreateExternalEntryForTest(t, ts, source, "dedupe-1", "first delivery"); created {
		t.Fatal("second delivery: created = true, want false")
	}

	rec := ts.post(t, "/api/i/notifications", map[string]any{"i": token})
	var got []notificationResponse
	mustDecode(t, rec, &got)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want exactly 1 despite the duplicate delivery", len(got))
	}
}

func TestHandleAPINotifications_NewestFirstAndUntilIdPaging(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := mustIssueNotificationsToken(t, ts, "notifications-paging-session")

	rootRec := ts.post(t, "/api/notes/create", map[string]any{"i": token, "text": "root"})
	var root createdNoteResponse
	mustDecode(t, rootRec, &root)

	var ids []string
	for i := 0; i < 3; i++ {
		reply := mustCreateGeneratedReplyForTest(t, ts, root.CreatedNote.ID, domain.EntryLLMReply, "reply")
		ids = append(ids, reply.ID) // ids[0] oldest ... ids[2] newest
		ts.clock.Advance(time.Minute)
	}

	first := ts.post(t, "/api/i/notifications", map[string]any{"i": token, "limit": 2})
	var firstPage []notificationResponse
	mustDecode(t, first, &firstPage)
	if len(firstPage) != 2 || firstPage[0].Note == nil || firstPage[0].Note.ID != ids[2] || firstPage[1].Note == nil || firstPage[1].Note.ID != ids[1] {
		t.Fatalf("first page = %v, want newest-first notes [%s, %s]", notificationNoteIDs(firstPage), ids[2], ids[1])
	}

	second := ts.post(t, "/api/i/notifications", map[string]any{"i": token, "limit": 2, "untilId": firstPage[1].ID})
	var secondPage []notificationResponse
	mustDecode(t, second, &secondPage)
	if len(secondPage) != 1 || secondPage[0].Note == nil || secondPage[0].Note.ID != ids[0] {
		t.Fatalf("second page = %v, want [%s]", notificationNoteIDs(secondPage), ids[0])
	}
}

func TestHandleAPINotifications_UnknownUntilIdReturnsEmptyPage(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := mustIssueNotificationsToken(t, ts, "notifications-unknown-untilid-session")

	rec := ts.post(t, "/api/i/notifications", map[string]any{"i": token, "untilId": "does-not-exist"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got []notificationResponse
	mustDecode(t, rec, &got)
	if len(got) != 0 {
		t.Errorf("got = %v, want empty for an unknown untilId", got)
	}
}

func TestHandleAPINotifications_EmptyWhenNoneDelivered(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := mustIssueNotificationsToken(t, ts, "notifications-empty-session")

	rec := ts.post(t, "/api/i/notifications", map[string]any{"i": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got []notificationResponse
	mustDecode(t, rec, &got)
	if len(got) != 0 {
		t.Errorf("got = %v, want empty", got)
	}
}

func TestHandleAPINotifications_MalformedBodyIsInvalidParam(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := mustIssueNotificationsToken(t, ts, "notifications-malformed-session")

	rec := ts.postRaw(t, "/api/i/notifications", `{"i":"`+token+`","limit":"not-a-number"}`)
	assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
}

func notificationNoteIDs(notifications []notificationResponse) []string {
	ids := make([]string, len(notifications))
	for i, n := range notifications {
		if n.Note != nil {
			ids[i] = n.Note.ID
		}
	}
	return ids
}
