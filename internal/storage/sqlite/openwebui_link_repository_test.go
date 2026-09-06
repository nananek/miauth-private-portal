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
