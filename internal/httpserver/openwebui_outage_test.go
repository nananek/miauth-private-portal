package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// TestOpenWebUIOutage_NotesCreateSucceedsRegardlessOfProviderReachability
// is Issue #54 (OWUI-R)'s outage acceptance criterion, its notes/create
// half: an owner post must be saved even when the configured Open WebUI
// provider is completely unreachable. That much is structurally
// guaranteed by /api/notes/create committing the post, its conversation
// link, its turn, and its "openwebui_turn" job in one transaction before
// any provider call is ever made (already covered at the unit level by
// TestNotesCreate_OpenWebUIBridgeEnqueuesAtomicallyWithPost) — this test
// carries it further: the note stays intact, with no reply, once the job
// actually runs against an unreachable provider and reaches a terminal
// (ambiguous) state without ever panicking or blocking.
func TestOpenWebUIOutage_NotesCreateSucceedsRegardlessOfProviderReachability(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)

	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "owner post during an outage"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rootID := resp.CreatedNote.ID

	showRec := ts.post(t, "/api/notes/show", map[string]any{"noteId": rootID})
	if showRec.Code != http.StatusOK {
		t.Fatalf("notes/show (post committed before the job ever runs): %d %s", showRec.Code, showRec.Body.String())
	}

	provider := &e2eFakeProvider{
		startChat: func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
			return openwebui.TurnResult{}, openwebui.NewProviderError(openwebui.CategoryTimeout, openwebui.PhaseCreate, context.DeadlineExceeded)
		},
	}
	err := runOpenWebUITurnJobForIgnoringError(t, ts, provider, rootID)
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("TurnJob.Handle error = %v, want a jobs.PermanentError (an unreachable provider does not jam the job queue)", err)
	}

	// The owner's post itself is completely unaffected by the outage:
	// still there, with no reply attached.
	stillShowRec := ts.post(t, "/api/notes/show", map[string]any{"noteId": rootID})
	if stillShowRec.Code != http.StatusOK {
		t.Fatalf("notes/show after outage: %d %s", stillShowRec.Code, stillShowRec.Body.String())
	}
	if children := notesChildren(t, ts, rootID); len(children) != 0 {
		t.Fatalf("children after outage = %v, want none (the provider never produced a reply)", children)
	}

	links, err := ts.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].State != domain.LinkAmbiguous {
		t.Fatalf("links after outage = %+v, want exactly 1 ambiguous link", links)
	}
}

// TestOpenWebUIOutage_DuplicateJobDeliveryNeverDuplicatesReplyOrRemoteChat
// is Issue #54 (OWUI-R)'s duplicate-delivery acceptance criterion: an
// at-least-once job queue redelivering the very same "openwebui_turn" job
// after it has already succeeded must never call the provider a second
// time, never create a second remote chat, and never project a second
// assistant reply.
func TestOpenWebUIOutage_DuplicateJobDeliveryNeverDuplicatesReplyOrRemoteChat(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)

	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ask the model, twice-delivered"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rootID := resp.CreatedNote.ID

	provider := &e2eFakeProvider{
		startChat: func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
			if err := req.OnChatCreated(ctx, "remote-chat-1"); err != nil {
				return openwebui.TurnResult{}, err
			}
			return openwebui.TurnResult{Content: "the model's reply", RemoteCurrentID: strPtrForTest("remote-msg-1")}, nil
		},
	}
	turnJob := findOpenWebUITurnJobFor(t, ts, rootID)
	handler := openwebui.NewTurnJob(ts.db.Repos, ts.timeline, provider, nil, nil, openwebui.TurnJobConfig{MaxAttempts: 8, MaxContextMessages: 100}, ts.clock, nil)

	if err := handler.Handle(t.Context(), turnJob); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if provider.startChatCalls != 1 {
		t.Fatalf("startChatCalls after first delivery = %d, want 1", provider.startChatCalls)
	}

	// The exact same job row, redelivered — an at-least-once queue's
	// redelivery, not a second EnqueueTurn.
	if err := handler.Handle(t.Context(), turnJob); err != nil {
		t.Fatalf("second (duplicate) Handle: %v, want nil", err)
	}
	if provider.startChatCalls != 1 {
		t.Fatalf("startChatCalls after duplicate delivery = %d, want still 1", provider.startChatCalls)
	}

	links, err := ts.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].RemoteChatID == nil || *links[0].RemoteChatID != "remote-chat-1" {
		t.Fatalf("links after duplicate delivery = %+v, want exactly 1 with remote chat remote-chat-1", links)
	}

	if children := notesChildren(t, ts, rootID); len(children) != 1 {
		t.Fatalf("children after duplicate delivery = %v, want exactly 1 reply", children)
	}
}
