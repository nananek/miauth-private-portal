package openwebui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// newTestRecoveryRegistry builds a *Registry against env's already-
// migrated database and already-seeded workspace/model, wired with a
// timeline.Service and provider the same way cmd/openwebuictl's own
// confirm subcommand wires one: recovery.go's ConfirmLink is the only
// method that needs either, so every other test passes a nil provider.
func newTestRecoveryRegistry(env *turnTestEnv, provider Provider) *Registry {
	timelineSvc := timeline.NewService(env.db, env.db.Repos, timeline.Config{Clock: env.clock})
	cfg := validRegistryConfig()
	cfg.GenerationEnabled = true
	return NewRegistry(env.db, env.db.Repos, cfg, env.clock, timelineSvc, provider)
}

// mustAmbiguousLinkWithTurn claims a link and, if remoteChatID is
// non-nil, confirms it ready (and, if preRemoteCurrentID is also
// non-nil, records that as an earlier turn's already-succeeded pointer)
// before freezing it ambiguous — so the link itself carries a known chat
// id, mirroring an uncertain *continuation*. A nil remoteChatID instead
// goes straight from creation_pending to ambiguous, mirroring a lost
// *creation* response, where the link may not know a chat id at all. It
// then records one turn on target (the owner post this turn replies to,
// typically root) with turnStatus and the given remote correlation.
func mustAmbiguousLinkWithTurn(
	t *testing.T, env *turnTestEnv, root, target domain.Entry, turnStatus domain.TurnProviderStatus, remoteChatID, preRemoteCurrentID, remoteAssistantMessageID *string,
) (domain.OpenWebUIConversationLink, domain.OpenWebUITurnLink) {
	t.Helper()
	now := env.clock.Now()
	link := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: root.ThreadID, BranchID: domain.NewID(),
		WorkspaceID: env.workspace.ID, ModelID: env.model.ID,
		ClaimedAt: now, LastTransitionAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.OpenWebUILinks.Claim(t.Context(), link); err != nil {
		t.Fatalf("claim link: %v", err)
	}
	if remoteChatID != nil {
		if err := env.db.OpenWebUILinks.MarkReady(t.Context(), link.ID, *remoteChatID, nil, now); err != nil {
			t.Fatalf("mark ready (pre-ambiguous): %v", err)
		}
		if preRemoteCurrentID != nil {
			if err := env.db.OpenWebUILinks.SetRemoteCurrent(t.Context(), link.ID, preRemoteCurrentID, now); err != nil {
				t.Fatalf("set remote current (pre-ambiguous): %v", err)
			}
		}
	}
	if err := env.db.OpenWebUILinks.MarkAmbiguous(t.Context(), link.ID, now); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
	got, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}

	turn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: target.ID, LocalParentID: target.ParentEntryID, RequestID: domain.NewID(), Revision: 1, Attempt: 1, Status: turnStatus,
		RemoteChatID: remoteChatID, RemoteAssistantMessageID: remoteAssistantMessageID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return got, turn
}

func TestConfirmLink_LookupDoneCreatesEntryMarksReadyAndNotifies(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, turn := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), nil, strPtr("remote-assistant-1"))

	provider := newFakeProvider(t)
	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		if remoteChatID != "remote-chat-1" || assistantMessageID != "remote-assistant-1" {
			t.Errorf("lookup args = %q/%q, want remote-chat-1/remote-assistant-1", remoteChatID, assistantMessageID)
		}
		return TurnOutcome{Found: true, Done: true, Content: "recovered", RemoteCurrentID: strPtr("remote-assistant-1"), PromptTokens: ptrInt(1), CompletionTokens: ptrInt(2)}, nil
	}
	reg := newTestRecoveryRegistry(env, provider)

	entry, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, nil)
	if err != nil {
		t.Fatalf("ConfirmLink: %v", err)
	}
	if entry.ID == "" || entry.Body != "recovered" {
		t.Errorf("entry = %+v, want a recovered reply", entry)
	}
	if entry.AuthorActorID != env.model.ActorID {
		t.Errorf("entry.AuthorActorID = %q, want %q", entry.AuthorActorID, env.model.ActorID)
	}

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkReady {
		t.Errorf("link.State = %q, want ready", gotLink.State)
	}
	if gotLink.RemoteCurrentID == nil || *gotLink.RemoteCurrentID != "remote-assistant-1" {
		t.Errorf("link.RemoteCurrentID = %v, want remote-assistant-1", gotLink.RemoteCurrentID)
	}

	gotTurn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTurn.Status != domain.TurnSucceeded || gotTurn.AssistantEntryID == nil || *gotTurn.AssistantEntryID != entry.ID {
		t.Errorf("turn = %+v, want succeeded with AssistantEntryID %q", gotTurn, entry.ID)
	}

	notifications, err := env.db.Notifications.ListDesc(t.Context(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].Type != domain.NotificationReply || notifications[0].RelatedEntryID != entry.ID {
		t.Errorf("notifications = %+v, want one reply notification for %q", notifications, entry.ID)
	}
}

func TestConfirmLink_LookupErrorFailsTurnLostKeepsLinkReady(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	// The link already succeeded an earlier turn (remote_current_id set)
	// before this later continuation went ambiguous, so the fix under
	// test is that a "lost" outcome must not disturb that pointer.
	link, turn := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), strPtr("remote-assistant-1"), strPtr("remote-assistant-2"))

	provider := newFakeProvider(t)
	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		return TurnOutcome{Found: true, Done: true, HasError: true}, nil
	}
	reg := newTestRecoveryRegistry(env, provider)

	entry, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, nil)
	if err != nil {
		t.Fatalf("ConfirmLink: %v", err)
	}
	if entry.ID != "" {
		t.Errorf("entry = %+v, want zero value (no reply recovered)", entry)
	}

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkReady {
		t.Errorf("link.State = %q, want ready", gotLink.State)
	}
	if gotLink.RemoteCurrentID == nil || *gotLink.RemoteCurrentID != "remote-assistant-1" {
		t.Errorf("link.RemoteCurrentID = %v, want unchanged remote-assistant-1", gotLink.RemoteCurrentID)
	}

	gotTurn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTurn.Status != domain.TurnFailed || gotTurn.FailureCategory == nil || *gotTurn.FailureCategory != domain.FailureCategoryLost {
		t.Errorf("turn = %+v, want failed/lost", gotTurn)
	}
}

func TestConfirmLink_NotFoundFailsTurnLost(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, turn := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), nil, strPtr("remote-assistant-1"))

	provider := newFakeProvider(t)
	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		return TurnOutcome{Found: false}, nil
	}
	reg := newTestRecoveryRegistry(env, provider)

	if _, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, nil); err != nil {
		t.Fatalf("ConfirmLink: %v", err)
	}
	gotTurn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTurn.Status != domain.TurnFailed || gotTurn.FailureCategory == nil || *gotTurn.FailureCategory != domain.FailureCategoryLost {
		t.Errorf("turn = %+v, want failed/lost", gotTurn)
	}
}

func TestConfirmLink_StillGeneratingReturnsErrStillAmbiguousUnchanged(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, turn := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), nil, strPtr("remote-assistant-1"))

	provider := newFakeProvider(t)
	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		return TurnOutcome{Found: true, Done: false}, nil
	}
	reg := newTestRecoveryRegistry(env, provider)

	_, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, nil)
	if !errors.Is(err, ErrStillAmbiguous) {
		t.Fatalf("ConfirmLink error = %v, want ErrStillAmbiguous", err)
	}

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkAmbiguous {
		t.Errorf("link.State = %q, want unchanged ambiguous", gotLink.State)
	}
	gotTurn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTurn.Status != domain.TurnAmbiguous {
		t.Errorf("turn.Status = %q, want unchanged ambiguous", gotTurn.Status)
	}
}

func TestConfirmLink_CreationLostNoMessageIDNeverCallsProvider(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	// No remote chat id anywhere yet: a lost creation response.
	link, turn := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, nil, nil, nil)

	provider := newFakeProvider(t) // no scripts: any call fails the test
	reg := newTestRecoveryRegistry(env, provider)

	if _, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, strPtr("owner-found-chat-1")); err != nil {
		t.Fatalf("ConfirmLink: %v", err)
	}
	if start, cont, lookup := provider.counts(); start != 0 || cont != 0 || lookup != 0 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want none", start, cont, lookup)
	}

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkReady {
		t.Errorf("link.State = %q, want ready", gotLink.State)
	}
	if gotLink.RemoteChatID == nil || *gotLink.RemoteChatID != "owner-found-chat-1" {
		t.Errorf("link.RemoteChatID = %v, want owner-found-chat-1", gotLink.RemoteChatID)
	}
	gotTurn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTurn.Status != domain.TurnFailed || gotTurn.FailureCategory == nil || *gotTurn.FailureCategory != domain.FailureCategoryCreationLost {
		t.Errorf("turn = %+v, want failed/creation_lost", gotTurn)
	}
}

func TestConfirmLink_NoChatIDReturnsErrRecoveryNeedsChatID(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, _ := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, nil, nil, nil)

	provider := newFakeProvider(t)
	reg := newTestRecoveryRegistry(env, provider)

	if _, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, nil); !errors.Is(err, ErrRecoveryNeedsChatID) {
		t.Fatalf("ConfirmLink error = %v, want ErrRecoveryNeedsChatID", err)
	}
}

func TestConfirmLink_ConflictingChatIDReturnsErrConflict(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, _ := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), nil, strPtr("remote-assistant-1"))

	provider := newFakeProvider(t)
	reg := newTestRecoveryRegistry(env, provider)

	if _, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, strPtr("remote-chat-2")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ConfirmLink error = %v, want domain.ErrConflict", err)
	}
}

func TestConfirmLink_NonAmbiguousLinkReturnsErrInvalidLinkTransition(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link := env.mustReadyLink(t, root.ThreadID)

	reg := newTestRecoveryRegistry(env, newFakeProvider(t))
	if _, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, nil); !errors.Is(err, domain.ErrInvalidLinkTransition) {
		t.Fatalf("ConfirmLink error = %v, want domain.ErrInvalidLinkTransition", err)
	}
}

func TestConfirmLink_NotOwnerReturnsErrNotOwner(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, _ := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), nil, strPtr("remote-assistant-1"))

	reg := newTestRecoveryRegistry(env, newFakeProvider(t))
	if _, err := reg.ConfirmLink(t.Context(), env.model.ActorID, link.ID, nil); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("ConfirmLink error = %v, want ErrNotOwner", err)
	}
}

func TestConfirmLink_DisabledReturnsErrDisabled(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, _ := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), nil, strPtr("remote-assistant-1"))

	timelineSvc := timeline.NewService(env.db, env.db.Repos, timeline.Config{Clock: env.clock})
	cfg := validRegistryConfig()
	cfg.Enabled = false
	reg := NewRegistry(env.db, env.db.Repos, cfg, env.clock, timelineSvc, newFakeProvider(t))

	if _, err := reg.ConfirmLink(t.Context(), env.ownerID, link.ID, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("ConfirmLink error = %v, want ErrDisabled", err)
	}
}

func TestAbandonLink_MarksDeadAndConcludesOpenTurnsOnly(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link := env.mustReadyLink(t, root.ThreadID)

	// An earlier turn already succeeded on this link (requires the link
	// to still be ready when it is recorded — SetRemoteCurrent's own
	// compare-and-set — so this must happen before the link goes
	// ambiguous below) and must be left exactly as it is.
	reply := env.mustCreateReplyAs(t, root, env.model.ActorID, domain.EntryLLMReply, "a0")
	succeeded := env.mustSucceededTurn(t, link, root, reply)

	// A later reply's turn is still open when the link goes ambiguous.
	now := env.clock.Now()
	m1 := env.mustCreateReply(t, reply, "m1")
	if err := env.db.OpenWebUILinks.MarkAmbiguous(t.Context(), link.ID, now); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
	openTurn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: m1.ID, LocalParentID: m1.ParentEntryID, RequestID: domain.NewID(), Revision: 1, Attempt: 1, Status: domain.TurnPending,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.OpenWebUITurnLinks.Create(t.Context(), openTurn); err != nil {
		t.Fatalf("create open turn: %v", err)
	}

	reg := newTestRecoveryRegistry(env, nil)
	if err := reg.AbandonLink(t.Context(), env.ownerID, link.ID); err != nil {
		t.Fatalf("AbandonLink: %v", err)
	}

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkDead {
		t.Errorf("link.State = %q, want dead", gotLink.State)
	}
	if gotLink.FailureCategory == nil || *gotLink.FailureCategory != domain.FailureCategoryOwnerAbandoned {
		t.Errorf("link.FailureCategory = %v, want owner_abandoned", gotLink.FailureCategory)
	}

	gotOpen, err := env.db.OpenWebUITurnLinks.Get(t.Context(), openTurn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOpen.Status != domain.TurnCancelled || gotOpen.TombstonedAt == nil {
		t.Errorf("open turn = %+v, want cancelled and tombstoned", gotOpen)
	}

	gotSucceeded, err := env.db.OpenWebUITurnLinks.Get(t.Context(), succeeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSucceeded.Status != domain.TurnSucceeded || gotSucceeded.TombstonedAt != nil {
		t.Errorf("succeeded turn = %+v, want left unchanged", gotSucceeded)
	}
}

func TestAbandonLink_NonAmbiguousReturnsErrInvalidLinkTransition(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link := env.mustReadyLink(t, root.ThreadID)

	reg := newTestRecoveryRegistry(env, nil)
	if err := reg.AbandonLink(t.Context(), env.ownerID, link.ID); !errors.Is(err, domain.ErrInvalidLinkTransition) {
		t.Fatalf("AbandonLink error = %v, want domain.ErrInvalidLinkTransition", err)
	}
}

func TestFreezeLink_DeadClaimJobMarksAmbiguous(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	now := env.clock.Now()

	job := domain.Job{ID: domain.NewID(), JobType: JobType, Payload: "{}", PayloadVersion: 1, State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := env.db.Jobs.Enqueue(t.Context(), job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if _, err := env.db.Jobs.Claim(t.Context(), "worker-1", 10, now, now.Add(time.Minute)); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if err := env.db.Jobs.Kill(t.Context(), job.ID, "worker-1", "boom", now); err != nil {
		t.Fatalf("kill job: %v", err)
	}

	link := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: root.ThreadID, BranchID: domain.NewID(),
		WorkspaceID: env.workspace.ID, ModelID: env.model.ID, ClaimJobID: &job.ID,
		ClaimedAt: now, LastTransitionAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.OpenWebUILinks.Claim(t.Context(), link); err != nil {
		t.Fatalf("claim link: %v", err)
	}

	reg := newTestRecoveryRegistry(env, nil)
	if err := reg.FreezeLink(t.Context(), env.ownerID, link.ID); err != nil {
		t.Fatalf("FreezeLink: %v", err)
	}
	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkAmbiguous {
		t.Errorf("link.State = %q, want ambiguous", gotLink.State)
	}
}

// TestFreezeLink_SucceededClaimJobMarksAmbiguous covers the gap a
// review found in the first cut of this method: a succeeded claim job
// usually means its StartChat already moved the link to ready, but not
// always — if RecordOutcome durably recorded the turn's outcome and only
// the very next call (MarkAmbiguous/MarkFailed) then failed for a
// transient, non-conflict reason, the job still finishes succeeded (a
// later redelivery finds the turn already terminal and returns nil)
// while the link is left stuck creation_pending with no automatic way
// out. A succeeded claim job must be just as freezable as a dead or
// failed one, since in every case it will never call the provider
// again.
func TestFreezeLink_SucceededClaimJobMarksAmbiguous(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	now := env.clock.Now()

	job := domain.Job{ID: domain.NewID(), JobType: JobType, Payload: "{}", PayloadVersion: 1, State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := env.db.Jobs.Enqueue(t.Context(), job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if _, err := env.db.Jobs.Claim(t.Context(), "worker-1", 10, now, now.Add(time.Minute)); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if err := env.db.Jobs.Succeed(t.Context(), job.ID, "worker-1", now); err != nil {
		t.Fatalf("succeed job: %v", err)
	}

	link := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: root.ThreadID, BranchID: domain.NewID(),
		WorkspaceID: env.workspace.ID, ModelID: env.model.ID, ClaimJobID: &job.ID,
		ClaimedAt: now, LastTransitionAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.OpenWebUILinks.Claim(t.Context(), link); err != nil {
		t.Fatalf("claim link: %v", err)
	}

	reg := newTestRecoveryRegistry(env, nil)
	if err := reg.FreezeLink(t.Context(), env.ownerID, link.ID); err != nil {
		t.Fatalf("FreezeLink: %v", err)
	}
	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkAmbiguous {
		t.Errorf("link.State = %q, want ambiguous", gotLink.State)
	}
}

// TestFreezeLink_NoClaimJobMarksAmbiguous covers a link with no
// ClaimJobID at all (a nil job id skips the FK the schema places on
// claim_job_id, unlike a dangling id pointing at a row that was never
// inserted, which the FK itself refuses to let this test construct):
// FreezeLink's job check is skipped entirely, matching a link this old
// or otherwise never wired to a job row.
func TestFreezeLink_NoClaimJobMarksAmbiguous(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	now := env.clock.Now()

	link := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: root.ThreadID, BranchID: domain.NewID(),
		WorkspaceID: env.workspace.ID, ModelID: env.model.ID,
		ClaimedAt: now, LastTransitionAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.OpenWebUILinks.Claim(t.Context(), link); err != nil {
		t.Fatalf("claim link: %v", err)
	}

	reg := newTestRecoveryRegistry(env, nil)
	if err := reg.FreezeLink(t.Context(), env.ownerID, link.ID); err != nil {
		t.Fatalf("FreezeLink: %v", err)
	}
	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkAmbiguous {
		t.Errorf("link.State = %q, want ambiguous", gotLink.State)
	}
}

func TestFreezeLink_PendingOrRunningClaimJobRejected(t *testing.T) {
	env := newTurnTestEnv(t)

	for _, tc := range []struct {
		name  string
		claim bool
	}{
		{name: "pending", claim: false},
		{name: "running", claim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := env.mustCreateRoot(t, "hello "+tc.name)
			now := env.clock.Now()
			job := domain.Job{ID: domain.NewID(), JobType: JobType, Payload: "{}", PayloadVersion: 1, State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if err := env.db.Jobs.Enqueue(t.Context(), job); err != nil {
				t.Fatalf("enqueue job: %v", err)
			}
			if tc.claim {
				if _, err := env.db.Jobs.Claim(t.Context(), "worker-1", 10, now, now.Add(time.Minute)); err != nil {
					t.Fatalf("claim job: %v", err)
				}
			}

			link := domain.OpenWebUIConversationLink{
				ID: domain.NewID(), ThreadID: root.ThreadID, BranchID: domain.NewID(),
				WorkspaceID: env.workspace.ID, ModelID: env.model.ID, ClaimJobID: &job.ID,
				ClaimedAt: now, LastTransitionAt: now, CreatedAt: now, UpdatedAt: now,
			}
			if err := env.db.OpenWebUILinks.Claim(t.Context(), link); err != nil {
				t.Fatalf("claim link: %v", err)
			}

			reg := newTestRecoveryRegistry(env, nil)
			if err := reg.FreezeLink(t.Context(), env.ownerID, link.ID); !errors.Is(err, ErrClaimJobStillActive) {
				t.Fatalf("FreezeLink error = %v, want ErrClaimJobStillActive", err)
			}
			gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotLink.State != domain.LinkCreationPending {
				t.Errorf("link.State = %q, want unchanged creation_pending", gotLink.State)
			}
		})
	}
}

func TestFreezeLink_NonCreationPendingReturnsErrInvalidLinkTransition(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link := env.mustReadyLink(t, root.ThreadID)

	reg := newTestRecoveryRegistry(env, nil)
	if err := reg.FreezeLink(t.Context(), env.ownerID, link.ID); !errors.Is(err, domain.ErrInvalidLinkTransition) {
		t.Fatalf("FreezeLink error = %v, want domain.ErrInvalidLinkTransition", err)
	}
}

func TestListLinks_FiltersByState(t *testing.T) {
	env := newTurnTestEnv(t)
	root1 := env.mustCreateRoot(t, "one")
	root2 := env.mustCreateRoot(t, "two")
	readyLink := env.mustReadyLink(t, root1.ThreadID)
	ambiguousLink, _ := mustAmbiguousLinkWithTurn(t, env, root2, root2, domain.TurnAmbiguous, strPtr("remote-chat-2"), nil, strPtr("remote-assistant-2"))

	reg := newTestRecoveryRegistry(env, nil)
	state := domain.LinkAmbiguous
	links, err := reg.ListLinks(t.Context(), env.ownerID, domain.OpenWebUILinkFilter{State: &state})
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(links) != 1 || links[0].ID != ambiguousLink.ID {
		t.Errorf("links = %+v, want exactly [%s]", links, ambiguousLink.ID)
	}
	_ = readyLink
}

func TestDescribeLink_ReturnsLinkAndTurns(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")
	link, turn := mustAmbiguousLinkWithTurn(t, env, root, root, domain.TurnAmbiguous, strPtr("remote-chat-1"), nil, strPtr("remote-assistant-1"))

	reg := newTestRecoveryRegistry(env, nil)
	gotLink, turns, err := reg.DescribeLink(t.Context(), env.ownerID, link.ID)
	if err != nil {
		t.Fatalf("DescribeLink: %v", err)
	}
	if gotLink.ID != link.ID {
		t.Errorf("gotLink.ID = %q, want %q", gotLink.ID, link.ID)
	}
	if len(turns) != 1 || turns[0].ID != turn.ID {
		t.Errorf("turns = %+v, want exactly [%s]", turns, turn.ID)
	}
}
