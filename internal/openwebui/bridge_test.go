package openwebui

import (
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func newTestBridge(env *turnTestEnv) *Bridge {
	return NewBridge(BridgeConfig{MaxContextMessages: 100}, env.clock, nil)
}

func TestEnqueueTurn_SkipsNonUserPost(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "root")
	newsEntry := env.mustCreateReplyAs(t, root, env.assistantActorID(t), domain.EntryLLMReply, "assistant reply")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, newsEntry); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_SkipsWhenGenerationDisabled(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig()) // GenerationEnabled left false
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ownerID := mustCreateOwner(t, tr)
	env := &turnTestEnv{db: tr.db, clock: tr.clock, ownerID: ownerID}
	bridge := newTestBridge(env)

	root := env.mustCreateRoot(t, "hello")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_SkipsWhenNoEnabledWorkspace(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig()) // never seeded: no workspace at all
	ownerID := domain.NewID()
	if err := tr.db.Actors.Create(t.Context(), domain.Actor{ID: ownerID, Type: domain.ActorOwner, CreatedAt: tr.clock.Now()}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	env := &turnTestEnv{db: tr.db, clock: tr.clock, ownerID: ownerID}
	bridge := newTestBridge(env)

	root := env.mustCreateRoot(t, "hello")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_SkipsAuthorNotLoginable(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	// The system actor can author a user_post row directly (bypassing
	// timeline.Service's own author-type gate), which is exactly the
	// defense-in-depth case EnqueueTurn's own IsLoginable check exists
	// for: never enqueue a turn for anything but the owner's own post.
	systemActor, err := env.db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatalf("get system actor: %v", err)
	}
	now := env.clock.Now()
	id := domain.NewID()
	entry := domain.Entry{ID: id, ThreadID: id, Kind: domain.EntryUserPost, AuthorActorID: systemActor.ID, Body: "not the owner", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now}
	if err := env.db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := env.db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, entry); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_SkipsMentionOnlyPost(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	// A bare mention (no other text) strips to an empty provider-facing
	// message (Issue #70's follow-up), so there is nothing to start a
	// turn over.
	root := env.mustCreateRoot(t, "@someone@example.com")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_SkipsWhitespaceOnlyMentionPost(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	// A mention padded with whitespace still strips to nothing once the
	// mention-gap cleanup and trim run.
	root := env.mustCreateRoot(t, "  @owner  ")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_SkipsWhitespaceOnlyPostWithNoMentionAtAll(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	// No mention at all, but nothing but whitespace either — the same
	// "nothing to send" case as a stripped-down mention.
	root := env.mustCreateRoot(t, "   ")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_DoesNotSkipMentionPlusRealText(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	// A mention alongside real text must still enqueue normally — only
	// an entirely-empty provider-facing result is skipped.
	root := env.mustCreateRoot(t, "@owner hello")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 1 {
		t.Fatalf("jobs = %v, want exactly 1", jobRows)
	}
}

func TestEnqueueTurn_SkipsIneligiblePath(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "root")
	hidden := env.mustCreateReply(t, root, "will be hidden")
	if err := env.db.Entries.SetHidden(t.Context(), hidden.ID, true, env.clock.Now()); err != nil {
		t.Fatalf("hide entry: %v", err)
	}
	target := env.mustCreateReply(t, hidden, "reply to hidden ancestor")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, target); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	requireNoTurnRows(t, env)
}

func TestEnqueueTurn_CreatesNewBranchForRoot(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 1 {
		t.Fatalf("jobs = %v, want exactly 1", jobRows)
	}
	job := jobRows[0]
	if job.JobType != JobType {
		t.Errorf("job.JobType = %q, want %q", job.JobType, JobType)
	}
	if job.SourceEntryID == nil || *job.SourceEntryID != root.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", job.SourceEntryID, root.ID)
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %v, want exactly 1", links)
	}
	link := links[0]
	if link.State != domain.LinkCreationPending {
		t.Errorf("link.State = %q, want creation_pending", link.State)
	}
	if link.ClaimJobID == nil || *link.ClaimJobID != job.ID {
		t.Errorf("link.ClaimJobID = %v, want %q", link.ClaimJobID, job.ID)
	}
	if link.WorkspaceID != env.workspace.ID || link.ModelID != env.model.ID {
		t.Errorf("link workspace/model = %q/%q, want %q/%q", link.WorkspaceID, link.ModelID, env.workspace.ID, env.model.ID)
	}

	turns, err := env.db.OpenWebUITurnLinks.ListByLink(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %v, want exactly 1", turns)
	}
	turn := turns[0]
	if turn.LocalMessageID != root.ID {
		t.Errorf("turn.LocalMessageID = %q, want %q", turn.LocalMessageID, root.ID)
	}
	if turn.LocalParentID != nil {
		t.Errorf("turn.LocalParentID = %v, want nil for a root post", turn.LocalParentID)
	}
	if turn.Status != domain.TurnPending {
		t.Errorf("turn.Status = %q, want pending", turn.Status)
	}
	if turn.RemoteParentID != nil {
		t.Errorf("turn.RemoteParentID = %v, want nil for a new branch", turn.RemoteParentID)
	}
}

func TestEnqueueTurn_ContinuesExistingLink(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	m1 := env.mustCreateReply(t, a0, "m1")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), m0.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %v, want exactly 1 (the continuation must not create a new one)", links)
	}

	turns, err := env.db.OpenWebUITurnLinks.ListByLink(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %v, want 2 (the seeded succeeded one, plus m1's)", turns)
	}
	var m1Turn *domain.OpenWebUITurnLink
	for i := range turns {
		if turns[i].LocalMessageID == m1.ID {
			m1Turn = &turns[i]
		}
	}
	if m1Turn == nil {
		t.Fatalf("no turn recorded for m1 among %v", turns)
	}
	if m1Turn.LocalParentID == nil || *m1Turn.LocalParentID != a0.ID {
		t.Errorf("m1Turn.LocalParentID = %v, want %q", m1Turn.LocalParentID, a0.ID)
	}
	gotLink, err := env.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m1Turn.RemoteParentID == nil || gotLink.RemoteCurrentID == nil || *m1Turn.RemoteParentID != *gotLink.RemoteCurrentID {
		t.Errorf("m1Turn.RemoteParentID = %v, want link.RemoteCurrentID %v", m1Turn.RemoteParentID, gotLink.RemoteCurrentID)
	}
}

func TestEnqueueTurn_DuplicateDeliveryIsIdempotent(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	root := env.mustCreateRoot(t, "hello")

	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn (first): %v", err)
	}
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn (second): %v", err)
	}

	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 1 {
		t.Fatalf("jobs = %v, want exactly 1 after two deliveries of the same entry", jobRows)
	}
	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %v, want exactly 1", links)
	}
}

// TestEnqueueTurn_SingleMentionRoutesToThatModelNotTheDefault is Issue
// #75's core routing rule: a post @mentioning exactly one active,
// non-default model starts its branch against that model, never the
// workspace's configured default.
func TestEnqueueTurn_SingleMentionRoutesToThatModelNotTheDefault(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	other := env.mustCreateSecondModel(t, "gpt-oss:120b", "other_model")

	root := env.mustCreateRoot(t, "@other_model help me")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %v, want exactly 1", links)
	}
	if links[0].ModelID != other.ID {
		t.Errorf("link.ModelID = %q, want the mentioned model %q, not the default %q", links[0].ModelID, other.ID, env.model.ID)
	}
}

// TestEnqueueTurn_NoMentionRoutesToDefault pins the fallback half of the
// same rule against a regression: no baseline test above ever exercised
// a body containing an "@" at all.
func TestEnqueueTurn_NoMentionRoutesToDefault(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	env.mustCreateSecondModel(t, "gpt-oss:120b", "other_model")

	root := env.mustCreateRoot(t, "no mention here")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].ModelID != env.model.ID {
		t.Fatalf("links = %v, want exactly 1 bound to the default model %q", links, env.model.ID)
	}
}

// TestEnqueueTurn_ReplyMentioningDifferentModelStartsNewBranch is Issue
// #75's cross-model-reply rule end to end: replying to an existing
// model's assistant message while @mentioning a *different* model must
// not attach to the first model's remote chat — it starts a second,
// independent branch bound to the mentioned model.
func TestEnqueueTurn_ReplyMentioningDifferentModelStartsNewBranch(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	other := env.mustCreateSecondModel(t, "gpt-oss:120b", "other_model")

	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	m1 := env.mustCreateReply(t, a0, "@other_model take over from here")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, m1); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), m0.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 2 {
		t.Fatalf("links = %v, want 2 (the original plus a new one for the mentioned model)", links)
	}
	var newLink *domain.OpenWebUIConversationLink
	for i := range links {
		if links[i].ID != link.ID {
			newLink = &links[i]
		}
	}
	if newLink == nil {
		t.Fatal("the original link's row disappeared")
	}
	if newLink.ModelID != other.ID {
		t.Errorf("newLink.ModelID = %q, want the mentioned model %q", newLink.ModelID, other.ID)
	}
	if newLink.State != domain.LinkCreationPending {
		t.Errorf("newLink.State = %q, want creation_pending", newLink.State)
	}

	// The original link is untouched: still ready, still exactly its one
	// succeeded turn.
	originalTurns, err := env.db.OpenWebUITurnLinks.ListByLink(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(originalTurns) != 1 {
		t.Errorf("original link turns = %v, want exactly 1 (untouched)", originalTurns)
	}
}

// TestEnqueueTurn_AmbiguousMentionRecordsFailedLinkAndTurnWithoutEnqueueing
// is the ambiguous_model_selection decision table entry (owner decision,
// Issue #75): mentioning two distinct active models at once must not
// enqueue any job or guess between them, but must leave an owner-visible
// record of what happened.
func TestEnqueueTurn_AmbiguousMentionRecordsFailedLinkAndTurnWithoutEnqueueing(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	second := env.mustCreateSecondModel(t, "gpt-oss:120b", "big_model")

	body := "hey @" + env.model.ActorSlug + " and @" + second.ActorSlug + ", which of you wants this?"
	root := env.mustCreateRoot(t, body)
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobRows {
		if j.JobType == JobType {
			t.Errorf("found an %q job %+v, want none for an ambiguous mention", JobType, j)
		}
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %v, want exactly 1 (recorded failed for visibility)", links)
	}
	link := links[0]
	if link.State != domain.LinkFailed {
		t.Errorf("link.State = %q, want failed", link.State)
	}
	if link.FailureCategory == nil || *link.FailureCategory != domain.FailureCategoryAmbiguousModelSelection {
		t.Errorf("link.FailureCategory = %v, want %q", link.FailureCategory, domain.FailureCategoryAmbiguousModelSelection)
	}
	if link.ClaimJobID != nil {
		t.Errorf("link.ClaimJobID = %v, want nil (no job was ever enqueued)", link.ClaimJobID)
	}

	turns, err := env.db.OpenWebUITurnLinks.ListByLink(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %v, want exactly 1", turns)
	}
	turn := turns[0]
	if turn.Status != domain.TurnFailed {
		t.Errorf("turn.Status = %q, want failed", turn.Status)
	}
	if turn.FailureCategory == nil || *turn.FailureCategory != domain.FailureCategoryAmbiguousModelSelection {
		t.Errorf("turn.FailureCategory = %v, want %q", turn.FailureCategory, domain.FailureCategoryAmbiguousModelSelection)
	}
	if turn.LocalMessageID != root.ID {
		t.Errorf("turn.LocalMessageID = %q, want %q", turn.LocalMessageID, root.ID)
	}
}

// TestEnqueueTurn_AmbiguousMentionOnlyPostIsNotSkippedAsEmpty pins the
// Issue #75 rebase decision (plan §8-3) that ambiguous_model_selection
// takes priority over the empty-provider-body skip
// (TestEnqueueTurn_SkipsMentionOnlyPost's case): a post that is nothing
// but two distinct model mentions — no other text at all, so its
// provider-facing body strips down to empty exactly like a single bare
// mention does — must still surface as an explicit, owner-visible
// ambiguous_model_selection failure, never be silently dropped as if it
// had nothing to send.
func TestEnqueueTurn_AmbiguousMentionOnlyPostIsNotSkippedAsEmpty(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	second := env.mustCreateSecondModel(t, "gpt-oss:120b", "big_model")

	body := "@" + env.model.ActorSlug + " @" + second.ActorSlug
	root := env.mustCreateRoot(t, body)
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobRows {
		if j.JobType == JobType {
			t.Errorf("found an %q job %+v, want none for an ambiguous mention-only post", JobType, j)
		}
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %v, want exactly 1 (recorded failed for visibility, not silently skipped)", links)
	}
	link := links[0]
	if link.State != domain.LinkFailed {
		t.Errorf("link.State = %q, want failed", link.State)
	}
	if link.FailureCategory == nil || *link.FailureCategory != domain.FailureCategoryAmbiguousModelSelection {
		t.Errorf("link.FailureCategory = %v, want %q", link.FailureCategory, domain.FailureCategoryAmbiguousModelSelection)
	}

	turns, err := env.db.OpenWebUITurnLinks.ListByLink(t.Context(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %v, want exactly 1", turns)
	}
	if turns[0].FailureCategory == nil || *turns[0].FailureCategory != domain.FailureCategoryAmbiguousModelSelection {
		t.Errorf("turn.FailureCategory = %v, want %q", turns[0].FailureCategory, domain.FailureCategoryAmbiguousModelSelection)
	}
}

// TestEnqueueTurn_MentionOfInactiveModelFallsBackToDefault backs the
// decision table's "inactive model" row folding into the same bucket as
// "no mention at all" or an unknown slug — never an error, never
// ambiguous.
func TestEnqueueTurn_MentionOfInactiveModelFallsBackToDefault(t *testing.T) {
	env := newTurnTestEnv(t)
	bridge := newTestBridge(env)
	other := env.mustCreateSecondModel(t, "gpt-oss:120b", "other_model")
	if err := env.db.OpenWebUIModels.SetActive(t.Context(), other.ID, false, env.clock.Now()); err != nil {
		t.Fatal(err)
	}

	root := env.mustCreateRoot(t, "@other_model are you there?")
	if err := bridge.EnqueueTurn(t.Context(), env.db.Repos, root); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}

	links, err := env.db.OpenWebUILinks.ListByThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].ModelID != env.model.ID {
		t.Fatalf("links = %v, want exactly 1 bound to the default model %q (mentioning an inactive model falls back)", links, env.model.ID)
	}
}

// requireNoTurnRows asserts EnqueueTurn wrote none of its three rows: no
// "openwebui_turn" job, and no conversation link at all.
func requireNoTurnRows(t *testing.T, env *turnTestEnv) {
	t.Helper()
	jobRows, err := env.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobRows {
		if j.JobType == JobType {
			t.Errorf("found an %q job %+v, want none", JobType, j)
		}
	}
	links, err := env.db.OpenWebUILinks.List(t.Context(), domain.OpenWebUILinkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Errorf("links = %v, want none", links)
	}
}
