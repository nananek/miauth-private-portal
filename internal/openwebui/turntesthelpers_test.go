package openwebui

import (
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// turnTestEnv seeds a generation-enabled workspace and default model (via
// Registry.Seed, the same production path) and exposes the pieces
// path/bridge/turn-job tests need: the migrated database, a controllable
// clock, the owner actor id, and the seeded workspace/model.
type turnTestEnv struct {
	db        *sqlite.DB
	clock     *fakeClock
	ownerID   string
	workspace domain.OpenWebUIWorkspace
	model     domain.OpenWebUIModel
}

// newTurnTestEnv builds a turnTestEnv with generation enabled. Tests that
// need generation left off (the bridge/job's own gating tests) build
// their own testRegistry with validRegistryConfig() instead.
func newTurnTestEnv(t *testing.T) *turnTestEnv {
	t.Helper()
	cfg := validRegistryConfig()
	cfg.GenerationEnabled = true
	tr := newTestRegistry(t, cfg)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	ownerID := mustCreateOwner(t, tr)

	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatalf("get enabled workspace: %v", err)
	}
	model, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatalf("get default model: %v", err)
	}

	return &turnTestEnv{db: tr.db, clock: tr.clock, ownerID: ownerID, workspace: workspace, model: model}
}

// assistantActorID resolves the reserved assistant actor's id (Issue
// #9), for tests building a path through a legacy assistant reply.
func (e *turnTestEnv) assistantActorID(t *testing.T) string {
	t.Helper()
	actor, err := e.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatalf("get assistant actor: %v", err)
	}
	return actor.ID
}

// mustCreateRoot creates a root user_post entry authored by e.ownerID
// (thread id == entry id), bypassing timeline.Service so a test controls
// the exact tree shape without any enqueue hook firing.
func (e *turnTestEnv) mustCreateRoot(t *testing.T, body string) domain.Entry {
	t.Helper()
	now := e.clock.Now()
	id := domain.NewID()
	entry := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryUserPost, AuthorActorID: e.ownerID,
		Body: body, ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	}
	if err := e.db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := e.db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create root entry: %v", err)
	}
	return entry
}

// mustCreateReply creates a user_post reply to parent, authored by
// e.ownerID.
func (e *turnTestEnv) mustCreateReply(t *testing.T, parent domain.Entry, body string) domain.Entry {
	t.Helper()
	return e.mustCreateReplyAs(t, parent, e.ownerID, domain.EntryUserPost, body)
}

// mustCreateReplyAs creates a reply entry of kind, authored by
// authorActorID, replying to parent.
func (e *turnTestEnv) mustCreateReplyAs(t *testing.T, parent domain.Entry, authorActorID string, kind domain.EntryKind, body string) domain.Entry {
	t.Helper()
	now := e.clock.Now()
	entry := domain.Entry{
		ID: domain.NewID(), ThreadID: parent.ThreadID, ParentEntryID: &parent.ID, Kind: kind,
		AuthorActorID: authorActorID, Body: body, ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	}
	if err := e.db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create reply entry: %v", err)
	}
	return entry
}

// mustCreateSecondModel registers a second active model in e's seeded
// workspace, for tests that need two distinct models to mention or to
// bind two different links to (cross-model reply/continuation, ambiguous
// mention resolution). It mints its own VirtualActor row the same way
// Registry.SyncCatalog's createSyncedModel does, but directly through
// the repository rather than through a fake CatalogProvider — the
// model's own registration mechanics are catalog.go's tests' concern,
// not these callers'.
func (e *turnTestEnv) mustCreateSecondModel(t *testing.T, externalModelID, actorSlug string) domain.OpenWebUIModel {
	t.Helper()
	now := e.clock.Now()
	actor := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: now}
	if err := e.db.Actors.Create(t.Context(), actor); err != nil {
		t.Fatalf("create second model actor: %v", err)
	}
	model := domain.OpenWebUIModel{
		ID:              domain.NewID(),
		WorkspaceID:     e.workspace.ID,
		ExternalModelID: externalModelID,
		DisplayName:     externalModelID,
		ActorSlug:       actorSlug,
		ActorID:         actor.ID,
		Active:          true,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := e.db.OpenWebUIModels.Create(t.Context(), model); err != nil {
		t.Fatalf("create second model: %v", err)
	}
	return model
}

// mustReadyLink claims and confirms a ready conversation link for
// threadID against e's seeded workspace/model, as if a completed
// StartChat had produced it.
func (e *turnTestEnv) mustReadyLink(t *testing.T, threadID string) domain.OpenWebUIConversationLink {
	t.Helper()
	now := e.clock.Now()
	link := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: threadID, BranchID: domain.NewID(),
		WorkspaceID: e.workspace.ID, ModelID: e.model.ID,
		ClaimedAt: now, LastTransitionAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := e.db.OpenWebUILinks.Claim(t.Context(), link); err != nil {
		t.Fatalf("claim link: %v", err)
	}
	remoteChatID := "remote-chat-" + link.ID
	if err := e.db.OpenWebUILinks.MarkReady(t.Context(), link.ID, remoteChatID, nil, now); err != nil {
		t.Fatalf("mark link ready: %v", err)
	}
	got, err := e.db.OpenWebUILinks.Get(t.Context(), link.ID)
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	return got
}

// mustSucceededTurn records a succeeded turn on link for localMessage
// with assistantEntry as its result, and advances the link's
// remote_current_id (mirroring TurnJob.complete's own writes), so
// SelectBranch sees assistantEntry as the link's head.
func (e *turnTestEnv) mustSucceededTurn(t *testing.T, link domain.OpenWebUIConversationLink, localMessage, assistantEntry domain.Entry) domain.OpenWebUITurnLink {
	t.Helper()
	now := e.clock.Now()
	turn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: localMessage.ID, LocalParentID: localMessage.ParentEntryID,
		RequestID: domain.NewID(), Revision: 1, Attempt: 1, Status: domain.TurnPending,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := e.db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if err := e.db.OpenWebUITurnLinks.SetAssistantEntry(t.Context(), turn.ID, assistantEntry.ID, now); err != nil {
		t.Fatalf("set assistant entry: %v", err)
	}
	if err := e.db.OpenWebUITurnLinks.RecordOutcome(t.Context(), turn.ID, domain.TurnOutcomeRecord{Status: domain.TurnSucceeded}, now); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	remoteCurrent := "remote-msg-" + assistantEntry.ID
	if err := e.db.OpenWebUILinks.SetRemoteCurrent(t.Context(), link.ID, &remoteCurrent, now); err != nil {
		t.Fatalf("set remote current: %v", err)
	}
	got, err := e.db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return got
}

// mustPendingTurn records a still-pending turn on link, replying to
// parent for target, exactly as Bridge.EnqueueTurn would when it selects
// link as target's continuation.
func (e *turnTestEnv) mustPendingTurn(t *testing.T, link domain.OpenWebUIConversationLink, parent, target domain.Entry) domain.OpenWebUITurnLink {
	t.Helper()
	now := e.clock.Now()
	turn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: target.ID, LocalParentID: &parent.ID,
		RequestID: domain.NewID(), Revision: 1, Attempt: 0, Status: domain.TurnPending,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := e.db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return turn
}
