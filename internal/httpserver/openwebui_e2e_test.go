package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// TestOpenWebUIEndToEnd_PostToProjectedVirtualActorReply drives the full
// in-process path plan §7.3 describes for PR3: POST /api/notes/create
// commits the post, its conversation link, its turn, and its
// "openwebui_turn" job atomically (already covered by
// TestNotesCreate_OpenWebUIBridgeEnqueuesAtomicallyWithPost); this test
// carries that job the rest of the way — through TurnJob.Handle against
// a fake Provider (e2eFakeProvider, openwebui_testhelpers_test.go) —
// and checks that the resulting VirtualActor-authored reply is visible
// through notes/children with its projected @<slug>@host identity,
// exactly as a real owner client would see it.
func TestOpenWebUIEndToEnd_PostToProjectedVirtualActorReply(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ask the model"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	provider := &e2eFakeProvider{
		startChat: func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
			if err := req.OnChatCreated(ctx, "remote-chat-1"); err != nil {
				return openwebui.TurnResult{}, err
			}
			return openwebui.TurnResult{Content: "the model's reply", RemoteCurrentID: strPtrForTest("remote-msg-1")}, nil
		},
	}
	runOpenWebUITurnJobFor(t, ts, provider, resp.CreatedNote.ID)
	if provider.startChatCalls != 1 {
		t.Errorf("StartChat calls = %d, want 1", provider.startChatCalls)
	}

	childRec := ts.post(t, "/api/notes/children", map[string]any{"noteId": resp.CreatedNote.ID})
	if childRec.Code != http.StatusOK {
		t.Fatalf("notes/children: %d %s", childRec.Code, childRec.Body.String())
	}
	var children []note
	if err := json.Unmarshal(childRec.Body.Bytes(), &children); err != nil {
		t.Fatalf("decode children: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %v, want exactly 1 reply", children)
	}
	reply := children[0]
	if reply.Text == nil || *reply.Text != "[reply]\n\nthe model's reply" {
		t.Errorf("reply.Text = %v, want the marked model reply", reply.Text)
	}
	if reply.User.Host == nil || *reply.User.Host != "openwebui.example.net" {
		t.Errorf("reply.User.Host = %v, want the VirtualActor's presentation host", reply.User.Host)
	}
	if wantSlug := openWebUITestModelSlug(); reply.User.Username != wantSlug {
		t.Errorf("reply.User.Username = %q, want %q", reply.User.Username, wantSlug)
	}

	notifications, err := ts.db.Notifications.ListDesc(t.Context(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].Type != domain.NotificationReply {
		t.Errorf("notifications = %+v, want one reply notification", notifications)
	}
}
