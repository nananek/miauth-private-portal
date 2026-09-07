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

// TestOpenWebUIAmbiguity_ChatCreationResponseLossNeverAutoRetriesOrDuplicatesChat
// is Issue #54 (OWUI-R)'s ambiguity acceptance criterion: a lost
// chat-creation response freezes the link ambiguous, and no later
// redelivery of the same job is ever allowed to call StartChat again or
// project a reply on its own — recovery is deliberately not automatic
// (ADR-0005 D7; internal/openwebui's own
// TestTurnJob_StartChat_TimeoutFreezesLinkAmbiguousAndNeverRecreates
// covers this at the unit level).
func TestOpenWebUIAmbiguity_ChatCreationResponseLossNeverAutoRetriesOrDuplicatesChat(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)

	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ask the model during a creation timeout"})
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
			return openwebui.TurnResult{}, openwebui.NewProviderError(openwebui.CategoryTimeout, openwebui.PhaseCreate, context.DeadlineExceeded)
		},
	}
	turnJob := findOpenWebUITurnJobFor(t, ts, rootID)
	handler := openwebui.NewTurnJob(ts.db.Repos, ts.timeline, provider, nil, nil, openwebui.TurnJobConfig{MaxAttempts: 8, MaxContextMessages: 100}, ts.clock, nil)

	err := handler.Handle(t.Context(), turnJob)
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("first Handle error = %v, want a jobs.PermanentError", err)
	}

	links, err := ts.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].State != domain.LinkAmbiguous {
		t.Fatalf("links after first delivery = %+v, want exactly 1 ambiguous link", links)
	}

	// A later redelivery of the very same job must not call StartChat
	// again: the turn is already terminal (ambiguous).
	if err := handler.Handle(t.Context(), turnJob); err != nil {
		t.Fatalf("second Handle: %v, want nil (duplicate delivery of an already-terminal turn)", err)
	}
	if provider.startChatCalls != 1 {
		t.Fatalf("startChatCalls across both deliveries = %d, want exactly 1", provider.startChatCalls)
	}

	if children := notesChildren(t, ts, rootID); len(children) != 0 {
		t.Fatalf("children = %v, want none: an ambiguous link never auto-produces a reply", children)
	}
}

// TestOpenWebUIAmbiguity_OwnerConfirmLinkIsTheOnlyWayToRecoverAReply is
// Issue #54 (OWUI-R)'s owner-recovery acceptance criterion: once a chat
// creation's response is lost and the link is frozen ambiguous (as in
// TestOpenWebUIAmbiguity_ChatCreationResponseLossNeverAutoRetriesOrDuplicatesChat
// above), the only way a reply ever appears is the owner's explicit
// openwebui.Registry.ConfirmLink call — nothing HTTP-reachable resolves
// it on its own (ADR-0005/roadmap: "Manual resolution ... is an explicit
// owner/operator action", CLI-only via cmd/openwebuictl; recovery.go's
// ConfirmLink has no HTTP route at all).
//
// The link's own remote chat id is unknown at this point — StartChat
// never reached OnChatCreated — so the owner supplies the chat id they
// found out-of-band (an Open WebUI admin UI, an operator's own note)
// rather than passing nil. That matches ConfirmLink's own documented
// contract (internal/openwebui/recovery.go) and
// TestConfirmLink_LookupDoneCreatesEntryMarksReadyAndNotifies: the turn's
// client-generated assistant-message id was already durably recorded
// before the failed StartChat call (turnjob.go's handleCreationPending
// persists it via setCorrelation before ever calling the provider), so
// this is not the "no message id was ever generated" branch
// TestConfirmLink_CreationLostNoMessageIDNeverCallsProvider covers —
// passing nil here would only return ErrRecoveryNeedsChatID instead of
// exercising the LookupTurnOutcome recovery path this test is for.
func TestOpenWebUIAmbiguity_OwnerConfirmLinkIsTheOnlyWayToRecoverAReply(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)

	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ask the model during a creation timeout"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rootID := resp.CreatedNote.ID

	lossyProvider := &e2eFakeProvider{
		startChat: func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
			return openwebui.TurnResult{}, openwebui.NewProviderError(openwebui.CategoryTimeout, openwebui.PhaseCreate, context.DeadlineExceeded)
		},
	}
	if err := runOpenWebUITurnJobForIgnoringError(t, ts, lossyProvider, rootID); err == nil {
		t.Fatalf("Handle: want a non-nil error freezing the link ambiguous")
	}

	links, err := ts.db.OpenWebUILinks.ListByThread(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].State != domain.LinkAmbiguous {
		t.Fatalf("links before recovery = %+v, want exactly 1 ambiguous link", links)
	}
	linkID := links[0].ID

	if children := notesChildren(t, ts, rootID); len(children) != 0 {
		t.Fatalf("children before recovery = %v, want none", children)
	}

	recoveryProvider := &e2eFakeProvider{
		lookupOutcome: func(ctx context.Context, remoteChatID, assistantMessageID string) (openwebui.TurnOutcome, error) {
			return openwebui.TurnOutcome{Found: true, Done: true, Content: "recovered reply", RemoteCurrentID: strPtrForTest("remote-msg-recovered")}, nil
		},
	}
	recoveryReg := openwebui.NewRegistry(ts.db, ts.db.Repos, openwebui.RegistryConfig{Enabled: true}, ts.clock, ts.timeline, recoveryProvider)

	entry, err := recoveryReg.ConfirmLink(t.Context(), ts.ownerID, linkID, strPtrForTest("owner-found-chat-1"))
	if err != nil {
		t.Fatalf("ConfirmLink: %v", err)
	}
	if entry.ID == "" || entry.Body != "recovered reply" {
		t.Fatalf("ConfirmLink entry = %+v, want a recovered reply", entry)
	}
	if recoveryProvider.lookupCalls != 1 {
		t.Fatalf("lookupCalls = %d, want exactly 1", recoveryProvider.lookupCalls)
	}

	children := notesChildren(t, ts, rootID)
	if len(children) != 1 || children[0].ID != entry.ID {
		t.Fatalf("children after recovery = %v, want exactly 1 matching the recovered entry %q", children, entry.ID)
	}
}
