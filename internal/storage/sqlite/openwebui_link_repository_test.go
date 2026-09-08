package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// linkFixture is the set of rows a conversation link needs to exist at
// all: a workspace with a default model, a thread with a root entry, and
// the durable job that holds the claim.
type linkFixture struct {
	workspace domain.OpenWebUIWorkspace
	model     domain.OpenWebUIModel
	ownerID   string
	root      domain.Entry
	jobID     string
}

func newLinkFixture(t *testing.T, db *DB) linkFixture {
	t.Helper()
	w, m := mustSeedWorkspaceWithDefaultModel(t, db)
	ownerID := mustCreateActor(t, db)
	return linkFixture{
		workspace: w,
		model:     m,
		ownerID:   ownerID,
		root:      mustCreateThreadAndRoot(t, db, ownerID, testTime),
		jobID:     mustEnqueueJob(t, db, nil, testTime).ID,
	}
}

// claim builds and claims a link for one branch of the fixture's thread.
func (f linkFixture) claim(t *testing.T, db *DB, branchID string, at time.Time) domain.OpenWebUIConversationLink {
	t.Helper()
	l := domain.OpenWebUIConversationLink{
		ID:               domain.NewID(),
		ThreadID:         f.root.ThreadID,
		BranchID:         branchID,
		WorkspaceID:      f.workspace.ID,
		ModelID:          f.model.ID,
		ClaimJobID:       &f.jobID,
		ClaimedAt:        at,
		LastTransitionAt: at,
		CreatedAt:        at,
		UpdatedAt:        at,
	}
	if err := db.OpenWebUILinks.Claim(t.Context(), l); err != nil {
		t.Fatalf("claim link: %v", err)
	}
	return l
}

// TestOpenWebUIConversationLinkRepository_ClaimIsSingleUsePerBranch is
// the atomic claim: the first claim of a branch wins and the second is a
// conflict, because two links for one branch would mean two remote chats
// for one local conversation.
func TestOpenWebUIConversationLinkRepository_ClaimIsSingleUsePerBranch(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	first := f.claim(t, db, "branch-1", testTime)

	second := first
	second.ID = domain.NewID()
	if err := db.OpenWebUILinks.Claim(t.Context(), second); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second claim of the same branch error = %v, want ErrConflict", err)
	}

	// A different branch of the same thread is a different claim, and
	// each branch still resolves to its own link.
	other := f.claim(t, db, "branch-2", testTime.Add(time.Minute))
	for branchID, want := range map[string]string{"branch-1": first.ID, "branch-2": other.ID} {
		got, err := db.OpenWebUILinks.GetByThreadBranch(t.Context(), f.root.ThreadID, branchID)
		if err != nil {
			t.Fatalf("GetByThreadBranch(%q): %v", branchID, err)
		}
		if got.ID != want {
			t.Errorf("GetByThreadBranch(%q).ID = %q, want %q", branchID, got.ID, want)
		}
	}
}

// TestOpenWebUIConversationLinkRepository_ClaimAlwaysStartsPending pins
// that Claim writes the state itself: a caller cannot claim a branch
// straight into ready and skip the confirmation that a remote chat
// actually exists.
func TestOpenWebUIConversationLinkRepository_ClaimAlwaysStartsPending(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)

	l := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: f.root.ThreadID, BranchID: "branch-1",
		WorkspaceID: f.workspace.ID, ModelID: f.model.ID, State: domain.LinkReady,
		ClaimJobID: &f.jobID, ClaimedAt: testTime, LastTransitionAt: testTime,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
	if err := db.OpenWebUILinks.Claim(t.Context(), l); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err := db.OpenWebUILinks.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.LinkCreationPending {
		t.Errorf("State = %q, want %q regardless of what the caller passed", got.State, domain.LinkCreationPending)
	}
	if got.ReadyAt != nil || got.RemoteChatID != nil || got.RemoteCurrentID != nil || got.FailureCategory != nil {
		t.Errorf("a fresh claim should have no outcome recorded yet: %+v", got)
	}
	if got.ClaimJobID == nil || *got.ClaimJobID != f.jobID {
		t.Errorf("ClaimJobID = %v, want %q", got.ClaimJobID, f.jobID)
	}
	if !got.AllowsInitialStartChat(f.jobID) {
		t.Error("the claiming job should be allowed to issue the initial StartChat")
	}
}

func TestOpenWebUIConversationLinkRepository_Claim_RejectsUnknownReferences(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)

	base := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: f.root.ThreadID, BranchID: "branch-x",
		WorkspaceID: f.workspace.ID, ModelID: f.model.ID, ClaimJobID: &f.jobID,
		ClaimedAt: testTime, LastTransitionAt: testTime, CreatedAt: testTime, UpdatedAt: testTime,
	}
	missing := "does-not-exist"
	for name, mutate := range map[string]func(l *domain.OpenWebUIConversationLink){
		"thread":    func(l *domain.OpenWebUIConversationLink) { l.ThreadID = missing },
		"workspace": func(l *domain.OpenWebUIConversationLink) { l.WorkspaceID = missing },
		"model":     func(l *domain.OpenWebUIConversationLink) { l.ModelID = missing },
		"claim job": func(l *domain.OpenWebUIConversationLink) { l.ClaimJobID = &missing },
	} {
		l := base
		l.ID = domain.NewID()
		l.BranchID = "branch-" + name
		mutate(&l)
		if err := db.OpenWebUILinks.Claim(t.Context(), l); err == nil {
			t.Errorf("claim naming a nonexistent %s should fail the foreign key", name)
		}
	}
}

// TestOpenWebUIConversationLinkRepository_TransitionsMatchTheStateMachine
// walks the same edges domain.LinkTransition allows, but against the
// stored row: every Mark* method's WHERE clause has to reject the states
// the pure function rejects, so a stale caller racing another writer
// cannot make a transition the machine forbids.
func TestOpenWebUIConversationLinkRepository_TransitionsMatchTheStateMachine(t *testing.T) {
	later := testTime.Add(time.Hour)

	// Each case sets a link up in one state, then applies one write and
	// says what should happen.
	tests := []struct {
		name         string
		setUp        func(t *testing.T, db *DB, linkID string)
		apply        func(db *DB, linkID string) error
		wantConflict bool
		wantState    domain.LinkState
	}{
		{
			name: "pending confirmed becomes ready",
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkReady(t.Context(), id, "chat-1", nil, later)
			},
			wantState: domain.LinkReady,
		},
		{
			name: "pending definitive failure becomes failed",
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkFailed(t.Context(), id, "contract_failed", later)
			},
			wantState: domain.LinkFailed,
		},
		{
			name:      "pending uncertain creation becomes ambiguous",
			apply:     func(db *DB, id string) error { return db.OpenWebUILinks.MarkAmbiguous(t.Context(), id, later) },
			wantState: domain.LinkAmbiguous,
		},
		{
			name:      "ready uncertain continuation becomes ambiguous",
			setUp:     setUpLinkReady,
			apply:     func(db *DB, id string) error { return db.OpenWebUILinks.MarkAmbiguous(t.Context(), id, later) },
			wantState: domain.LinkAmbiguous,
		},
		{
			name:  "ambiguous owner confirmation becomes ready",
			setUp: setUpLinkAmbiguous,
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkReady(t.Context(), id, "chat-1", nil, later)
			},
			wantState: domain.LinkReady,
		},
		{
			name:      "ambiguous owner abandonment becomes dead",
			setUp:     setUpLinkAmbiguous,
			apply:     func(db *DB, id string) error { return db.OpenWebUILinks.MarkDead(t.Context(), id, "abandoned", later) },
			wantState: domain.LinkDead,
		},
		{
			name:  "ready cannot be marked ready again by a second creation",
			setUp: setUpLinkReady,
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkReady(t.Context(), id, "chat-2", nil, later)
			},
			wantConflict: true,
			wantState:    domain.LinkReady,
		},
		{
			name:  "ready cannot be marked failed",
			setUp: setUpLinkReady,
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkFailed(t.Context(), id, "contract_failed", later)
			},
			wantConflict: true,
			wantState:    domain.LinkReady,
		},
		{
			name:         "ready cannot be marked dead without passing through ambiguous",
			setUp:        setUpLinkReady,
			apply:        func(db *DB, id string) error { return db.OpenWebUILinks.MarkDead(t.Context(), id, "abandoned", later) },
			wantConflict: true,
			wantState:    domain.LinkReady,
		},
		{
			name:         "pending cannot be marked dead",
			apply:        func(db *DB, id string) error { return db.OpenWebUILinks.MarkDead(t.Context(), id, "abandoned", later) },
			wantConflict: true,
			wantState:    domain.LinkCreationPending,
		},
		{
			name:         "ambiguous cannot be marked ambiguous again",
			setUp:        setUpLinkAmbiguous,
			apply:        func(db *DB, id string) error { return db.OpenWebUILinks.MarkAmbiguous(t.Context(), id, later) },
			wantConflict: true,
			wantState:    domain.LinkAmbiguous,
		},
		{
			name:  "ambiguous cannot be marked failed",
			setUp: setUpLinkAmbiguous,
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkFailed(t.Context(), id, "contract_failed", later)
			},
			wantConflict: true,
			wantState:    domain.LinkAmbiguous,
		},
		{
			name:  "failed is terminal",
			setUp: setUpLinkFailed,
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkReady(t.Context(), id, "chat-1", nil, later)
			},
			wantConflict: true,
			wantState:    domain.LinkFailed,
		},
		{
			name:         "failed cannot become ambiguous",
			setUp:        setUpLinkFailed,
			apply:        func(db *DB, id string) error { return db.OpenWebUILinks.MarkAmbiguous(t.Context(), id, later) },
			wantConflict: true,
			wantState:    domain.LinkFailed,
		},
		{
			name:  "dead is terminal",
			setUp: setUpLinkDead,
			apply: func(db *DB, id string) error {
				return db.OpenWebUILinks.MarkReady(t.Context(), id, "chat-1", nil, later)
			},
			wantConflict: true,
			wantState:    domain.LinkDead,
		},
		{
			name:         "dead cannot become ambiguous",
			setUp:        setUpLinkDead,
			apply:        func(db *DB, id string) error { return db.OpenWebUILinks.MarkAmbiguous(t.Context(), id, later) },
			wantConflict: true,
			wantState:    domain.LinkDead,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTestDB(t)
			f := newLinkFixture(t, db)
			l := f.claim(t, db, "branch-1", testTime)
			if tt.setUp != nil {
				tt.setUp(t, db, l.ID)
			}

			err := tt.apply(db, l.ID)
			if tt.wantConflict {
				if !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("error = %v, want ErrConflict", err)
				}
			} else if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}

			got, err := db.OpenWebUILinks.Get(t.Context(), l.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
		})
	}
}

// The four setUp helpers put a claimed link into one state using the
// repository's own transitions, so a broken transition cannot quietly
// set up a test for another one.
func setUpLinkReady(t *testing.T, db *DB, linkID string) {
	t.Helper()
	if err := db.OpenWebUILinks.MarkReady(t.Context(), linkID, "chat-1", nil, testTime.Add(time.Minute)); err != nil {
		t.Fatalf("set up ready: %v", err)
	}
}

func setUpLinkAmbiguous(t *testing.T, db *DB, linkID string) {
	t.Helper()
	if err := db.OpenWebUILinks.MarkAmbiguous(t.Context(), linkID, testTime.Add(time.Minute)); err != nil {
		t.Fatalf("set up ambiguous: %v", err)
	}
}

func setUpLinkFailed(t *testing.T, db *DB, linkID string) {
	t.Helper()
	if err := db.OpenWebUILinks.MarkFailed(t.Context(), linkID, "contract_failed", testTime.Add(time.Minute)); err != nil {
		t.Fatalf("set up failed: %v", err)
	}
}

func setUpLinkDead(t *testing.T, db *DB, linkID string) {
	t.Helper()
	setUpLinkAmbiguous(t, db, linkID)
	if err := db.OpenWebUILinks.MarkDead(t.Context(), linkID, "abandoned", testTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("set up dead: %v", err)
	}
}

// TestOpenWebUIConversationLinkRepository_MarkReadyRecordsOutcome covers
// what MarkReady writes as well as which states it accepts, including
// the two COALESCEs: ready_at keeps the moment the link first had a
// chat, and a nil current-message pointer does not erase one already
// earned.
func TestOpenWebUIConversationLinkRepository_MarkReadyRecordsOutcome(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)

	firstReady := testTime.Add(time.Minute)
	current := "msg-1"
	if err := db.OpenWebUILinks.MarkReady(t.Context(), l.ID, "chat-1", &current, firstReady); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	got, err := db.OpenWebUILinks.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteChatID == nil || *got.RemoteChatID != "chat-1" {
		t.Errorf("RemoteChatID = %v, want %q", got.RemoteChatID, "chat-1")
	}
	if got.RemoteCurrentID == nil || *got.RemoteCurrentID != current {
		t.Errorf("RemoteCurrentID = %v, want %q", got.RemoteCurrentID, current)
	}
	if got.ReadyAt == nil || !got.ReadyAt.Equal(firstReady) {
		t.Fatalf("ReadyAt = %v, want %v", got.ReadyAt, firstReady)
	}
	if !got.LastTransitionAt.Equal(firstReady) {
		t.Errorf("LastTransitionAt = %v, want %v", got.LastTransitionAt, firstReady)
	}
	if !got.AllowsContinue() {
		t.Error("a ready link should allow a continuation")
	}
	if got.AllowsInitialStartChat(f.jobID) {
		t.Error("the claim is spent once the link is ready")
	}

	// Ambiguous and back: the owner confirms the same chat, without a
	// current-message pointer to offer.
	setUpLinkAmbiguous(t, db, l.ID)
	secondReady := testTime.Add(time.Hour)
	if err := db.OpenWebUILinks.MarkReady(t.Context(), l.ID, "chat-1", nil, secondReady); err != nil {
		t.Fatalf("MarkReady after ambiguous: %v", err)
	}
	got, err = db.OpenWebUILinks.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReadyAt == nil || !got.ReadyAt.Equal(firstReady) {
		t.Errorf("ReadyAt = %v, want the first ready moment %v", got.ReadyAt, firstReady)
	}
	if got.RemoteCurrentID == nil || *got.RemoteCurrentID != current {
		t.Errorf("RemoteCurrentID = %v, want the earlier %q rather than being cleared", got.RemoteCurrentID, current)
	}
	if !got.LastTransitionAt.Equal(secondReady) {
		t.Errorf("LastTransitionAt = %v, want %v", got.LastTransitionAt, secondReady)
	}
}

func TestOpenWebUIConversationLinkRepository_MarkTerminalRecordsCategory(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	failedLink := f.claim(t, db, "branch-failed", testTime)
	deadLink := f.claim(t, db, "branch-dead", testTime.Add(time.Minute))

	if err := db.OpenWebUILinks.MarkFailed(t.Context(), failedLink.ID, "auth_failed", testTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := db.OpenWebUILinks.Get(t.Context(), failedLink.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailureCategory == nil || *got.FailureCategory != "auth_failed" {
		t.Errorf("FailureCategory = %v, want %q", got.FailureCategory, "auth_failed")
	}
	if !got.IsTerminal() || got.AllowsContinue() || got.AllowsAutoRetry() {
		t.Errorf("a failed link should be terminal and permit nothing: %+v", got)
	}

	setUpLinkDead(t, db, deadLink.ID)
	got, err = db.OpenWebUILinks.Get(t.Context(), deadLink.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailureCategory == nil || *got.FailureCategory != "abandoned" {
		t.Errorf("FailureCategory = %v, want %q", got.FailureCategory, "abandoned")
	}
	if !got.IsTerminal() {
		t.Error("a dead link should be terminal")
	}
}

// TestOpenWebUIConversationLinkRepository_SetRemoteCurrentRequiresReady
// keeps the current-message pointer tied to a link that actually has a
// chat, and checks it does not disturb the state machine's own
// timestamp.
func TestOpenWebUIConversationLinkRepository_SetRemoteCurrentRequiresReady(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)

	current := "msg-1"
	if err := db.OpenWebUILinks.SetRemoteCurrent(t.Context(), l.ID, &current, testTime.Add(time.Hour)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("SetRemoteCurrent on a pending link error = %v, want ErrConflict", err)
	}

	readyAt := testTime.Add(time.Minute)
	if err := db.OpenWebUILinks.MarkReady(t.Context(), l.ID, "chat-1", nil, readyAt); err != nil {
		t.Fatal(err)
	}
	setAt := testTime.Add(time.Hour)
	if err := db.OpenWebUILinks.SetRemoteCurrent(t.Context(), l.ID, &current, setAt); err != nil {
		t.Fatalf("SetRemoteCurrent on a ready link: %v", err)
	}
	got, err := db.OpenWebUILinks.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteCurrentID == nil || *got.RemoteCurrentID != current {
		t.Errorf("RemoteCurrentID = %v, want %q", got.RemoteCurrentID, current)
	}
	if !got.LastTransitionAt.Equal(readyAt) {
		t.Errorf("LastTransitionAt = %v, want the unchanged %v: recording a correlation value is not a state change", got.LastTransitionAt, readyAt)
	}
	if !got.UpdatedAt.Equal(setAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, setAt)
	}

	// Explicit clearing is possible through this method, which is why
	// MarkReady's nil means "leave alone" rather than "clear".
	if err := db.OpenWebUILinks.SetRemoteCurrent(t.Context(), l.ID, nil, setAt); err != nil {
		t.Fatal(err)
	}
	got, err = db.OpenWebUILinks.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteCurrentID != nil {
		t.Errorf("RemoteCurrentID = %v, want nil after an explicit clear", got.RemoteCurrentID)
	}
}

// TestOpenWebUIConversationLinkRepository_RemoteChatIsUniquePerWorkspace
// backs the partial unique index: any number of links may have no remote
// chat yet, but two in one workspace may not claim the same one — that
// would be two local branches writing into one remote conversation.
func TestOpenWebUIConversationLinkRepository_RemoteChatIsUniquePerWorkspace(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	first := f.claim(t, db, "branch-1", testTime)
	second := f.claim(t, db, "branch-2", testTime.Add(time.Minute))
	third := f.claim(t, db, "branch-3", testTime.Add(2*time.Minute))

	// Three pending links coexist with no remote chat between them.
	for _, l := range []domain.OpenWebUIConversationLink{first, second, third} {
		got, err := db.OpenWebUILinks.Get(t.Context(), l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RemoteChatID != nil {
			t.Fatalf("link %q has a remote chat before being confirmed", l.ID)
		}
	}

	later := testTime.Add(time.Hour)
	if err := db.OpenWebUILinks.MarkReady(t.Context(), first.ID, "chat-1", nil, later); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenWebUILinks.MarkReady(t.Context(), second.ID, "chat-1", nil, later); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("adopting an already-claimed remote chat error = %v, want ErrConflict", err)
	}
	stillPending, err := db.OpenWebUILinks.Get(t.Context(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillPending.State != domain.LinkCreationPending {
		t.Errorf("the rejected link's state = %q, want unchanged %q", stillPending.State, domain.LinkCreationPending)
	}

	// The same remote chat id in another workspace is a different chat:
	// the id is the provider's namespace, not ours.
	otherWorkspace := mustCreateWorkspace(t, db, "https://b.example.net")
	otherModel := mustCreateModel(t, db, otherWorkspace.ID, "gpt-oss:20b", "model", testTime)
	otherLink := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: f.root.ThreadID, BranchID: "branch-elsewhere",
		WorkspaceID: otherWorkspace.ID, ModelID: otherModel.ID, ClaimJobID: &f.jobID,
		ClaimedAt: testTime, LastTransitionAt: testTime, CreatedAt: testTime, UpdatedAt: testTime,
	}
	if err := db.OpenWebUILinks.Claim(t.Context(), otherLink); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenWebUILinks.MarkReady(t.Context(), otherLink.ID, "chat-1", nil, later); err != nil {
		t.Errorf("the same remote chat id in another workspace should be allowed: %v", err)
	}

	byRemote, err := db.OpenWebUILinks.GetByRemoteChat(t.Context(), f.workspace.ID, "chat-1")
	if err != nil || byRemote.ID != first.ID {
		t.Fatalf("GetByRemoteChat = %+v, err = %v, want link %q", byRemote, err, first.ID)
	}
	if _, err := db.OpenWebUILinks.GetByRemoteChat(t.Context(), f.workspace.ID, "chat-unknown"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByRemoteChat(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestOpenWebUIConversationLinkRepository_ListByThreadIgnoresRemoteIDs is
// ADR-0005 D2 at the storage layer: a thread's links come back in local
// (created_at, id) order, and changing or removing every remote value
// does not reorder them.
func TestOpenWebUIConversationLinkRepository_ListByThreadIgnoresRemoteIDs(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)

	// Claimed newest-first, so insertion order and (created_at, id)
	// order disagree.
	third := f.claim(t, db, "branch-3", testTime.Add(2*time.Minute))
	second := f.claim(t, db, "branch-2", testTime.Add(time.Minute))
	first := f.claim(t, db, "branch-1", testTime)
	want := []string{first.ID, second.ID, third.ID}

	assertOrder := func(what string) []domain.OpenWebUIConversationLink {
		t.Helper()
		got, err := db.OpenWebUILinks.ListByThread(t.Context(), f.root.ThreadID)
		if err != nil {
			t.Fatalf("ListByThread (%s): %v", what, err)
		}
		if len(got) != len(want) {
			t.Fatalf("ListByThread (%s) returned %d links, want %d", what, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Errorf("ListByThread (%s)[%d].ID = %q, want %q", what, i, got[i].ID, want[i])
			}
		}
		return got
	}

	assertOrder("no remote ids")

	// Give them remote chat ids in the reverse of the local order. If
	// anything sorted or filtered by a remote value, this would move
	// them.
	later := testTime.Add(time.Hour)
	for i, l := range []domain.OpenWebUIConversationLink{third, second, first} {
		if err := db.OpenWebUILinks.MarkReady(t.Context(), l.ID, string(rune('a'+i))+"-chat", nil, later); err != nil {
			t.Fatal(err)
		}
	}
	assertOrder("remote ids in reverse order")

	empty, err := db.OpenWebUILinks.ListByThread(t.Context(), "does-not-exist")
	if err != nil {
		t.Fatalf("ListByThread(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByThread(unknown) returned %d links, want 0", len(empty))
	}
}

func TestOpenWebUIConversationLinkRepository_GetByThreadBranch(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)

	got, err := db.OpenWebUILinks.GetByThreadBranch(t.Context(), f.root.ThreadID, "branch-1")
	if err != nil || got.ID != l.ID {
		t.Fatalf("GetByThreadBranch = %+v, err = %v, want link %q", got, err, l.ID)
	}
	// "unlinked" is the absence of a row, which is what a caller sees
	// here for a branch nothing has claimed.
	if _, err := db.OpenWebUILinks.GetByThreadBranch(t.Context(), f.root.ThreadID, "branch-unclaimed"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByThreadBranch on an unclaimed branch error = %v, want ErrNotFound", err)
	}
	if _, err := db.OpenWebUILinks.Get(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(unknown) error = %v, want ErrNotFound", err)
	}
}

// mustCreateTurn records one turn against a link, mirroring the entries
// it names.
func mustCreateTurn(t *testing.T, db *DB, linkID, branchID string, localMessage domain.Entry, requestID string, at time.Time) domain.OpenWebUITurnLink {
	t.Helper()
	turn := domain.OpenWebUITurnLink{
		ID:             domain.NewID(),
		LinkID:         linkID,
		BranchID:       branchID,
		LocalMessageID: localMessage.ID,
		LocalParentID:  localMessage.ParentEntryID,
		RequestID:      requestID,
		Revision:       1,
		Status:         domain.TurnPending,
		CreatedAt:      at,
		UpdatedAt:      at,
	}
	if err := db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return turn
}

// mustCreateReplyEntry adds a reply to parent, so a turn has a real
// entries row (and a real parent) to name.
func mustCreateReplyEntry(t *testing.T, db *DB, parent domain.Entry, actorID string, kind domain.EntryKind, at time.Time) domain.Entry {
	t.Helper()
	e := domain.Entry{
		ID: domain.NewID(), ThreadID: parent.ThreadID, ParentEntryID: &parent.ID, Kind: kind,
		AuthorActorID: actorID, Body: "body", ProcessingStatus: domain.ProcessingNone,
		CreatedAt: at, UpdatedAt: at,
	}
	if err := db.Entries.Create(t.Context(), e); err != nil {
		t.Fatalf("create reply entry: %v", err)
	}
	return e
}

func TestOpenWebUITurnLinkRepository_CreateGetAndLookups(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)

	got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LocalMessageID != f.root.ID || got.Status != domain.TurnPending || got.Revision != 1 || got.Attempt != 0 {
		t.Errorf("Get() = %+v, want the created turn", got)
	}
	// A branch root's turn has no local parent, and no remote value has
	// been learned yet: every one of them must scan back as nil rather
	// than as an empty string.
	if got.LocalParentID != nil || got.AssistantEntryID != nil || got.TombstonedAt != nil {
		t.Errorf("optional local columns = %v/%v/%v, want all nil", got.LocalParentID, got.AssistantEntryID, got.TombstonedAt)
	}
	if got.RemoteChatID != nil || got.RemoteMessageID != nil || got.RemoteAssistantMessageID != nil ||
		got.RemoteParentID != nil || got.RemoteCurrentID != nil {
		t.Errorf("remote correlation columns = %+v, want all nil", got)
	}

	byRequest, err := db.OpenWebUITurnLinks.GetByRequestID(t.Context(), "request-1")
	if err != nil || byRequest.ID != turn.ID {
		t.Fatalf("GetByRequestID = %+v, err = %v, want turn %q", byRequest, err, turn.ID)
	}
	if _, err := db.OpenWebUITurnLinks.GetByRequestID(t.Context(), "request-unknown"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByRequestID(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestOpenWebUITurnLinkRepository_Create_UniquenessConstraints backs the
// two constraints that make recording a turn safe from a retryable path:
// the local correlation key is unique, and so is the logical turn key
// (link, local message, revision). A genuinely new revision of the same
// message is still allowed — that is a re-ask, not a duplicate.
func TestOpenWebUITurnLinkRepository_Create_UniquenessConstraints(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	existing := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)

	duplicateRequest := existing
	duplicateRequest.ID = domain.NewID()
	duplicateRequest.Revision = 2
	if err := db.OpenWebUITurnLinks.Create(t.Context(), duplicateRequest); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate request_id error = %v, want ErrConflict", err)
	}

	duplicateTurnKey := existing
	duplicateTurnKey.ID = domain.NewID()
	duplicateTurnKey.RequestID = "request-2"
	if err := db.OpenWebUITurnLinks.Create(t.Context(), duplicateTurnKey); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate (link, local message, revision) error = %v, want ErrConflict", err)
	}

	nextRevision := existing
	nextRevision.ID = domain.NewID()
	nextRevision.RequestID = "request-3"
	nextRevision.Revision = 2
	if err := db.OpenWebUITurnLinks.Create(t.Context(), nextRevision); err != nil {
		t.Errorf("a new revision of the same message should be allowed: %v", err)
	}
}

func TestOpenWebUITurnLinkRepository_Create_RejectsUnknownReferences(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)

	missing := "does-not-exist"
	base := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: l.ID, BranchID: l.BranchID, LocalMessageID: f.root.ID,
		RequestID: "request-base", Revision: 1, Status: domain.TurnPending,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
	for name, mutate := range map[string]func(turn *domain.OpenWebUITurnLink){
		"link":            func(turn *domain.OpenWebUITurnLink) { turn.LinkID = missing },
		"local message":   func(turn *domain.OpenWebUITurnLink) { turn.LocalMessageID = missing },
		"local parent":    func(turn *domain.OpenWebUITurnLink) { turn.LocalParentID = &missing },
		"assistant entry": func(turn *domain.OpenWebUITurnLink) { turn.AssistantEntryID = &missing },
	} {
		turn := base
		turn.ID = domain.NewID()
		turn.RequestID = "request-" + name
		mutate(&turn)
		if err := db.OpenWebUITurnLinks.Create(t.Context(), turn); err == nil {
			t.Errorf("a turn naming a nonexistent %s should fail the foreign key", name)
		}
	}

	unknownStatus := base
	unknownStatus.ID = domain.NewID()
	unknownStatus.RequestID = "request-status"
	unknownStatus.Status = domain.TurnProviderStatus("probably_fine")
	if err := db.OpenWebUITurnLinks.Create(t.Context(), unknownStatus); err == nil {
		t.Error("an unknown provider status should fail the CHECK constraint")
	}
}

// TestOpenWebUITurnLinkRepository_MirrorsTheLocalReplyTree is the local
// reply-tree contract: a turn's local parent is whatever the entries
// table says the owner's message replied to, for a whole
// owner/assistant/owner/assistant chain, and no remote value is involved
// in establishing any of it.
func TestOpenWebUITurnLinkRepository_MirrorsTheLocalReplyTree(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	virtualActorID := mustCreateVirtualActor(t, db)

	// M0 (the root, by the owner) -> A0 -> M1 -> A1.
	assistant0 := mustCreateReplyEntry(t, db, f.root, virtualActorID, domain.EntryLLMReply, testTime.Add(time.Minute))
	message1 := mustCreateReplyEntry(t, db, assistant0, f.ownerID, domain.EntryUserPost, testTime.Add(2*time.Minute))
	assistant1 := mustCreateReplyEntry(t, db, message1, virtualActorID, domain.EntryLLMReply, testTime.Add(3*time.Minute))

	turn0 := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-0", testTime)
	turn1 := mustCreateTurn(t, db, l.ID, l.BranchID, message1, "request-1", testTime.Add(2*time.Minute))
	for turn, assistantEntry := range map[string]domain.Entry{turn0.ID: assistant0, turn1.ID: assistant1} {
		if err := db.OpenWebUITurnLinks.SetAssistantEntry(t.Context(), turn, assistantEntry.ID, testTime.Add(time.Hour)); err != nil {
			t.Fatalf("SetAssistantEntry: %v", err)
		}
	}

	got, err := db.OpenWebUITurnLinks.ListByLink(t.Context(), l.ID)
	if err != nil {
		t.Fatalf("ListByLink: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByLink returned %d turns, want 2", len(got))
	}
	if got[0].ID != turn0.ID || got[1].ID != turn1.ID {
		t.Fatalf("ListByLink order = %q, %q; want %q, %q", got[0].ID, got[1].ID, turn0.ID, turn1.ID)
	}
	if got[0].LocalParentID != nil {
		t.Errorf("the root turn's LocalParentID = %v, want nil", got[0].LocalParentID)
	}
	if got[1].LocalParentID == nil || *got[1].LocalParentID != assistant0.ID {
		t.Errorf("the second turn's LocalParentID = %v, want the first assistant entry %q", got[1].LocalParentID, assistant0.ID)
	}

	// The turn's local parent is exactly the entry's own parent: the
	// reply tree is read from entries, never reconstructed from a
	// correlation value.
	storedEntry, err := db.Entries.Get(t.Context(), message1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedEntry.ParentEntryID == nil || *got[1].LocalParentID != *storedEntry.ParentEntryID {
		t.Errorf("turn local parent %v disagrees with the entry's parent %v", got[1].LocalParentID, storedEntry.ParentEntryID)
	}
	if got[0].AssistantEntryID == nil || *got[0].AssistantEntryID != assistant0.ID {
		t.Errorf("the root turn's AssistantEntryID = %v, want %q", got[0].AssistantEntryID, assistant0.ID)
	}
}

// TestOpenWebUITurnLinkRepository_ListByLinkIgnoresRemoteIDs is the turn
// half of ADR-0005 D2: recording correlation values, in any order and
// with any values, must not change the order turns are read back in.
func TestOpenWebUITurnLinkRepository_ListByLinkIgnoresRemoteIDs(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)

	// Recorded newest-first so insertion order and (created_at, id)
	// order disagree.
	entries := []domain.Entry{f.root}
	parent := f.root
	for i := range 2 {
		parent = mustCreateReplyEntry(t, db, parent, f.ownerID, domain.EntryUserPost, testTime.Add(time.Duration(i+1)*time.Minute))
		entries = append(entries, parent)
	}
	third := mustCreateTurn(t, db, l.ID, l.BranchID, entries[2], "request-3", testTime.Add(2*time.Minute))
	second := mustCreateTurn(t, db, l.ID, l.BranchID, entries[1], "request-2", testTime.Add(time.Minute))
	first := mustCreateTurn(t, db, l.ID, l.BranchID, entries[0], "request-1", testTime)
	want := []string{first.ID, second.ID, third.ID}

	assertOrder := func(what string) {
		t.Helper()
		got, err := db.OpenWebUITurnLinks.ListByLink(t.Context(), l.ID)
		if err != nil {
			t.Fatalf("ListByLink (%s): %v", what, err)
		}
		if len(got) != len(want) {
			t.Fatalf("ListByLink (%s) returned %d turns, want %d", what, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Errorf("ListByLink (%s)[%d].ID = %q, want %q", what, i, got[i].ID, want[i])
			}
		}
	}

	assertOrder("no remote ids")

	// Correlation values assigned in the reverse of the local order.
	for i, turn := range []domain.OpenWebUITurnLink{third, second, first} {
		id := string(rune('a'+i)) + "-value"
		corr := domain.OpenWebUITurnCorrelation{
			RemoteChatID: &id, RemoteMessageID: &id, RemoteAssistantMessageID: &id,
			RemoteParentID: &id, RemoteCurrentID: &id,
		}
		if err := db.OpenWebUITurnLinks.SetRemoteCorrelation(t.Context(), turn.ID, corr, testTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	assertOrder("remote ids in reverse order")
}

func TestOpenWebUITurnLinkRepository_SetRemoteCorrelation(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)

	chatID, messageID, assistantID := "chat-1", "user-msg-1", "assistant-msg-1"
	later := testTime.Add(time.Hour)
	corr := domain.OpenWebUITurnCorrelation{
		RemoteChatID:             &chatID,
		RemoteMessageID:          &messageID,
		RemoteAssistantMessageID: &assistantID,
	}
	if err := db.OpenWebUITurnLinks.SetRemoteCorrelation(t.Context(), turn.ID, corr, later); err != nil {
		t.Fatalf("SetRemoteCorrelation: %v", err)
	}
	got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteChatID == nil || *got.RemoteChatID != chatID ||
		got.RemoteMessageID == nil || *got.RemoteMessageID != messageID ||
		got.RemoteAssistantMessageID == nil || *got.RemoteAssistantMessageID != assistantID {
		t.Errorf("recorded correlation = %+v, want %+v", got, corr)
	}
	// The fields the caller did not hold stay NULL rather than becoming
	// empty strings.
	if got.RemoteParentID != nil || got.RemoteCurrentID != nil {
		t.Errorf("unset correlation fields = %v/%v, want nil", got.RemoteParentID, got.RemoteCurrentID)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, later)
	}

	// The write replaces the correlation rather than merging into it, so
	// an empty one clears every column.
	if err := db.OpenWebUITurnLinks.SetRemoteCorrelation(t.Context(), turn.ID, domain.OpenWebUITurnCorrelation{}, later); err != nil {
		t.Fatal(err)
	}
	got, err = db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteChatID != nil || got.RemoteMessageID != nil || got.RemoteAssistantMessageID != nil {
		t.Errorf("correlation after an empty write = %+v, want every field nil", got)
	}

	if err := db.OpenWebUITurnLinks.SetRemoteCorrelation(t.Context(), "does-not-exist", corr, later); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetRemoteCorrelation on an unknown turn error = %v, want ErrNotFound", err)
	}
}

func TestOpenWebUITurnLinkRepository_SetProviderStatusAndAssistantEntry(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)
	virtualActorID := mustCreateVirtualActor(t, db)
	assistantEntry := mustCreateReplyEntry(t, db, f.root, virtualActorID, domain.EntryLLMReply, testTime.Add(time.Minute))
	later := testTime.Add(time.Hour)

	// A turn's status is a record, not a machine: it moves between
	// categories in whatever order the observations arrive.
	for _, status := range []domain.TurnProviderStatus{
		domain.TurnAmbiguous, domain.TurnAuthFailed, domain.TurnContractFailed,
		domain.TurnFailed, domain.TurnCancelled, domain.TurnSucceeded,
	} {
		if err := db.OpenWebUITurnLinks.SetProviderStatus(t.Context(), turn.ID, status, 2, later); err != nil {
			t.Fatalf("SetProviderStatus(%q): %v", status, err)
		}
		got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != status || got.Attempt != 2 {
			t.Errorf("Status/Attempt = %q/%d, want %q/2", got.Status, got.Attempt, status)
		}
	}

	if err := db.OpenWebUITurnLinks.SetAssistantEntry(t.Context(), turn.ID, assistantEntry.ID, later); err != nil {
		t.Fatalf("SetAssistantEntry: %v", err)
	}
	got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AssistantEntryID == nil || *got.AssistantEntryID != assistantEntry.ID {
		t.Errorf("AssistantEntryID = %v, want %q", got.AssistantEntryID, assistantEntry.ID)
	}

	if err := db.OpenWebUITurnLinks.SetProviderStatus(t.Context(), "does-not-exist", domain.TurnSucceeded, 1, later); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetProviderStatus on an unknown turn error = %v, want ErrNotFound", err)
	}
	if err := db.OpenWebUITurnLinks.SetAssistantEntry(t.Context(), "does-not-exist", assistantEntry.ID, later); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetAssistantEntry on an unknown turn error = %v, want ErrNotFound", err)
	}
}

// TestOpenWebUITurnLinkRepository_TombstoneIsSingleUse keeps a
// superseded turn's record intact: the row and the entries it names stay
// put, and a second tombstone is a conflict rather than a silent rewrite
// of when the first one happened.
func TestOpenWebUITurnLinkRepository_TombstoneIsSingleUse(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)

	tombstonedAt := testTime.Add(time.Hour)
	if err := db.OpenWebUITurnLinks.Tombstone(t.Context(), turn.ID, tombstonedAt); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatalf("a tombstoned turn should still be readable: %v", err)
	}
	if got.TombstonedAt == nil || !got.TombstonedAt.Equal(tombstonedAt) {
		t.Errorf("TombstonedAt = %v, want %v", got.TombstonedAt, tombstonedAt)
	}

	if err := db.OpenWebUITurnLinks.Tombstone(t.Context(), turn.ID, testTime.Add(2*time.Hour)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second Tombstone error = %v, want ErrConflict", err)
	}
	after, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.TombstonedAt == nil || !after.TombstonedAt.Equal(tombstonedAt) {
		t.Errorf("TombstonedAt after a rejected second tombstone = %v, want unchanged %v", after.TombstonedAt, tombstonedAt)
	}
	// The entry the turn named is untouched: hiding a note is a separate
	// concern (ADR-0004).
	if _, err := db.Entries.Get(t.Context(), f.root.ID); err != nil {
		t.Errorf("the turn's local message entry should be unaffected by a tombstone: %v", err)
	}

	if err := db.OpenWebUITurnLinks.Tombstone(t.Context(), "does-not-exist", tombstonedAt); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Tombstone on an unknown turn error = %v, want ErrConflict", err)
	}
}

// TestOpenWebUIConversationLinkRepository_MarkReadyPinsToTheAlreadyStoredRemoteChat
// backs Issue #52's handoff item 1: once a link has recorded a
// remote_chat_id, MarkReady may only confirm it onto that same chat again
// (the owner-recovery path an ambiguous link with a partial creation
// result takes), never silently switch it to a different one.
func TestOpenWebUIConversationLinkRepository_MarkReadyPinsToTheAlreadyStoredRemoteChat(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)

	// A pending link with no remote_chat_id yet accepts any chat id: the
	// ordinary creation-confirmed path.
	fresh := f.claim(t, db, "branch-fresh", testTime)
	if err := db.OpenWebUILinks.MarkReady(t.Context(), fresh.ID, "chat-any", nil, testTime.Add(time.Minute)); err != nil {
		t.Fatalf("MarkReady on a link with no stored remote chat: %v", err)
	}

	// A link that already recorded "chat-a" (via an earlier MarkReady,
	// then frozen back to ambiguous) may be confirmed onto "chat-a"
	// again, but not onto "chat-b".
	pinned := f.claim(t, db, "branch-pinned", testTime.Add(2*time.Minute))
	readyAt := testTime.Add(3 * time.Minute)
	if err := db.OpenWebUILinks.MarkReady(t.Context(), pinned.ID, "chat-a", nil, readyAt); err != nil {
		t.Fatalf("initial MarkReady: %v", err)
	}
	setUpLinkAmbiguous(t, db, pinned.ID)

	if err := db.OpenWebUILinks.MarkReady(t.Context(), pinned.ID, "chat-b", nil, testTime.Add(4*time.Minute)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("MarkReady onto a different remote chat error = %v, want ErrConflict", err)
	}
	stillAmbiguous, err := db.OpenWebUILinks.Get(t.Context(), pinned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillAmbiguous.State != domain.LinkAmbiguous || stillAmbiguous.RemoteChatID == nil || *stillAmbiguous.RemoteChatID != "chat-a" {
		t.Errorf("link after the rejected MarkReady = %+v, want unchanged ambiguous/chat-a", stillAmbiguous)
	}

	if err := db.OpenWebUILinks.MarkReady(t.Context(), pinned.ID, "chat-a", nil, testTime.Add(5*time.Minute)); err != nil {
		t.Errorf("MarkReady onto the same already-stored remote chat: %v", err)
	}
	confirmed, err := db.OpenWebUILinks.Get(t.Context(), pinned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.State != domain.LinkReady {
		t.Errorf("State = %q after confirming the same chat, want %q", confirmed.State, domain.LinkReady)
	}
}

// TestOpenWebUIConversationLinkRepository_List backs owner-facing
// recovery tooling's need to enumerate links by state without a thread
// id, unlike every other lookup in this file.
func TestOpenWebUIConversationLinkRepository_List(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)

	pending := f.claim(t, db, "branch-pending", testTime)
	ambiguous := f.claim(t, db, "branch-ambiguous", testTime.Add(time.Minute))
	setUpLinkAmbiguous(t, db, ambiguous.ID)
	ready := f.claim(t, db, "branch-ready", testTime.Add(2*time.Minute))
	setUpLinkReady(t, db, ready.ID)

	all, err := db.OpenWebUILinks.List(t.Context(), domain.OpenWebUILinkFilter{})
	if err != nil {
		t.Fatalf("List(no filter): %v", err)
	}
	wantAll := []string{pending.ID, ambiguous.ID, ready.ID}
	if len(all) != len(wantAll) {
		t.Fatalf("List(no filter) returned %d links, want %d", len(all), len(wantAll))
	}
	for i, id := range wantAll {
		if all[i].ID != id {
			t.Errorf("List(no filter)[%d].ID = %q, want %q", i, all[i].ID, id)
		}
	}

	ambiguousState := domain.LinkAmbiguous
	byState, err := db.OpenWebUILinks.List(t.Context(), domain.OpenWebUILinkFilter{State: &ambiguousState})
	if err != nil {
		t.Fatalf("List(state=ambiguous): %v", err)
	}
	if len(byState) != 1 || byState[0].ID != ambiguous.ID {
		t.Fatalf("List(state=ambiguous) = %+v, want only %q", byState, ambiguous.ID)
	}

	threadID := f.root.ThreadID
	byThread, err := db.OpenWebUILinks.List(t.Context(), domain.OpenWebUILinkFilter{ThreadID: &threadID})
	if err != nil {
		t.Fatalf("List(thread): %v", err)
	}
	if len(byThread) != len(wantAll) {
		t.Errorf("List(thread) returned %d links, want %d", len(byThread), len(wantAll))
	}

	unknownThread := "does-not-exist"
	empty, err := db.OpenWebUILinks.List(t.Context(), domain.OpenWebUILinkFilter{ThreadID: &unknownThread})
	if err != nil {
		t.Fatalf("List(unknown thread): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("List(unknown thread) returned %d links, want 0", len(empty))
	}

	limited, err := db.OpenWebUILinks.List(t.Context(), domain.OpenWebUILinkFilter{Limit: 1})
	if err != nil {
		t.Fatalf("List(limit=1): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != pending.ID {
		t.Fatalf("List(limit=1) = %+v, want only the first link %q", limited, pending.ID)
	}
}

// TestOpenWebUITurnLinkRepository_BeginAttempt backs ADR-0005 D7: the
// attempt count and last_attempt_at must be durably recorded before a
// provider call is made, so a crash or lease expiry afterward is
// distinguishable, on the next run, from a turn that never attempted
// anything.
func TestOpenWebUITurnLinkRepository_BeginAttempt(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)

	at := testTime.Add(time.Minute)
	if err := db.OpenWebUITurnLinks.BeginAttempt(t.Context(), turn.ID, 1, at); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", got.Attempt)
	}
	if got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(at) {
		t.Errorf("LastAttemptAt = %v, want %v", got.LastAttemptAt, at)
	}
	if got.Status != domain.TurnPending {
		t.Errorf("Status = %q, want unchanged %q: BeginAttempt records only the attempt", got.Status, domain.TurnPending)
	}

	if err := db.OpenWebUITurnLinks.BeginAttempt(t.Context(), "does-not-exist", 1, at); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("BeginAttempt on an unknown turn error = %v, want ErrNotFound", err)
	}
}

// TestOpenWebUITurnLinkRepository_RecordOutcome covers what RecordOutcome
// writes together (status, failure category, usage, finish reason) and
// completed_at's two rules: it is set only for a terminal status, and a
// later terminal write overwrites it rather than keeping the first one
// (unlike a link's ready_at).
func TestOpenWebUITurnLinkRepository_RecordOutcome(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)

	// TurnPending is the only non-terminal status, so it is the one that
	// must leave completed_at unset.
	pendingAt := testTime.Add(time.Minute)
	if err := db.OpenWebUITurnLinks.RecordOutcome(t.Context(), turn.ID, domain.TurnOutcomeRecord{
		Status: domain.TurnPending,
	}, pendingAt); err != nil {
		t.Fatalf("RecordOutcome(pending): %v", err)
	}
	got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt after a non-terminal RecordOutcome = %v, want nil", got.CompletedAt)
	}

	firstTerminalAt := testTime.Add(2 * time.Minute)
	authFailed := domain.FailureCategoryAuthFailed
	if err := db.OpenWebUITurnLinks.RecordOutcome(t.Context(), turn.ID, domain.TurnOutcomeRecord{
		Status:          domain.TurnAuthFailed,
		FailureCategory: &authFailed,
	}, firstTerminalAt); err != nil {
		t.Fatalf("RecordOutcome(auth_failed): %v", err)
	}
	got, err = db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TurnAuthFailed {
		t.Errorf("Status = %q, want %q", got.Status, domain.TurnAuthFailed)
	}
	if got.FailureCategory == nil || *got.FailureCategory != domain.FailureCategoryAuthFailed {
		t.Errorf("FailureCategory = %v, want %q", got.FailureCategory, domain.FailureCategoryAuthFailed)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(firstTerminalAt) {
		t.Errorf("CompletedAt = %v, want %v", got.CompletedAt, firstTerminalAt)
	}

	// A later terminal write (owner recovery resolving an earlier
	// ambiguous/failed outcome to succeeded) overwrites both the status
	// and completed_at, and clears the failure category by omitting it.
	secondTerminalAt := testTime.Add(3 * time.Minute)
	promptTokens, completionTokens := 12, 34
	finishReason := "stop"
	if err := db.OpenWebUITurnLinks.RecordOutcome(t.Context(), turn.ID, domain.TurnOutcomeRecord{
		Status:           domain.TurnSucceeded,
		PromptTokens:     &promptTokens,
		CompletionTokens: &completionTokens,
		FinishReason:     &finishReason,
	}, secondTerminalAt); err != nil {
		t.Fatalf("RecordOutcome(succeeded): %v", err)
	}
	got, err = db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TurnSucceeded {
		t.Errorf("Status = %q, want %q", got.Status, domain.TurnSucceeded)
	}
	if got.FailureCategory != nil {
		t.Errorf("FailureCategory = %v, want nil after a write that named none", got.FailureCategory)
	}
	if got.PromptTokens == nil || *got.PromptTokens != promptTokens {
		t.Errorf("PromptTokens = %v, want %d", got.PromptTokens, promptTokens)
	}
	if got.CompletionTokens == nil || *got.CompletionTokens != completionTokens {
		t.Errorf("CompletionTokens = %v, want %d", got.CompletionTokens, completionTokens)
	}
	if got.FinishReason == nil || *got.FinishReason != finishReason {
		t.Errorf("FinishReason = %v, want %q", got.FinishReason, finishReason)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(secondTerminalAt) {
		t.Errorf("CompletedAt = %v, want the later %v, not the first terminal write", got.CompletedAt, secondTerminalAt)
	}

	if err := db.OpenWebUITurnLinks.RecordOutcome(t.Context(), "does-not-exist", domain.TurnOutcomeRecord{Status: domain.TurnFailed}, secondTerminalAt); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("RecordOutcome on an unknown turn error = %v, want ErrNotFound", err)
	}
}

// TestOpenWebUITurnLinkRepository_RecordOutcome_TitleAndSources is Issues
// #81/#84's own addition to RecordOutcome: Title and Sources persist and
// round-trip through Get exactly like every other outcome field, and a
// later write that omits them clears them the same way an omitted
// FailureCategory already does (see
// TestOpenWebUITurnLinkRepository_RecordOutcome above) — RecordOutcome
// always overwrites, never merges.
func TestOpenWebUITurnLinkRepository_RecordOutcome_TitleAndSources(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)

	title := "A generated chat title"
	url := "https://example.com/result"
	sources := []domain.Source{
		{Kind: "tool", DisplayName: "get_weather", Arguments: map[string]string{"city": "Tokyo"}},
		{Kind: "web_search", DisplayName: "web_search", URL: &url},
	}
	at := testTime.Add(time.Minute)
	if err := db.OpenWebUITurnLinks.RecordOutcome(t.Context(), turn.ID, domain.TurnOutcomeRecord{
		Status: domain.TurnSucceeded, Title: &title, Sources: sources,
	}, at); err != nil {
		t.Fatalf("RecordOutcome(title+sources): %v", err)
	}
	got, err := db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteChatTitle == nil || *got.RemoteChatTitle != title {
		t.Errorf("RemoteChatTitle = %v, want %q", got.RemoteChatTitle, title)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("Sources = %+v, want 2 entries", got.Sources)
	}
	if got.Sources[0].Kind != "tool" || got.Sources[0].Arguments["city"] != "Tokyo" {
		t.Errorf("Sources[0] = %+v, want the tool source", got.Sources[0])
	}
	if got.Sources[1].URL == nil || *got.Sources[1].URL != url {
		t.Errorf("Sources[1].URL = %v, want %q", got.Sources[1].URL, url)
	}

	// A later write that names neither clears both, the same
	// always-overwrite rule FailureCategory already follows.
	laterAt := testTime.Add(2 * time.Minute)
	if err := db.OpenWebUITurnLinks.RecordOutcome(t.Context(), turn.ID, domain.TurnOutcomeRecord{
		Status: domain.TurnSucceeded,
	}, laterAt); err != nil {
		t.Fatalf("RecordOutcome(no title/sources): %v", err)
	}
	got, err = db.OpenWebUITurnLinks.Get(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteChatTitle != nil {
		t.Errorf("RemoteChatTitle after an omitting write = %v, want nil", got.RemoteChatTitle)
	}
	if got.Sources != nil {
		t.Errorf("Sources after an omitting write = %+v, want nil", got.Sources)
	}
}

// TestOpenWebUITurnLinkRepository_GetByAssistantEntry is Issues #81/#84's
// wire-projection enrichment lookup: it resolves a turn from the entry
// its own reply authored, and ErrNotFound for any entry that is not one
// (an Issue #9 plain LLM reply, or any other entry kind), matching the
// interface doc comment's own contract.
func TestOpenWebUITurnLinkRepository_GetByAssistantEntry(t *testing.T) {
	db := newTestDB(t)
	f := newLinkFixture(t, db)
	l := f.claim(t, db, "branch-1", testTime)
	turn := mustCreateTurn(t, db, l.ID, l.BranchID, f.root, "request-1", testTime)
	virtualActorID := mustCreateVirtualActor(t, db)
	assistantEntry := mustCreateReplyEntry(t, db, f.root, virtualActorID, domain.EntryLLMReply, testTime.Add(time.Minute))

	if _, err := db.OpenWebUITurnLinks.GetByAssistantEntry(t.Context(), assistantEntry.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByAssistantEntry before SetAssistantEntry error = %v, want ErrNotFound", err)
	}

	if err := db.OpenWebUITurnLinks.SetAssistantEntry(t.Context(), turn.ID, assistantEntry.ID, testTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("SetAssistantEntry: %v", err)
	}
	got, err := db.OpenWebUITurnLinks.GetByAssistantEntry(t.Context(), assistantEntry.ID)
	if err != nil {
		t.Fatalf("GetByAssistantEntry: %v", err)
	}
	if got.ID != turn.ID {
		t.Errorf("GetByAssistantEntry id = %q, want %q", got.ID, turn.ID)
	}

	// An Issue #9 plain LLM reply (or any other entry never named by
	// SetAssistantEntry) has no turn at all.
	if _, err := db.OpenWebUITurnLinks.GetByAssistantEntry(t.Context(), f.root.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByAssistantEntry(unrelated entry) error = %v, want ErrNotFound", err)
	}
}
