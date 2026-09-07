package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// restartFakeProvider is a scripted openwebui.Provider for
// TestOpenWebUIRestart_ThreadAndLinkStatePersistAcrossIndependentHarnessInstances:
// unlike e2eFakeProvider (openwebui_e2e_test.go), it also answers
// ContinueTurn, since this test drives a branch across more than one
// turn to exercise restart-surviving continuation, not just a single
// StartChat. LookupTurnOutcome is left unscripted (errNotScripted, from
// openwebui_e2e_test.go): no scenario here revisits an in-flight turn.
type restartFakeProvider struct {
	startChatCalls    int
	continueTurnCalls int
}

func (p *restartFakeProvider) StartChat(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
	p.startChatCalls++
	if err := req.OnChatCreated(ctx, "remote-chat-restart-1"); err != nil {
		return openwebui.TurnResult{}, err
	}
	return openwebui.TurnResult{
		Content:         req.NewTurn.Content + " :: assistant reply 1",
		RemoteCurrentID: strPtrForTest("remote-msg-restart-1"),
	}, nil
}

func (p *restartFakeProvider) ContinueTurn(ctx context.Context, req openwebui.ContinueTurnRequest) (openwebui.TurnResult, error) {
	p.continueTurnCalls++
	return openwebui.TurnResult{
		Content:         req.NewTurn.Content + fmt.Sprintf(" :: assistant reply %d", p.continueTurnCalls+1),
		RemoteCurrentID: strPtrForTest(fmt.Sprintf("remote-msg-restart-continue-%d", p.continueTurnCalls)),
	}, nil
}

func (p *restartFakeProvider) LookupTurnOutcome(ctx context.Context, remoteChatID, assistantMessageID string) (openwebui.TurnOutcome, error) {
	return openwebui.TurnOutcome{}, errNotScripted
}

// runOpenWebUITurnJobFor finds the pending "openwebui_turn" job enqueued
// for sourceEntryID and hands it to a fresh *openwebui.TurnJob backed by
// ts's own repos/timeline and provider, mirroring how
// TestOpenWebUIEndToEnd_PostToProjectedVirtualActorReply (openwebui_e2e_
// test.go) carries a job the rest of the way to a projected reply.
func runOpenWebUITurnJobFor(t *testing.T, ts *noteAPITestServer, provider openwebui.Provider, sourceEntryID string) {
	t.Helper()
	jobRows, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var turnJob domain.Job
	for _, j := range jobRows {
		if j.JobType == openwebui.JobType && j.SourceEntryID != nil && *j.SourceEntryID == sourceEntryID {
			turnJob = j
		}
	}
	if turnJob.ID == "" {
		t.Fatalf("no %q job for source entry %q among %v", openwebui.JobType, sourceEntryID, jobRows)
	}
	handler := openwebui.NewTurnJob(ts.db.Repos, ts.timeline, provider, openwebui.TurnJobConfig{MaxAttempts: 8, MaxContextMessages: 100}, ts.clock, nil)
	if err := handler.Handle(t.Context(), turnJob); err != nil {
		t.Fatalf("TurnJob.Handle: %v", err)
	}
}

// onlyChildOf posts /api/notes/children for parentID and requires
// exactly one reply, returning it. Every step in this test's chain is a
// single reply to the note before it, so "exactly one child" also pins
// that restart never duplicates a reply onto the same parent.
func onlyChildOf(t *testing.T, ts *noteAPITestServer, parentID string) note {
	t.Helper()
	rec := ts.post(t, "/api/notes/children", map[string]any{"noteId": parentID})
	if rec.Code != http.StatusOK {
		t.Fatalf("notes/children(%s): %d %s", parentID, rec.Code, rec.Body.String())
	}
	var children []note
	if err := json.Unmarshal(rec.Body.Bytes(), &children); err != nil {
		t.Fatalf("decode children: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children of %s = %v, want exactly 1", parentID, children)
	}
	return children[0]
}

// TestOpenWebUIRestart_ThreadAndLinkStatePersistAcrossIndependentHarnessInstances
// is Issue #54 (OWUI-R)'s restart-recovery acceptance criterion: an
// owner root -> assistant -> follow-up -> assistant chain must resume,
// after a process restart, in the same local thread and reply_to_id
// tree, continuing the same remote chat rather than starting a new one.
//
// "Restart" is modeled the way plan54 (workerA) settled on: two fully
// independent *noteAPITestServer harnesses (their own *Server, MiAuth
// service, timeline service, and Open WebUI Registry/Bridge — nothing
// shared in memory) built one after the other against the very same
// on-disk SQLite file, with the first harness's database handle closed
// before the second is opened. A real cmd/server restart differs only in
// starting a new OS process instead of a new Go value, which does not
// change what this test needs to observe: everything that survives a
// restart must already live in that file, not in either harness's
// in-memory state.
func TestOpenWebUIRestart_ThreadAndLinkStatePersistAcrossIndependentHarnessInstances(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")

	// --- First "process": root post, its first assistant reply, and one
	// owner follow-up continuing the same branch. ---
	ts1 := newNoteAPITestServerOpenWebUIEnabledAt(t, dbPath, "note-api-restart-1")

	rootRec := ts1.post(t, "/api/notes/create", map[string]any{"text": "owner root post"})
	if rootRec.Code != http.StatusOK {
		t.Fatalf("create root note: %d %s", rootRec.Code, rootRec.Body.String())
	}
	var rootResp createdNoteResponse
	if err := json.Unmarshal(rootRec.Body.Bytes(), &rootResp); err != nil {
		t.Fatalf("decode root: %v", err)
	}
	rootID := rootResp.CreatedNote.ID

	provider := &restartFakeProvider{}
	runOpenWebUITurnJobFor(t, ts1, provider, rootID)
	if provider.startChatCalls != 1 {
		t.Fatalf("startChatCalls = %d, want 1", provider.startChatCalls)
	}
	reply1 := onlyChildOf(t, ts1, rootID)
	if reply1.ReplyID == nil || *reply1.ReplyID != rootID {
		t.Fatalf("reply1.ReplyID = %v, want %q", reply1.ReplyID, rootID)
	}

	followUpRec := ts1.post(t, "/api/notes/create", map[string]any{"text": "owner follow-up", "replyId": reply1.ID})
	if followUpRec.Code != http.StatusOK {
		t.Fatalf("create follow-up note: %d %s", followUpRec.Code, followUpRec.Body.String())
	}
	var followUpResp createdNoteResponse
	if err := json.Unmarshal(followUpRec.Body.Bytes(), &followUpResp); err != nil {
		t.Fatalf("decode follow-up: %v", err)
	}
	followUpID := followUpResp.CreatedNote.ID

	runOpenWebUITurnJobFor(t, ts1, provider, followUpID)
	if provider.startChatCalls != 1 || provider.continueTurnCalls != 1 {
		t.Fatalf("after follow-up: startChatCalls=%d continueTurnCalls=%d, want 1,1", provider.startChatCalls, provider.continueTurnCalls)
	}
	reply2 := onlyChildOf(t, ts1, followUpID)
	if reply2.ReplyID == nil || *reply2.ReplyID != followUpID {
		t.Fatalf("reply2.ReplyID = %v, want %q", reply2.ReplyID, followUpID)
	}

	linksBeforeRestart, err := ts1.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(linksBeforeRestart) != 1 || linksBeforeRestart[0].State != domain.LinkReady {
		t.Fatalf("links before restart = %+v, want exactly 1 ready link", linksBeforeRestart)
	}
	linkID := linksBeforeRestart[0].ID
	if linksBeforeRestart[0].RemoteChatID == nil || *linksBeforeRestart[0].RemoteChatID != "remote-chat-restart-1" {
		t.Fatalf("link.RemoteChatID = %v, want %q", linksBeforeRestart[0].RemoteChatID, "remote-chat-restart-1")
	}

	// Simulate the process going away entirely: close the first
	// harness's own database handle before anything from the second
	// harness is built, so nothing here can be reading through the first
	// harness's connection by accident.
	if err := ts1.db.Close(); err != nil {
		t.Fatalf("close first harness database: %v", err)
	}

	// --- Second "process": an independent harness against the same
	// file, verifying the chain survived and continuing it once more. ---
	ts2 := newNoteAPITestServerOpenWebUIEnabledAt(t, dbPath, "note-api-restart-2")

	if ts2.ownerID != ts1.ownerID {
		t.Fatalf("ts2 owner actor id = %q, want the same owner actor id %q as before restart", ts2.ownerID, ts1.ownerID)
	}

	restartedReply1 := onlyChildOf(t, ts2, rootID)
	if restartedReply1.ID != reply1.ID {
		t.Fatalf("reply1 after restart = %q, want the same entry id %q", restartedReply1.ID, reply1.ID)
	}
	restartedReply2 := onlyChildOf(t, ts2, followUpID)
	if restartedReply2.ID != reply2.ID {
		t.Fatalf("reply2 after restart = %q, want the same entry id %q", restartedReply2.ID, reply2.ID)
	}

	linksAfterRestart, err := ts2.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(linksAfterRestart) != 1 || linksAfterRestart[0].ID != linkID {
		t.Fatalf("links after restart = %+v, want exactly the same one link %q as before restart", linksAfterRestart, linkID)
	}
	if linksAfterRestart[0].State != domain.LinkReady {
		t.Fatalf("link state after restart = %q, want %q", linksAfterRestart[0].State, domain.LinkReady)
	}
	if linksAfterRestart[0].RemoteChatID == nil || *linksAfterRestart[0].RemoteChatID != "remote-chat-restart-1" {
		t.Fatalf("link.RemoteChatID after restart = %v, want the same remote chat %q as before restart", linksAfterRestart[0].RemoteChatID, "remote-chat-restart-1")
	}

	// A further owner follow-up after restart must still continue the
	// very same branch/remote chat, not start a new one.
	followUp2Rec := ts2.post(t, "/api/notes/create", map[string]any{"text": "owner follow-up after restart", "replyId": reply2.ID})
	if followUp2Rec.Code != http.StatusOK {
		t.Fatalf("create post-restart follow-up note: %d %s", followUp2Rec.Code, followUp2Rec.Body.String())
	}
	var followUp2Resp createdNoteResponse
	if err := json.Unmarshal(followUp2Rec.Body.Bytes(), &followUp2Resp); err != nil {
		t.Fatalf("decode post-restart follow-up: %v", err)
	}
	followUp2ID := followUp2Resp.CreatedNote.ID

	runOpenWebUITurnJobFor(t, ts2, provider, followUp2ID)
	if provider.startChatCalls != 1 || provider.continueTurnCalls != 2 {
		t.Fatalf("after post-restart follow-up: startChatCalls=%d continueTurnCalls=%d, want 1,2 (no new remote chat)", provider.startChatCalls, provider.continueTurnCalls)
	}
	reply3 := onlyChildOf(t, ts2, followUp2ID)
	if reply3.ReplyID == nil || *reply3.ReplyID != followUp2ID {
		t.Fatalf("reply3.ReplyID = %v, want %q", reply3.ReplyID, followUp2ID)
	}

	finalLinks, err := ts2.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalLinks) != 1 || finalLinks[0].ID != linkID {
		t.Fatalf("links after post-restart continuation = %+v, want still exactly the same one link %q", finalLinks, linkID)
	}
}
