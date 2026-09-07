package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// TestOpenWebUIBranchIsolation_ReplyToEarlierAncestorStartsNewRemoteChatButStaysInSameLocalThread
// is Issue #54 (OWUI-R)'s branch-isolation acceptance criterion: replying
// to an earlier local ancestor — not the current branch's head — must
// start a brand new remote chat rather than continuing the existing one
// (internal/openwebui/path.go's SelectBranch, exercised at the unit
// level by TestSelectBranch_NoContinuationForReplyToEarlierNode and
// TestTurnJob_StaleBranchIsolation_CompletionOnlyMovesItsOwnLink), while
// the new reply stays exactly where the local reply-tree says it
// belongs: a direct child of the root it replied to, a sibling of the
// root's first assistant-reply branch, never spliced into that branch's
// own history.
func TestOpenWebUIBranchIsolation_ReplyToEarlierAncestorStartsNewRemoteChatButStaysInSameLocalThread(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)

	provider := &e2eFakeProvider{}
	provider.startChat = func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
		// provider.startChatCalls is already incremented for this call by
		// the wrapping e2eFakeProvider.StartChat, so it doubles as this
		// call's own 1-based sequence number.
		chatID := fmt.Sprintf("remote-chat-%d", provider.startChatCalls)
		if err := req.OnChatCreated(ctx, chatID); err != nil {
			return openwebui.TurnResult{}, err
		}
		return openwebui.TurnResult{Content: "assistant reply on " + chatID, RemoteCurrentID: strPtrForTest("remote-msg-" + chatID)}, nil
	}
	provider.continueTurn = func(ctx context.Context, req openwebui.ContinueTurnRequest) (openwebui.TurnResult, error) {
		return openwebui.TurnResult{Content: "assistant continuation on " + req.RemoteChatID, RemoteCurrentID: strPtrForTest("remote-msg-continue-" + req.RemoteChatID)}, nil
	}

	// Root post -> its first assistant reply, on remote chat 1.
	rootRec := ts.post(t, "/api/notes/create", map[string]any{"text": "owner root post"})
	if rootRec.Code != http.StatusOK {
		t.Fatalf("create root note: %d %s", rootRec.Code, rootRec.Body.String())
	}
	var rootResp createdNoteResponse
	if err := json.Unmarshal(rootRec.Body.Bytes(), &rootResp); err != nil {
		t.Fatalf("decode root: %v", err)
	}
	rootID := rootResp.CreatedNote.ID

	runOpenWebUITurnJobFor(t, ts, provider, rootID)
	if provider.startChatCalls != 1 {
		t.Fatalf("startChatCalls after root turn = %d, want 1", provider.startChatCalls)
	}
	a1 := onlyChildOf(t, ts, rootID)
	if a1.ReplyID == nil || *a1.ReplyID != rootID {
		t.Fatalf("a1.ReplyID = %v, want %q", a1.ReplyID, rootID)
	}

	// A follow-up on a1 (the branch's current head) continues remote
	// chat 1, not a new one.
	followUpRec := ts.post(t, "/api/notes/create", map[string]any{"text": "owner follow-up on a1", "replyId": a1.ID})
	if followUpRec.Code != http.StatusOK {
		t.Fatalf("create follow-up note: %d %s", followUpRec.Code, followUpRec.Body.String())
	}
	var followUpResp createdNoteResponse
	if err := json.Unmarshal(followUpRec.Body.Bytes(), &followUpResp); err != nil {
		t.Fatalf("decode follow-up: %v", err)
	}
	followUpID := followUpResp.CreatedNote.ID

	runOpenWebUITurnJobFor(t, ts, provider, followUpID)
	if provider.startChatCalls != 1 || provider.continueTurnCalls != 1 {
		t.Fatalf("after follow-up: startChatCalls=%d continueTurnCalls=%d, want 1,1", provider.startChatCalls, provider.continueTurnCalls)
	}
	onlyChildOf(t, ts, followUpID)

	// A second, independent reply to the ROOT itself — an earlier
	// ancestor of the branch's current head, not the head — must not
	// continue remote chat 1: it starts a brand new remote chat 2.
	rootReply2Rec := ts.post(t, "/api/notes/create", map[string]any{"text": "owner second reply to root", "replyId": rootID})
	if rootReply2Rec.Code != http.StatusOK {
		t.Fatalf("create second root reply: %d %s", rootReply2Rec.Code, rootReply2Rec.Body.String())
	}
	var rootReply2Resp createdNoteResponse
	if err := json.Unmarshal(rootReply2Rec.Body.Bytes(), &rootReply2Resp); err != nil {
		t.Fatalf("decode second root reply: %v", err)
	}
	rootReply2ID := rootReply2Resp.CreatedNote.ID

	runOpenWebUITurnJobFor(t, ts, provider, rootReply2ID)
	if provider.startChatCalls != 2 {
		t.Fatalf("startChatCalls after second root reply = %d, want 2 (a new remote chat, not a continuation)", provider.startChatCalls)
	}
	if provider.continueTurnCalls != 1 {
		t.Fatalf("continueTurnCalls after second root reply = %d, want unchanged 1", provider.continueTurnCalls)
	}
	b1 := onlyChildOf(t, ts, rootReply2ID)
	if b1.ReplyID == nil || *b1.ReplyID != rootReply2ID {
		t.Fatalf("b1.ReplyID = %v, want %q", b1.ReplyID, rootReply2ID)
	}

	// Local thread shape: root's direct children are a1 and the owner's
	// second reply — the same local thread throughout; branch separation
	// happened only at the remote-chat level.
	rootChildren := notesChildren(t, ts, rootID)
	if len(rootChildren) != 2 {
		t.Fatalf("root children = %v, want exactly 2 (a1 and the owner's second reply)", rootChildren)
	}
	gotIDs := map[string]bool{rootChildren[0].ID: true, rootChildren[1].ID: true}
	if !gotIDs[a1.ID] || !gotIDs[rootReply2ID] {
		t.Fatalf("root children = %v, want %q and %q", rootChildren, a1.ID, rootReply2ID)
	}

	// Two distinct conversation links, on two distinct remote chats.
	links, err := ts.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 2 {
		t.Fatalf("links = %+v, want exactly 2", links)
	}
	remoteChatIDs := map[string]bool{}
	for _, l := range links {
		if l.RemoteChatID == nil {
			t.Fatalf("link %+v has no remote chat id", l)
		}
		remoteChatIDs[*l.RemoteChatID] = true
	}
	if len(remoteChatIDs) != 2 {
		t.Fatalf("distinct remote chat ids = %v, want exactly 2", remoteChatIDs)
	}
}
