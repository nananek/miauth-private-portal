package timeline

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestCreateRoot_AssignsActorForEveryRootKind(t *testing.T) {
	ts := newTestService(t)

	tests := []struct {
		kind      domain.EntryKind
		actorType domain.ActorType
	}{
		{kind: domain.EntryUserPost, actorType: domain.ActorOwner},
		{kind: domain.EntryNews, actorType: domain.ActorSystem},
		{kind: domain.EntryMail, actorType: domain.ActorSystem},
		{kind: domain.EntrySystem, actorType: domain.ActorSystem},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			entry, err := ts.CreateRoot(t.Context(), tt.kind, "body", nil)
			if err != nil {
				t.Fatalf("CreateRoot: %v", err)
			}
			if !entry.IsRoot() || entry.ID != entry.ThreadID {
				t.Errorf("root topology is invalid: %+v", entry)
			}
			if entry.ProcessingStatus != domain.ProcessingNone {
				t.Errorf("ProcessingStatus = %q, want none", entry.ProcessingStatus)
			}
			actor, err := ts.db.Actors.GetByType(t.Context(), tt.actorType)
			if err != nil {
				t.Fatal(err)
			}
			if entry.AuthorActorID != actor.ID {
				t.Errorf("AuthorActorID = %q, want %s actor %q", entry.AuthorActorID, tt.actorType, actor.ID)
			}
			thread, err := ts.db.Threads.Get(t.Context(), entry.ThreadID)
			if err != nil {
				t.Fatal(err)
			}
			if !thread.CreatedAt.Equal(ts.clock.Now()) || !thread.UpdatedAt.Equal(ts.clock.Now()) {
				t.Errorf("thread timestamps = (%v, %v), want %v", thread.CreatedAt, thread.UpdatedAt, ts.clock.Now())
			}
		})
	}
}

func TestCreateRoot_RejectsReplyAndUnknownKinds(t *testing.T) {
	ts := newTestService(t)
	for _, kind := range []domain.EntryKind{domain.EntryLLMReply, domain.EntryLLMFollowUp, "unknown"} {
		if _, err := ts.CreateRoot(t.Context(), kind, "body", nil); !errors.Is(err, ErrInvalidKind) {
			t.Errorf("CreateRoot(%q) error = %v, want ErrInvalidKind", kind, err)
		}
	}

	entries, err := ts.GetTimeline(t.Context(), domain.Page{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("invalid root calls persisted %d entries, want 0", len(entries))
	}
}

func TestCreateReply_DerivesThreadFromParentAndTouchesIt(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	createdThread, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}

	ts.clock.Advance(time.Minute)
	job := newTestJob(ts.clock.Now(), nil)
	reply, err := ts.CreateReply(t.Context(), root.ID, domain.EntryLLMReply, "reply", &job)
	if err != nil {
		t.Fatalf("CreateReply: %v", err)
	}
	if reply.ThreadID != root.ThreadID {
		t.Errorf("reply.ThreadID = %q, want parent's %q", reply.ThreadID, root.ThreadID)
	}
	if reply.ParentEntryID == nil || *reply.ParentEntryID != root.ID {
		t.Errorf("reply.ParentEntryID = %v, want %q", reply.ParentEntryID, root.ID)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	if reply.AuthorActorID != assistant.ID {
		t.Errorf("reply.AuthorActorID = %q, want assistant %q", reply.AuthorActorID, assistant.ID)
	}
	storedJob, err := ts.db.Jobs.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedJob.SourceEntryID == nil || *storedJob.SourceEntryID != reply.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", storedJob.SourceEntryID, reply.ID)
	}

	touchedThread, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !touchedThread.UpdatedAt.Equal(ts.clock.Now()) || !touchedThread.UpdatedAt.After(createdThread.UpdatedAt) {
		t.Errorf("thread UpdatedAt = %v, want touched at %v", touchedThread.UpdatedAt, ts.clock.Now())
	}
}

func TestCreateReply_RejectsMissingParent(t *testing.T) {
	ts := newTestService(t)
	_, err := ts.CreateReply(t.Context(), "missing", domain.EntryUserPost, "reply", nil)
	if !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("CreateReply() error = %v, want ErrParentNotFound", err)
	}
	entries, listErr := ts.GetTimeline(t.Context(), domain.Page{}, true)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(entries) != 0 {
		t.Errorf("missing-parent reply persisted %d entries, want 0", len(entries))
	}
}

func TestCreateRoot_EnqueuesJobForEntryAtomically(t *testing.T) {
	ts := newTestService(t)
	job := newTestJob(ts.clock.Now(), nil)

	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", &job)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ts.db.Jobs.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SourceEntryID == nil || *stored.SourceEntryID != entry.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", stored.SourceEntryID, entry.ID)
	}
	if job.SourceEntryID != nil {
		t.Errorf("CreateRoot mutated caller's job: SourceEntryID = %v", job.SourceEntryID)
	}
}

func TestCreateRoot_RollsBackEntryAndThreadWhenJobConflicts(t *testing.T) {
	ts := newTestService(t)
	key := "duplicate-root"
	seed := newTestJob(ts.clock.Now(), &key)
	if err := ts.db.Jobs.Enqueue(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	conflict := newTestJob(ts.clock.Now(), &key)

	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", &conflict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CreateRoot() error = %v, want ErrConflict", err)
	}
	entries, err := ts.GetTimeline(t.Context(), domain.Page{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed CreateRoot persisted %d entries, want 0", len(entries))
	}
}

func TestCreateReply_RollsBackEntryAndThreadTouchWhenJobConflicts(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	key := "duplicate-reply"
	seed := newTestJob(ts.clock.Now(), &key)
	if err := ts.db.Jobs.Enqueue(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	conflict := newTestJob(ts.clock.Now(), &key)

	if _, err := ts.CreateReply(t.Context(), root.ID, domain.EntryLLMReply, "reply", &conflict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CreateReply() error = %v, want ErrConflict", err)
	}
	thread, err := ts.GetThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 || thread[0].ID != root.ID {
		t.Errorf("failed CreateReply left thread entries = %v, want only root", thread)
	}
	after, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("failed CreateReply touched thread at %v, want unchanged %v", after.UpdatedAt, before.UpdatedAt)
	}
}

func TestCreateGeneratedReply_CreatesEntryAndCompletesGeneration(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatal(err)
	}

	ts.clock.Advance(time.Minute)
	promptTokens, completionTokens := 12, 34
	reply, err := ts.CreateGeneratedReply(t.Context(), root.ID, domain.EntryLLMReply, "generated reply", gen.ID, &promptTokens, &completionTokens)
	if err != nil {
		t.Fatalf("CreateGeneratedReply: %v", err)
	}
	if reply.ThreadID != root.ThreadID {
		t.Errorf("reply.ThreadID = %q, want parent's %q", reply.ThreadID, root.ThreadID)
	}
	if reply.ParentEntryID == nil || *reply.ParentEntryID != root.ID {
		t.Errorf("reply.ParentEntryID = %v, want %q", reply.ParentEntryID, root.ID)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	if reply.AuthorActorID != assistant.ID {
		t.Errorf("reply.AuthorActorID = %q, want assistant %q", reply.AuthorActorID, assistant.ID)
	}

	stored, err := ts.db.Generations.Get(t.Context(), gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.GenerationComplete {
		t.Errorf("generation Status = %q, want complete", stored.Status)
	}
	if stored.ResultEntryID == nil || *stored.ResultEntryID != reply.ID {
		t.Errorf("generation ResultEntryID = %v, want %q", stored.ResultEntryID, reply.ID)
	}
	if stored.Body == nil || *stored.Body != "generated reply" {
		t.Errorf("generation Body = %v, want %q", stored.Body, "generated reply")
	}
	if stored.PromptTokens == nil || *stored.PromptTokens != 12 || stored.CompletionTokens == nil || *stored.CompletionTokens != 34 {
		t.Errorf("generation tokens = (%v, %v), want (12, 34)", stored.PromptTokens, stored.CompletionTokens)
	}
	if stored.GeneratedAt == nil || !stored.GeneratedAt.Equal(ts.clock.Now()) {
		t.Errorf("generation GeneratedAt = %v, want %v", stored.GeneratedAt, ts.clock.Now())
	}
}

func TestCreateGeneratedReply_RejectsNonAssistantKind(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []domain.EntryKind{domain.EntryUserPost, domain.EntryNews, domain.EntryMail, domain.EntrySystem, "unknown"} {
		if _, err := ts.CreateGeneratedReply(t.Context(), root.ID, kind, "body", gen.ID, nil, nil); !errors.Is(err, ErrInvalidKind) {
			t.Errorf("CreateGeneratedReply(%q) error = %v, want ErrInvalidKind", kind, err)
		}
	}
}

// TestCreateGeneratedReply_RollsBackEntryWhenGenerationNotPending verifies
// the same atomicity CreateReply's job-conflict tests give a durable job
// intent: if the generation row is no longer pending (already completed
// or failed by an earlier delivery of the same job), the reply entry and
// thread touch must not be persisted either.
func TestCreateGeneratedReply_RollsBackEntryWhenGenerationNotPending(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatal(err)
	}
	if err := ts.db.Generations.Fail(t.Context(), gen.ID, "test_failure", ts.clock.Now()); err != nil {
		t.Fatal(err)
	}

	ts.clock.Advance(time.Minute)
	if _, err := ts.CreateGeneratedReply(t.Context(), root.ID, domain.EntryLLMReply, "generated", gen.ID, nil, nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CreateGeneratedReply() error = %v, want ErrConflict", err)
	}

	thread, err := ts.GetThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 || thread[0].ID != root.ID {
		t.Errorf("failed CreateGeneratedReply left thread entries = %v, want only root", thread)
	}
	after, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("failed CreateGeneratedReply touched thread at %v, want unchanged %v", after.UpdatedAt, before.UpdatedAt)
	}
}

func TestEditPost_UpdatesOwnersPostAndEnqueuesJob(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "before", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Hour)
	job := newTestJob(ts.clock.Now(), nil)

	edited, err := ts.EditPost(t.Context(), root.ID, ts.owner.ID, "after", &job)
	if err != nil {
		t.Fatalf("EditPost: %v", err)
	}
	if edited.Body != "after" || !edited.UpdatedAt.Equal(ts.clock.Now()) {
		t.Errorf("edited entry = %+v, want body and UpdatedAt changed", edited)
	}
	if !edited.CreatedAt.Equal(root.CreatedAt) {
		t.Errorf("CreatedAt changed from %v to %v", root.CreatedAt, edited.CreatedAt)
	}
	stored := requireEntryBody(t, ts.db, root.ID, "after")
	if !stored.UpdatedAt.Equal(ts.clock.Now()) {
		t.Errorf("stored UpdatedAt = %v, want %v", stored.UpdatedAt, ts.clock.Now())
	}
	storedJob, err := ts.db.Jobs.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedJob.SourceEntryID == nil || *storedJob.SourceEntryID != root.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", storedJob.SourceEntryID, root.ID)
	}
}

func TestEditPost_RejectsWrongEditorAndNonUserPost(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "owner text", nil)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.EditPost(t.Context(), root.ID, assistant.ID, "bad edit", nil); !errors.Is(err, ErrNotEditable) {
		t.Errorf("EditPost(wrong editor) error = %v, want ErrNotEditable", err)
	}
	requireEntryBody(t, ts.db, root.ID, "owner text")

	reply, err := ts.CreateReply(t.Context(), root.ID, domain.EntryLLMReply, "generated", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.EditPost(t.Context(), reply.ID, ts.owner.ID, "bad edit", nil); !errors.Is(err, ErrNotEditable) {
		t.Errorf("EditPost(non-user post) error = %v, want ErrNotEditable", err)
	}
	requireEntryBody(t, ts.db, reply.ID, "generated")
}

func TestEditPost_NotFound(t *testing.T) {
	ts := newTestService(t)
	_, err := ts.EditPost(t.Context(), "missing", ts.owner.ID, "edit", nil)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("EditPost() error = %v, want ErrNotFound", err)
	}
}

func TestEditPost_RollsBackBodyWhenJobConflicts(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "before", nil)
	if err != nil {
		t.Fatal(err)
	}
	key := "duplicate-edit"
	seed := newTestJob(ts.clock.Now(), &key)
	if err := ts.db.Jobs.Enqueue(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Hour)
	conflict := newTestJob(ts.clock.Now(), &key)

	if _, err := ts.EditPost(t.Context(), root.ID, ts.owner.ID, "after", &conflict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("EditPost() error = %v, want ErrConflict", err)
	}
	stored := requireEntryBody(t, ts.db, root.ID, "before")
	if !stored.UpdatedAt.Equal(root.UpdatedAt) {
		t.Errorf("failed EditPost changed UpdatedAt to %v, want %v", stored.UpdatedAt, root.UpdatedAt)
	}
}

func TestVisibility_DefaultTimelineExcludesArchivedAndHidden(t *testing.T) {
	ts := newTestService(t)
	visible, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "visible", nil)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "archived", nil)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "hidden", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.SetArchived(t.Context(), archived.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetHidden(t.Context(), hidden.ID, true); err != nil {
		t.Fatal(err)
	}

	defaultTimeline, err := ts.GetTimeline(t.Context(), domain.Page{Limit: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultTimeline) != 1 || defaultTimeline[0].ID != visible.ID {
		t.Errorf("default timeline = %v, want only visible entry", defaultTimeline)
	}
	all, err := ts.GetTimeline(t.Context(), domain.Page{Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("inclusive timeline returned %d entries, want 3", len(all))
	}
	for _, entry := range []domain.Entry{archived, hidden} {
		thread, err := ts.GetThread(t.Context(), entry.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(thread) != 1 || thread[0].ID != entry.ID {
			t.Errorf("GetThread(%s) = %v, want hidden/archived entry", entry.ID, thread)
		}
	}
}

func TestGetThreadAndChildren_DistinguishConversationFromDirectReplies(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	child1, err := ts.CreateReply(t.Context(), root.ID, domain.EntryUserPost, "child 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	grandchild, err := ts.CreateReply(t.Context(), child1.ID, domain.EntryLLMReply, "grandchild", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	child2, err := ts.CreateReply(t.Context(), root.ID, domain.EntryLLMFollowUp, "child 2", nil)
	if err != nil {
		t.Fatal(err)
	}

	conversation, err := ts.GetThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	wantConversation := []string{root.ID, child1.ID, grandchild.ID, child2.ID}
	if len(conversation) != len(wantConversation) {
		t.Fatalf("conversation length = %d, want %d", len(conversation), len(wantConversation))
	}
	for i, want := range wantConversation {
		if conversation[i].ID != want {
			t.Errorf("conversation[%d].ID = %s, want %s", i, conversation[i].ID, want)
		}
	}

	children, err := ts.GetChildren(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || children[0].ID != child1.ID || children[1].ID != child2.ID {
		t.Errorf("root children = %v, want child1 and child2 only", children)
	}
	grandchildren, err := ts.GetChildren(t.Context(), child1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grandchildren) != 1 || grandchildren[0].ID != grandchild.ID {
		t.Errorf("child1 children = %v, want grandchild only", grandchildren)
	}
}

func TestGetTimeline_StableCursorUsesCreatedAtAndID(t *testing.T) {
	ts := newTestService(t)
	want := make(map[string]bool)
	for range 3 {
		entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "same time", nil)
		if err != nil {
			t.Fatal(err)
		}
		want[entry.ID] = true
	}

	seen := make(map[string]bool)
	var cursor *domain.Cursor
	for range len(want) {
		page, err := ts.GetTimeline(t.Context(), domain.Page{After: cursor, Limit: 1}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 {
			t.Fatalf("page length = %d, want 1", len(page))
		}
		if seen[page[0].ID] {
			t.Fatalf("entry %s returned twice", page[0].ID)
		}
		seen[page[0].ID] = true
		cursor = &domain.Cursor{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("entry %s was skipped by pagination", id)
		}
	}
}

func TestGetTimelineDesc_NewestFirstThenOlder(t *testing.T) {
	ts := newTestService(t)
	var ids []string
	for i := 0; i < 3; i++ {
		entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "post", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, entry.ID) // ids[0] oldest, ids[2] newest
		ts.clock.Advance(time.Minute)
	}

	first, err := ts.GetTimelineDesc(t.Context(), nil, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != ids[2] || first[1].ID != ids[1] {
		t.Fatalf("first page = %v, want newest-first [%s, %s]", first, ids[2], ids[1])
	}

	cursor := &domain.Cursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID}
	second, err := ts.GetTimelineDesc(t.Context(), cursor, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != ids[0] {
		t.Fatalf("second page = %v, want [%s]", second, ids[0])
	}
}

func TestGetEntry_ReturnsArchivedAndHiddenWithoutFiltering(t *testing.T) {
	ts := newTestService(t)
	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "post", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.SetHidden(t.Context(), entry.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := ts.GetEntry(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != entry.ID || got.HiddenAt == nil {
		t.Errorf("GetEntry = %+v, want the hidden entry returned as-is", got)
	}

	if _, err := ts.GetEntry(t.Context(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetEntry(missing) error = %v, want ErrNotFound", err)
	}
}

func TestCountByAuthor_CountsAcrossThreadsIncludingHidden(t *testing.T) {
	ts := newTestService(t)
	first, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "two", nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetHidden(t.Context(), first.ID, true); err != nil {
		t.Fatal(err)
	}

	n, err := ts.CountByAuthor(t.Context(), ts.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("CountByAuthor = %d, want 2", n)
	}
}

func TestResolveAuthor_ReturnsActorType(t *testing.T) {
	ts := newTestService(t)

	actor, err := ts.ResolveAuthor(t.Context(), ts.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actor.ID != ts.owner.ID || actor.Type != domain.ActorOwner {
		t.Errorf("ResolveAuthor(owner) = %+v, want type %q", actor, domain.ActorOwner)
	}

	if _, err := ts.ResolveAuthor(t.Context(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ResolveAuthor(missing) error = %v, want ErrNotFound", err)
	}
}
