package openwebui

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// fakeProvider is a scripted, call-recording Provider for TurnJob tests.
// Each method delegates to an optional func field; a nil one fails the
// test loudly rather than silently returning a zero value, so a test
// that forgets to script a call it expected finds out immediately.
type fakeProvider struct {
	t *testing.T

	mu             sync.Mutex
	startChatCalls int
	continueCalls  int
	lookupCalls    int

	inFlight    int32
	maxInFlight int32

	startChat     func(ctx context.Context, req StartChatRequest) (TurnResult, error)
	continueTurn  func(ctx context.Context, req ContinueTurnRequest) (TurnResult, error)
	lookupOutcome func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error)
}

func newFakeProvider(t *testing.T) *fakeProvider { return &fakeProvider{t: t} }

func (f *fakeProvider) enter() func() {
	n := atomic.AddInt32(&f.inFlight, 1)
	for {
		max := atomic.LoadInt32(&f.maxInFlight)
		if n <= max || atomic.CompareAndSwapInt32(&f.maxInFlight, max, n) {
			break
		}
	}
	return func() { atomic.AddInt32(&f.inFlight, -1) }
}

func (f *fakeProvider) StartChat(ctx context.Context, req StartChatRequest) (TurnResult, error) {
	leave := f.enter()
	defer leave()
	f.mu.Lock()
	f.startChatCalls++
	f.mu.Unlock()
	if f.startChat == nil {
		f.t.Fatalf("fakeProvider: unexpected StartChat call")
	}
	return f.startChat(ctx, req)
}

func (f *fakeProvider) ContinueTurn(ctx context.Context, req ContinueTurnRequest) (TurnResult, error) {
	leave := f.enter()
	defer leave()
	f.mu.Lock()
	f.continueCalls++
	f.mu.Unlock()
	if f.continueTurn == nil {
		f.t.Fatalf("fakeProvider: unexpected ContinueTurn call")
	}
	return f.continueTurn(ctx, req)
}

func (f *fakeProvider) LookupTurnOutcome(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
	leave := f.enter()
	defer leave()
	f.mu.Lock()
	f.lookupCalls++
	f.mu.Unlock()
	if f.lookupOutcome == nil {
		f.t.Fatalf("fakeProvider: unexpected LookupTurnOutcome call")
	}
	return f.lookupOutcome(ctx, remoteChatID, assistantMessageID)
}

func (f *fakeProvider) counts() (start, cont, lookup int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startChatCalls, f.continueCalls, f.lookupCalls
}

func ptrInt(v int) *int { return &v }

// mustSoleJob returns the single pending job env's database holds,
// failing the test if there is not exactly one.
func mustSoleJob(t *testing.T, env *turnTestEnv) domain.Job {
	t.Helper()
	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 1 {
		t.Fatalf("jobs = %v, want exactly 1", jobRows)
	}
	return jobRows[0]
}

func mustTurnJobPayload(t *testing.T, job domain.Job) turnJobPayload {
	t.Helper()
	var payload turnJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("decode job payload: %v", err)
	}
	return payload
}

func newTestTurnJob(env *turnTestEnv, provider Provider, cfg TurnJobConfig) (*TurnJob, *timeline.Service) {
	timelineSvc := timeline.NewService(env.db, env.db.Repos, timeline.Config{Clock: env.clock})
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.MaxContextMessages == 0 {
		cfg.MaxContextMessages = 100
	}
	return NewTurnJob(env.db.Repos, timelineSvc, provider, cfg, env.clock, nil), timelineSvc
}

func TestTurnJob_StartChat_SuccessCreatesReplyAndNotification(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello model")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	provider := newFakeProvider(t)
	provider.startChat = func(ctx context.Context, req StartChatRequest) (TurnResult, error) {
		if len(req.Messages) != 0 {
			t.Errorf("StartChat Messages = %v, want empty for a root post", req.Messages)
		}
		if req.NewTurn.Content != "hello model" {
			t.Errorf("StartChat NewTurn = %+v", req.NewTurn)
		}
		if err := req.OnChatCreated(ctx, "remote-chat-1"); err != nil {
			return TurnResult{}, err
		}
		return TurnResult{Content: "hi there", RemoteCurrentID: strPtr("remote-msg-1"), PromptTokens: ptrInt(3), CompletionTokens: ptrInt(5)}, nil
	}

	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if start, cont, lookup := provider.counts(); start != 1 || cont != 0 || lookup != 0 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want 1/0/0", start, cont, lookup)
	}

	children, err := env.db.Entries.ListChildren(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %v, want exactly 1", children)
	}
	reply := children[0]
	if reply.AuthorActorID != env.model.ActorID {
		t.Errorf("reply.AuthorActorID = %q, want %q", reply.AuthorActorID, env.model.ActorID)
	}
	if reply.Body != "hi there" {
		t.Errorf("reply.Body = %q, want %q", reply.Body, "hi there")
	}

	notifications, err := env.db.Notifications.ListDesc(t.Context(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].Type != domain.NotificationReply || notifications[0].RelatedEntryID != reply.ID {
		t.Errorf("notifications = %+v, want one reply notification for %q", notifications, reply.ID)
	}

	link, err := env.db.OpenWebUILinks.Get(t.Context(), payload.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if link.State != domain.LinkReady {
		t.Errorf("link.State = %q, want ready", link.State)
	}
	if link.RemoteChatID == nil || *link.RemoteChatID != "remote-chat-1" {
		t.Errorf("link.RemoteChatID = %v, want remote-chat-1", link.RemoteChatID)
	}
	if link.RemoteCurrentID == nil || *link.RemoteCurrentID != "remote-msg-1" {
		t.Errorf("link.RemoteCurrentID = %v, want remote-msg-1", link.RemoteCurrentID)
	}
	workspace, err := env.db.OpenWebUIWorkspaces.Get(t.Context(), link.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.ChatCreateStatus != domain.CapabilityVerified {
		t.Errorf("workspace.ChatCreateStatus = %q, want verified", workspace.ChatCreateStatus)
	}

	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnSucceeded {
		t.Errorf("turn.Status = %q, want succeeded", turn.Status)
	}
	if turn.AssistantEntryID == nil || *turn.AssistantEntryID != reply.ID {
		t.Errorf("turn.AssistantEntryID = %v, want %q", turn.AssistantEntryID, reply.ID)
	}
	if turn.PromptTokens == nil || *turn.PromptTokens != 3 || turn.CompletionTokens == nil || *turn.CompletionTokens != 5 {
		t.Errorf("turn tokens = %v/%v, want 3/5", turn.PromptTokens, turn.CompletionTokens)
	}
	// The turn's own remote_chat_id must survive complete()'s later
	// SetRemoteCorrelation call, which replaces all five correlation
	// columns at once rather than merging: it must be seeded from what
	// OnChatCreated already persisted, not from a stale in-memory turn
	// value that never learned the chat id.
	if turn.RemoteChatID == nil || *turn.RemoteChatID != "remote-chat-1" {
		t.Errorf("turn.RemoteChatID = %v, want remote-chat-1", turn.RemoteChatID)
	}
}

func TestTurnJob_StartChat_StripsMentionTagsFromProviderContentButKeepsEntryBodyIntact(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hey @luna@ai.tail2c8c7.ts.net, how are you?")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)

	provider := newFakeProvider(t)
	provider.startChat = func(ctx context.Context, req StartChatRequest) (TurnResult, error) {
		if req.NewTurn.Content != "hey , how are you?" {
			t.Errorf("StartChat NewTurn.Content = %q, want mention omitted", req.NewTurn.Content)
		}
		if err := req.OnChatCreated(ctx, "remote-chat-1"); err != nil {
			return TurnResult{}, err
		}
		return TurnResult{Content: "fine", RemoteCurrentID: strPtr("remote-msg-1")}, nil
	}

	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	stored, err := env.db.Entries.Get(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Body != "hey @luna@ai.tail2c8c7.ts.net, how are you?" {
		t.Errorf("stored Entry.Body = %q, want the original unmodified mention", stored.Body)
	}
}

func TestTurnJob_DuplicateDelivery_NeverCallsProvider(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	now := env.clock.Now()
	category := domain.FailureCategoryAuthFailed
	if err := env.db.OpenWebUITurnLinks.RecordOutcome(t.Context(), payload.TurnID, domain.TurnOutcomeRecord{
		Status: domain.TurnFailed, FailureCategory: &category,
	}, now); err != nil {
		t.Fatalf("record outcome: %v", err)
	}

	provider := newFakeProvider(t) // no scripts: a call fails the test
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v, want nil for an already-terminal turn", err)
	}
	if start, cont, lookup := provider.counts(); start != 0 || cont != 0 || lookup != 0 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want none", start, cont, lookup)
	}
}

func TestTurnJob_StartChat_AuthFailed_PermanentAndMarksLinkFailed(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	provider := newFakeProvider(t)
	provider.startChat = func(ctx context.Context, req StartChatRequest) (TurnResult, error) {
		return TurnResult{}, NewProviderError(CategoryAuthFailed, PhaseCreate, errors.New("401"))
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	err := turnJob.Handle(t.Context(), job)
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("Handle error = %v, want a jobs.PermanentError", err)
	}

	link, err := env.db.OpenWebUILinks.Get(t.Context(), payload.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if link.State != domain.LinkFailed {
		t.Errorf("link.State = %q, want failed", link.State)
	}
	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnFailed || turn.FailureCategory == nil || *turn.FailureCategory != domain.FailureCategoryAuthFailed {
		t.Errorf("turn = %+v, want failed/auth_failed", turn)
	}
}

// TestTurnJob_StartChat_TurnPhaseFailureAfterCreation_RetriesRatherThanFreezing
// covers a case the plan's §5.4 table treats very differently from a
// creation failure: StartChat bundles createChat and the chat's first
// runTurn into one call, so a transient failure coming back from it can
// belong to either phase. Once OnChatCreated has already run (the chat
// exists and the link is ready in storage), the failure is the turn's,
// not the chat's, and must get the same bounded-retry treatment a ready
// link's continuation gets — never an immediate, permanent ambiguous
// freeze on the very first attempt (that treatment is reserved for a
// genuine creation failure, where the chat's existence itself is
// unknown).
func TestTurnJob_StartChat_TurnPhaseFailureAfterCreation_RetriesRatherThanFreezing(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	provider := newFakeProvider(t)
	provider.startChat = func(ctx context.Context, req StartChatRequest) (TurnResult, error) {
		if err := req.OnChatCreated(ctx, "remote-chat-1"); err != nil {
			return TurnResult{}, err
		}
		// The chat now exists and the link has already been marked
		// ready by OnChatCreated; this failure is the completions call
		// that follows it, i.e. a turn-phase error, not a create-phase
		// one.
		return TurnResult{}, NewProviderError(CategoryServerError, PhaseTurn, errors.New("500"))
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	err := turnJob.Handle(t.Context(), job)
	var permanent *jobs.PermanentError
	if errors.As(err, &permanent) {
		t.Fatalf("Handle error = %v, want a retryable (non-Permanent) error: a transient failure after a successful chat creation must not be treated as an unrecoverable creation loss", err)
	}
	if err == nil {
		t.Fatal("Handle error = nil, want a retryable error for the failed turn")
	}

	link, err := env.db.OpenWebUILinks.Get(t.Context(), payload.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if link.State != domain.LinkReady {
		t.Errorf("link.State = %q, want ready (chat creation succeeded; only the turn failed)", link.State)
	}
	if link.RemoteChatID == nil || *link.RemoteChatID != "remote-chat-1" {
		t.Errorf("link.RemoteChatID = %v, want remote-chat-1", link.RemoteChatID)
	}

	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status.IsTerminal() {
		t.Errorf("turn.Status = %q, want non-terminal (still retryable)", turn.Status)
	}
	if turn.RemoteChatID == nil || *turn.RemoteChatID != "remote-chat-1" {
		t.Errorf("turn.RemoteChatID = %v, want remote-chat-1", turn.RemoteChatID)
	}
	if start, _, _ := provider.counts(); start != 1 {
		t.Errorf("StartChat calls = %d, want exactly 1", start)
	}
}

func TestTurnJob_StartChat_TimeoutFreezesLinkAmbiguousAndNeverRecreates(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	provider := newFakeProvider(t)
	provider.startChat = func(ctx context.Context, req StartChatRequest) (TurnResult, error) {
		return TurnResult{}, NewProviderError(CategoryTimeout, PhaseCreate, context.DeadlineExceeded)
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	err := turnJob.Handle(t.Context(), job)
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("Handle error = %v, want a jobs.PermanentError (create is never retried)", err)
	}

	link, err := env.db.OpenWebUILinks.Get(t.Context(), payload.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if link.State != domain.LinkAmbiguous {
		t.Errorf("link.State = %q, want ambiguous", link.State)
	}
	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnAmbiguous || turn.FailureCategory == nil || *turn.FailureCategory != domain.FailureCategoryCreationLost {
		t.Errorf("turn = %+v, want ambiguous/creation_lost", turn)
	}

	// A second delivery of the same job must not call StartChat again: the
	// turn is already terminal (ambiguous), so Handle takes the
	// duplicate-delivery no-op path.
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("second Handle: %v, want nil (duplicate delivery)", err)
	}
	if start, _, _ := provider.counts(); start != 1 {
		t.Errorf("StartChat calls = %d, want exactly 1 across both deliveries", start)
	}
}

func TestTurnJob_ContinueTurn_Success(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	bridge := newTestBridge(env)
	m1 := env.mustCreateReply(t, a0, "m1")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var job domain.Job
	for _, j := range jobRows {
		p := mustTurnJobPayload(t, j)
		if p.LinkID == link.ID {
			var payload turnJobPayload
			if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			// Skip the seeded turn's own job row if any exists; only the
			// bridge-enqueued m1 job should be pending.
			if j.State == domain.JobPending {
				job = j
			}
		}
	}
	if job.ID == "" {
		t.Fatalf("no pending job found among %v", jobRows)
	}
	payload := mustTurnJobPayload(t, job)

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantParent := gotLink.RemoteCurrentID

	provider := newFakeProvider(t)
	provider.continueTurn = func(ctx context.Context, req ContinueTurnRequest) (TurnResult, error) {
		if link.RemoteChatID == nil || req.RemoteChatID != *link.RemoteChatID {
			t.Errorf("RemoteChatID = %q, want %v", req.RemoteChatID, link.RemoteChatID)
		}
		if wantParent == nil || req.IDs.ParentAssistantID == nil || *req.IDs.ParentAssistantID != *wantParent {
			t.Errorf("ParentAssistantID = %v, want %v", req.IDs.ParentAssistantID, wantParent)
		}
		if len(req.Messages) != 2 {
			t.Errorf("Messages = %v, want 2 (m0, a0)", req.Messages)
		}
		return TurnResult{Content: "a1", RemoteCurrentID: strPtr("remote-msg-a1")}, nil
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if start, cont, lookup := provider.counts(); start != 0 || cont != 1 || lookup != 0 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want 0/1/0", start, cont, lookup)
	}

	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnSucceeded {
		t.Errorf("turn.Status = %q, want succeeded", turn.Status)
	}

	finalLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalLink.RemoteCurrentID == nil || *finalLink.RemoteCurrentID != "remote-msg-a1" {
		t.Errorf("link.RemoteCurrentID = %v, want remote-msg-a1", finalLink.RemoteCurrentID)
	}

	// plan §5.4 step 7: a successful continuation is the only thing that
	// verifies the chat_continue capability (chat_create is verified
	// separately, by OnChatCreated — see the StartChat success test).
	workspace, err := env.db.OpenWebUIWorkspaces.Get(t.Context(), env.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.ChatContinueStatus != domain.CapabilityVerified {
		t.Errorf("workspace.ChatContinueStatus = %q, want verified", workspace.ChatContinueStatus)
	}
}

func TestTurnJob_ContinueRetry_LookupDoneAdoptsWithoutResend(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)
	m1 := env.mustCreateReply(t, a0, "m1")

	// Simulate a turn whose previous attempt sent the continuation but the
	// process crashed before recording an outcome: attempt > 0, remote ids
	// recorded, still pending.
	turn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: m1.ID, LocalParentID: &a0.ID,
		RequestID: domain.NewID(), Revision: 1, Attempt: 1, Status: domain.TurnPending,
		RemoteChatID: strPtr("remote-chat-1"), RemoteMessageID: strPtr("remote-user-1"), RemoteAssistantMessageID: strPtr("remote-assistant-1"),
		CreatedAt: env.clock.Now(), UpdatedAt: env.clock.Now(),
	}
	if err := env.db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	payload, _ := json.Marshal(turnJobPayload{TurnID: turn.ID, LinkID: link.ID, Revision: 1, ThreadID: m0.ThreadID})
	job := domain.Job{ID: domain.NewID(), JobType: JobType, Payload: string(payload), PayloadVersion: 1, State: domain.JobPending, Attempt: 1, SourceEntryID: &m1.ID, NextRunAt: env.clock.Now(), CreatedAt: env.clock.Now(), UpdatedAt: env.clock.Now()}
	if err := env.db.Jobs.Enqueue(t.Context(), job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	provider := newFakeProvider(t)
	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		if remoteChatID != "remote-chat-1" || assistantMessageID != "remote-assistant-1" {
			t.Errorf("lookup args = %q/%q, want remote-chat-1/remote-assistant-1", remoteChatID, assistantMessageID)
		}
		return TurnOutcome{Found: true, Done: true, Content: "recovered content", RemoteCurrentID: strPtr("remote-assistant-1")}, nil
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if start, cont, lookup := provider.counts(); start != 0 || cont != 0 || lookup != 1 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want 0/0/1 (adopted without resend)", start, cont, lookup)
	}

	got, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TurnSucceeded {
		t.Errorf("turn.Status = %q, want succeeded", got.Status)
	}
	if got.AssistantEntryID == nil {
		t.Fatal("turn.AssistantEntryID = nil, want the recovered reply's entry id")
	}
	entry, err := env.db.Entries.Get(t.Context(), *got.AssistantEntryID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Body != "recovered content" {
		t.Errorf("recovered entry.Body = %q, want %q", entry.Body, "recovered content")
	}
}

func TestTurnJob_ContinueRetry_LookupErrorResendsSameIDs(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)
	m1 := env.mustCreateReply(t, a0, "m1")

	turn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: m1.ID, LocalParentID: &a0.ID,
		RequestID: domain.NewID(), Revision: 1, Attempt: 1, Status: domain.TurnPending,
		RemoteChatID: strPtr("remote-chat-1"), RemoteMessageID: strPtr("remote-user-1"), RemoteAssistantMessageID: strPtr("remote-assistant-1"),
		CreatedAt: env.clock.Now(), UpdatedAt: env.clock.Now(),
	}
	if err := env.db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	payload, _ := json.Marshal(turnJobPayload{TurnID: turn.ID, LinkID: link.ID, Revision: 1, ThreadID: m0.ThreadID})
	job := domain.Job{ID: domain.NewID(), JobType: JobType, Payload: string(payload), PayloadVersion: 1, State: domain.JobPending, Attempt: 1, SourceEntryID: &m1.ID, NextRunAt: env.clock.Now(), CreatedAt: env.clock.Now(), UpdatedAt: env.clock.Now()}
	if err := env.db.Jobs.Enqueue(t.Context(), job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	provider := newFakeProvider(t)
	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		return TurnOutcome{Found: false}, nil
	}
	provider.continueTurn = func(ctx context.Context, req ContinueTurnRequest) (TurnResult, error) {
		if req.IDs.UserMessageID != "remote-user-1" || req.IDs.AssistantMessageID != "remote-assistant-1" {
			t.Errorf("resend ids = %+v, want the original remote-user-1/remote-assistant-1", req.IDs)
		}
		return TurnResult{Content: "resent content", RemoteCurrentID: strPtr("remote-assistant-1")}, nil
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if start, cont, lookup := provider.counts(); start != 0 || cont != 1 || lookup != 1 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want 0/1/1", start, cont, lookup)
	}
}

// TestTurnJob_ContinueRetry_MissingTurnChatIDFallsBackToLink guards
// against a crash, not just a wrong result: if OnChatCreated's MarkReady
// commits but the SetRemoteCorrelation that should immediately follow it
// on the same turn fails or is interrupted first (a DB error, a crash, a
// lost lease), storage is left with the link ready and its own
// remote_chat_id set, but this turn's own remote_chat_id still NULL,
// attempt already at 1, and remote_assistant_message_id already
// recorded. The next delivery routes through handleReadyRetry, which
// used to dereference turn.RemoteChatID unconditionally — a nil-pointer
// panic that jobs.Manager does not recover from (it takes the whole
// process down, not just this one job). It must fall back to the link's
// own remote_chat_id instead.
func TestTurnJob_ContinueRetry_MissingTurnChatIDFallsBackToLink(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	link := env.mustReadyLink(t, m0.ThreadID)

	turn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: m0.ID,
		RequestID:      domain.NewID(), Revision: 1, Attempt: 1, Status: domain.TurnPending,
		RemoteMessageID: strPtr("remote-user-1"), RemoteAssistantMessageID: strPtr("remote-assistant-1"),
		CreatedAt: env.clock.Now(), UpdatedAt: env.clock.Now(),
	}
	if turn.RemoteChatID != nil {
		t.Fatal("test setup: turn.RemoteChatID must start nil")
	}
	if err := env.db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	payload, _ := json.Marshal(turnJobPayload{TurnID: turn.ID, LinkID: link.ID, Revision: 1, ThreadID: m0.ThreadID})
	job := domain.Job{ID: domain.NewID(), JobType: JobType, Payload: string(payload), PayloadVersion: 1, State: domain.JobPending, Attempt: 1, SourceEntryID: &m0.ID, NextRunAt: env.clock.Now(), CreatedAt: env.clock.Now(), UpdatedAt: env.clock.Now()}
	if err := env.db.Jobs.Enqueue(t.Context(), job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	provider := newFakeProvider(t)
	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		if link.RemoteChatID == nil || remoteChatID != *link.RemoteChatID {
			t.Errorf("lookup remoteChatID = %q, want the link's own %v", remoteChatID, link.RemoteChatID)
		}
		if assistantMessageID != "remote-assistant-1" {
			t.Errorf("lookup assistantMessageID = %q, want remote-assistant-1", assistantMessageID)
		}
		return TurnOutcome{Found: true, Done: true, Content: "recovered", RemoteCurrentID: strPtr("remote-assistant-1")}, nil
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if start, cont, lookup := provider.counts(); start != 0 || cont != 0 || lookup != 1 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want 0/0/1 (adopted without resend)", start, cont, lookup)
	}

	got, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TurnSucceeded {
		t.Errorf("turn.Status = %q, want succeeded", got.Status)
	}
	if got.RemoteChatID == nil || link.RemoteChatID == nil || *got.RemoteChatID != *link.RemoteChatID {
		t.Errorf("turn.RemoteChatID = %v, want healed to the link's own %v", got.RemoteChatID, link.RemoteChatID)
	}
}

func TestTurnJob_ContinueTurn_AuthFailedNeverRetries(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	bridge := newTestBridge(env)
	m1 := env.mustCreateReply(t, a0, "m1")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{State: jobStatePtr(domain.JobPending)})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 1 {
		t.Fatalf("pending jobs = %v, want exactly 1", jobRows)
	}
	job := jobRows[0]
	payload := mustTurnJobPayload(t, job)

	provider := newFakeProvider(t)
	provider.continueTurn = func(ctx context.Context, req ContinueTurnRequest) (TurnResult, error) {
		return TurnResult{}, NewProviderError(CategoryAuthFailed, PhaseTurn, errors.New("401"))
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	err = turnJob.Handle(t.Context(), job)
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("Handle error = %v, want a jobs.PermanentError", err)
	}

	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnAuthFailed && (turn.Status != domain.TurnFailed || turn.FailureCategory == nil || *turn.FailureCategory != domain.FailureCategoryAuthFailed) {
		t.Errorf("turn = %+v, want failed/auth_failed", turn)
	}

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkReady {
		t.Errorf("link.State = %q, want ready (a turn failure must not disturb a ready link)", gotLink.State)
	}

	// A second delivery must not call the provider again: the turn is
	// already terminal.
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("second Handle: %v, want nil (duplicate delivery)", err)
	}
	if _, cont, _ := provider.counts(); cont != 1 {
		t.Errorf("ContinueTurn calls = %d, want exactly 1 across both deliveries", cont)
	}
}

// TestTurnJob_ConcurrentHandle_SerializesPerThread backs ADR-0005 D5's
// in-process, per-thread single-flight: two turns on the same thread,
// handled concurrently by two goroutines, must never both be in the
// provider at once.
func TestTurnJob_ConcurrentHandle_SerializesPerThread(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "root")
	bridge := newTestBridge(env)

	// Two independent new-branch turns sharing the same thread: two
	// distinct root-level replies from the owner, each starting its own
	// branch (per ADR-0005 D4, since neither replies to any assistant
	// head).
	m1 := env.mustCreateReply(t, root, "question 1")
	m2 := env.mustCreateReply(t, root, "question 2")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn m1: %v", err)
	}
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m2); err != nil {
		t.Fatalf("EnqueueTurn m2: %v", err)
	}
	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 2 {
		t.Fatalf("jobs = %v, want exactly 2", jobRows)
	}

	release := make(chan struct{})
	provider := newFakeProvider(t)
	provider.startChat = func(ctx context.Context, req StartChatRequest) (TurnResult, error) {
		<-release
		if err := req.OnChatCreated(ctx, "remote-chat-"+req.IDs.UserMessageID); err != nil {
			return TurnResult{}, err
		}
		return TurnResult{Content: "reply", RemoteCurrentID: strPtr("remote-msg-" + req.IDs.AssistantMessageID)}, nil
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, j := range jobRows {
		wg.Add(1)
		go func(j domain.Job) {
			defer wg.Done()
			errs <- turnJob.Handle(t.Context(), j)
		}(j)
	}
	// Give both goroutines a chance to reach the lock/provider boundary
	// before releasing either.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Handle: %v", err)
		}
	}

	if max := atomic.LoadInt32(&provider.maxInFlight); max > 1 {
		t.Errorf("max concurrent StartChat calls = %d, want at most 1 (per-thread single-flight)", max)
	}
	if start, _, _ := provider.counts(); start != 2 {
		t.Errorf("StartChat calls = %d, want 2 (both eventually ran)", start)
	}
}

// TestTurnJob_LinkStateGuard_AmbiguousFailedDeadNeverCallProvider covers
// plan §7.3's state-transition guard: a pending turn whose link has
// already left creation_pending/ready (an owner recovery action, or a
// prior job run, moved it to ambiguous/failed/dead) must fail closed as
// link_not_ready without the provider ever being called. Handle's own
// switch on link.State — its default case — is the only gate for this,
// so this exercises it directly rather than through handleCreationPending
// or handleReady.
func TestTurnJob_LinkStateGuard_AmbiguousFailedDeadNeverCallProvider(t *testing.T) {
	for _, state := range []domain.LinkState{domain.LinkAmbiguous, domain.LinkFailed, domain.LinkDead} {
		t.Run(string(state), func(t *testing.T) {
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
			switch state {
			case domain.LinkAmbiguous:
				if err := env.db.OpenWebUILinks.MarkAmbiguous(t.Context(), link.ID, now); err != nil {
					t.Fatalf("mark ambiguous: %v", err)
				}
			case domain.LinkFailed:
				if err := env.db.OpenWebUILinks.MarkFailed(t.Context(), link.ID, domain.FailureCategoryClientRejected, now); err != nil {
					t.Fatalf("mark failed: %v", err)
				}
			case domain.LinkDead:
				if err := env.db.OpenWebUILinks.MarkAmbiguous(t.Context(), link.ID, now); err != nil {
					t.Fatalf("mark ambiguous (pre-dead): %v", err)
				}
				if err := env.db.OpenWebUILinks.MarkDead(t.Context(), link.ID, domain.FailureCategoryOwnerAbandoned, now); err != nil {
					t.Fatalf("mark dead: %v", err)
				}
			}
			preLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
			if err != nil {
				t.Fatal(err)
			}
			if preLink.State != state {
				t.Fatalf("preLink.State = %q, want %q", preLink.State, state)
			}

			turn := domain.OpenWebUITurnLink{
				ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
				LocalMessageID: root.ID,
				RequestID:      domain.NewID(), Revision: 1, Attempt: 0, Status: domain.TurnPending,
				CreatedAt: now, UpdatedAt: now,
			}
			if err := env.db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
				t.Fatalf("create turn: %v", err)
			}
			payload, _ := json.Marshal(turnJobPayload{TurnID: turn.ID, LinkID: link.ID, Revision: 1, ThreadID: root.ThreadID})
			job := domain.Job{ID: domain.NewID(), JobType: JobType, Payload: string(payload), PayloadVersion: 1, State: domain.JobPending, SourceEntryID: &root.ID, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if err := env.db.Jobs.Enqueue(t.Context(), job); err != nil {
				t.Fatalf("enqueue job: %v", err)
			}

			provider := newFakeProvider(t) // no scripts: any call fails the test
			turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

			err = turnJob.Handle(t.Context(), job)
			var permanent *jobs.PermanentError
			if !errors.As(err, &permanent) {
				t.Fatalf("Handle error = %v, want a jobs.PermanentError", err)
			}

			gotTurn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotTurn.Status != domain.TurnFailed || gotTurn.FailureCategory == nil || *gotTurn.FailureCategory != domain.FailureCategoryLinkNotReady {
				t.Errorf("turn = %+v, want failed/link_not_ready", gotTurn)
			}

			gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotLink.State != state {
				t.Errorf("link.State = %q, want unchanged %q", gotLink.State, state)
			}
			if start, cont, lookup := provider.counts(); start != 0 || cont != 0 || lookup != 0 {
				t.Errorf("provider calls = start:%d continue:%d lookup:%d, want none", start, cont, lookup)
			}
		})
	}
}

// TestTurnJob_StaleBranchIsolation_CompletionOnlyMovesItsOwnLink covers a
// roadmap item PR3 left untested: a turn's completion must only ever
// move its own link's remote_current_id, never a sibling branch's. Two
// branches (two links) exist on the same thread; only one of them ever
// completes a turn during this test, and the other — a stale/never-
// touched branch — must be left exactly as it started.
func TestTurnJob_StaleBranchIsolation_CompletionOnlyMovesItsOwnLink(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "root")
	bridge := newTestBridge(env)

	// Two independent new-branch turns on the same thread (ADR-0005 D4:
	// two root-level replies, neither a continuation of the other).
	m1 := env.mustCreateReply(t, root, "question 1")
	m2 := env.mustCreateReply(t, root, "question 2")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn m1: %v", err)
	}
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m2); err != nil {
		t.Fatalf("EnqueueTurn m2: %v", err)
	}
	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 2 {
		t.Fatalf("jobs = %v, want exactly 2", jobRows)
	}

	// Identify which job belongs to m1 so only its turn is ever handled;
	// m2's link is the "stale branch" that must stay untouched throughout.
	var m1Job domain.Job
	var m2LinkID string
	for _, j := range jobRows {
		p := mustTurnJobPayload(t, j)
		turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), p.TurnID)
		if err != nil {
			t.Fatal(err)
		}
		if turn.LocalMessageID == m1.ID {
			m1Job = j
		} else {
			m2LinkID = p.LinkID
		}
	}
	if m1Job.ID == "" || m2LinkID == "" {
		t.Fatalf("could not identify both jobs among %v", jobRows)
	}
	m2LinkBefore, err := env.db.OpenWebUILinks.Get(t.Context(), m2LinkID)
	if err != nil {
		t.Fatal(err)
	}

	provider := newFakeProvider(t)
	provider.startChat = func(ctx context.Context, req StartChatRequest) (TurnResult, error) {
		if err := req.OnChatCreated(ctx, "remote-chat-m1"); err != nil {
			return TurnResult{}, err
		}
		return TurnResult{Content: "reply to m1", RemoteCurrentID: strPtr("remote-msg-m1")}, nil
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})
	if err := turnJob.Handle(t.Context(), m1Job); err != nil {
		t.Fatalf("Handle(m1): %v", err)
	}

	m2LinkAfter, err := env.db.OpenWebUILinks.Get(t.Context(), m2LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if m2LinkAfter.RemoteCurrentID != nil {
		t.Errorf("m2's link.RemoteCurrentID = %v, want unchanged nil (m1's completion must not touch it)", m2LinkAfter.RemoteCurrentID)
	}
	if m2LinkAfter.State != m2LinkBefore.State {
		t.Errorf("m2's link.State = %q, want unchanged %q", m2LinkAfter.State, m2LinkBefore.State)
	}
	if m2LinkAfter.UpdatedAt != m2LinkBefore.UpdatedAt {
		t.Errorf("m2's link.UpdatedAt changed (%v -> %v), want untouched by m1's completion", m2LinkBefore.UpdatedAt, m2LinkAfter.UpdatedAt)
	}
}

// TestTurnJob_StartChat_ContextCancelledMidCall_RetriesWithoutFreezing
// covers plan §5.4 step 8's cancellation rule for the create phase: a
// StartChat call that fails only because its own ctx was already
// cancelled (jobs.Manager shutting down, or this job's lease expiring
// mid-call) must leave the turn exactly as BeginAttempt left it —
// pending, attempt already recorded — never freezing the link ambiguous
// on this same delivery the way a genuine remote-side creation failure
// would. Only a *later* delivery's turn.Attempt > 0 check (creation is
// never replayed) may do that, once the outcome is genuinely unknown
// rather than merely locally interrupted.
func TestTurnJob_StartChat_ContextCancelledMidCall_RetriesWithoutFreezing(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	ctx, cancel := context.WithCancel(t.Context())
	provider := newFakeProvider(t)
	provider.startChat = func(callCtx context.Context, req StartChatRequest) (TurnResult, error) {
		// Simulate a Manager shutdown or lease loss racing this exact
		// call: the outer job context is cancelled while the request is
		// still in flight, and the (real) adapter's own classifyDoError
		// maps that to CategoryTimeout — see its own doc comment.
		cancel()
		<-callCtx.Done()
		return TurnResult{}, NewProviderError(CategoryTimeout, PhaseCreate, callCtx.Err())
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	err := turnJob.Handle(ctx, job)
	var permanent *jobs.PermanentError
	if errors.As(err, &permanent) {
		t.Fatalf("Handle error = %v, want a retryable (non-Permanent) error for a cancellation-driven failure", err)
	}
	if err == nil {
		t.Fatal("Handle error = nil, want an error (the call was cancelled)")
	}

	link, err := env.db.OpenWebUILinks.Get(t.Context(), payload.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if link.State != domain.LinkCreationPending {
		t.Errorf("link.State = %q, want unchanged creation_pending", link.State)
	}
	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnPending || turn.Attempt != 1 {
		t.Errorf("turn = %+v, want pending/attempt=1 (retryable, not frozen)", turn)
	}
}

// TestTurnJob_ContinueTurn_ContextCancelledMidCall_NextDeliveryLooksUpFirst
// is ContinueTurn's side of the same rule: a cancellation-driven failure
// leaves the turn retryable, and — because BeginAttempt and
// setCorrelation already ran before the call — the next delivery routes
// through handleReadyRetry's lookup-first path rather than immediately
// resending.
func TestTurnJob_ContinueTurn_ContextCancelledMidCall_NextDeliveryLooksUpFirst(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	bridge := newTestBridge(env)
	m1 := env.mustCreateReply(t, a0, "m1")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	ctx, cancel := context.WithCancel(t.Context())
	provider := newFakeProvider(t)
	provider.continueTurn = func(callCtx context.Context, req ContinueTurnRequest) (TurnResult, error) {
		cancel()
		<-callCtx.Done()
		return TurnResult{}, NewProviderError(CategoryTimeout, PhaseTurn, callCtx.Err())
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	err := turnJob.Handle(ctx, job)
	var permanent *jobs.PermanentError
	if errors.As(err, &permanent) {
		t.Fatalf("Handle error = %v, want a retryable (non-Permanent) error", err)
	}

	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnPending || turn.Attempt != 1 || turn.RemoteAssistantMessageID == nil {
		t.Errorf("turn = %+v, want pending/attempt=1 with its correlation already recorded", turn)
	}

	provider.lookupOutcome = func(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error) {
		return TurnOutcome{Found: true, Done: true, Content: "recovered after cancel", RemoteCurrentID: strPtr("remote-recovered")}, nil
	}
	if err := turnJob.Handle(t.Context(), job); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if start, cont, lookup := provider.counts(); start != 0 || cont != 1 || lookup != 1 {
		t.Errorf("provider calls = start:%d continue:%d lookup:%d, want 0/1/1 (one continue, then a lookup-first redelivery, no resend)", start, cont, lookup)
	}
}

// TestTurnJob_ContinueTurn_ContractFailedFailsPermanentlyLinkStaysReady
// covers a schema-drift/malformed response reaching TurnJob itself,
// rather than only internal/provider/openwebui's own (already-covered)
// classification of it into CategoryContractFailed: the turn fails
// permanently with FailureCategoryContractFailed, the still-confirmed
// link is left ready (this is a turn-phase failure, not a creation
// one), and no notification or assistant entry is ever created.
func TestTurnJob_ContinueTurn_ContractFailedFailsPermanentlyLinkStaysReady(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	bridge := newTestBridge(env)
	m1 := env.mustCreateReply(t, a0, "m1")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	job := mustSoleJob(t, env)
	payload := mustTurnJobPayload(t, job)

	provider := newFakeProvider(t)
	provider.continueTurn = func(ctx context.Context, req ContinueTurnRequest) (TurnResult, error) {
		return TurnResult{}, NewProviderError(CategoryContractFailed, PhaseTurn, errors.New("decode completion response: json: unexpected end of JSON input"))
	}
	turnJob, _ := newTestTurnJob(env, provider, TurnJobConfig{})

	err := turnJob.Handle(t.Context(), job)
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("Handle error = %v, want a jobs.PermanentError", err)
	}

	turn, err := env.db.OpenWebUITurnLinks.Get(t.Context(), payload.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != domain.TurnFailed || turn.FailureCategory == nil || *turn.FailureCategory != domain.FailureCategoryContractFailed {
		t.Errorf("turn = %+v, want failed/contract_failed", turn)
	}
	if turn.AssistantEntryID != nil {
		t.Errorf("turn.AssistantEntryID = %v, want nil (no reply for a schema-drift failure)", turn.AssistantEntryID)
	}

	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLink.State != domain.LinkReady {
		t.Errorf("link.State = %q, want ready (a turn failure must not disturb a ready link)", gotLink.State)
	}

	notifications, err := env.db.Notifications.ListDesc(t.Context(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 0 {
		t.Errorf("notifications = %+v, want none", notifications)
	}
}

func jobStatePtr(s domain.JobState) *domain.JobState { return &s }
