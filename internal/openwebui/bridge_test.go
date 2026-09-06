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
