package timeline

import (
	"context"
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

// TestCreateRoot_EnqueuesMultipleJobsForEntryAtomically backs Issue #10's
// need to enqueue both a reply-generation job and a classification job
// for the same post in one transaction.
func TestCreateRoot_EnqueuesMultipleJobsForEntryAtomically(t *testing.T) {
	ts := newTestService(t)
	first := newTestJob(ts.clock.Now(), nil)
	second := newTestJob(ts.clock.Now(), nil)

	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", &first, &second)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []domain.Job{first, second} {
		stored, err := ts.db.Jobs.Get(t.Context(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.SourceEntryID == nil || *stored.SourceEntryID != entry.ID {
			t.Errorf("job %s SourceEntryID = %v, want %q", job.ID, stored.SourceEntryID, entry.ID)
		}
	}
}

// TestCreateRoot_SkipsNilJobsMixedWithNonNilOnes backs enqueueForEntry's
// per-element nil check: since jobs became variadic, a caller passing one
// nil job produces a one-element slice containing nil (not a nil slice),
// and that must still be handled the same as before — skipped, not
// enqueued — even when it appears alongside a real job.
func TestCreateRoot_SkipsNilJobsMixedWithNonNilOnes(t *testing.T) {
	ts := newTestService(t)
	real := newTestJob(ts.clock.Now(), nil)

	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil, &real, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != real.ID {
		t.Fatalf("jobs = %v, want exactly [%s]", jobs, real.ID)
	}
	if jobs[0].SourceEntryID == nil || *jobs[0].SourceEntryID != entry.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", jobs[0].SourceEntryID, entry.ID)
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

// TestCreateGeneratedReply_BodyNeverGetsWireMarker pins Issue #13 AC5's
// design constraint: the "[reply]"/"[follow-up question]" markers Aria's
// timeline needs to tell generated replies and follow-up questions apart
// (internal/httpserver's wireText) are a wire-projection concern only.
// Neither the domain Entry.Body nor the LLMGeneration.Body audit record
// may ever carry them, or the generation log would stop reflecting what
// the provider actually produced.
func TestCreateGeneratedReply_BodyNeverGetsWireMarker(t *testing.T) {
	for _, kind := range []domain.EntryKind{domain.EntryLLMReply, domain.EntryLLMFollowUp} {
		t.Run(string(kind), func(t *testing.T) {
			ts := newTestService(t)
			root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
			if err != nil {
				t.Fatal(err)
			}
			gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
			if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
				t.Fatal(err)
			}

			const rawBody = "plain generated text, no marker"
			reply, err := ts.CreateGeneratedReply(t.Context(), root.ID, kind, rawBody, gen.ID, nil, nil)
			if err != nil {
				t.Fatalf("CreateGeneratedReply: %v", err)
			}
			if reply.Body != rawBody {
				t.Errorf("reply.Body = %q, want unmarked %q", reply.Body, rawBody)
			}

			stored, err := ts.db.Generations.Get(t.Context(), gen.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Body == nil || *stored.Body != rawBody {
				t.Errorf("generation Body = %v, want unmarked %q", stored.Body, rawBody)
			}
		})
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

func TestCreateGeneratedReplyBy_AssistantActorSucceeds(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}

	ts.clock.Advance(time.Minute)
	reply, err := ts.CreateGeneratedReplyBy(t.Context(), assistant.ID, root.ID, "generated reply", nil)
	if err != nil {
		t.Fatalf("CreateGeneratedReplyBy: %v", err)
	}
	if reply.Kind != domain.EntryLLMReply {
		t.Errorf("reply.Kind = %q, want llm_reply", reply.Kind)
	}
	if reply.ThreadID != root.ThreadID {
		t.Errorf("reply.ThreadID = %q, want parent's %q", reply.ThreadID, root.ThreadID)
	}
	if reply.ParentEntryID == nil || *reply.ParentEntryID != root.ID {
		t.Errorf("reply.ParentEntryID = %v, want %q", reply.ParentEntryID, root.ID)
	}
	if reply.AuthorActorID != assistant.ID {
		t.Errorf("reply.AuthorActorID = %q, want assistant %q", reply.AuthorActorID, assistant.ID)
	}
	if reply.Body != "generated reply" {
		t.Errorf("reply.Body = %q, want unmarked %q", reply.Body, "generated reply")
	}

	thread, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !thread.UpdatedAt.Equal(ts.clock.Now()) {
		t.Errorf("thread UpdatedAt = %v, want touched at %v", thread.UpdatedAt, ts.clock.Now())
	}
}

// TestCreateGeneratedReplyBy_VirtualActorSucceeds is Issue #52's core
// case: an active Open WebUI model actor whose workspace is enabled may
// author a generated reply exactly as the assistant actor can.
func TestCreateGeneratedReplyBy_VirtualActorSucceeds(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	virtual := mustCreateVirtualActor(t, ts.db, ts.clock.Now())

	reply, err := ts.CreateGeneratedReplyBy(t.Context(), virtual.ID, root.ID, "generated by the model", nil)
	if err != nil {
		t.Fatalf("CreateGeneratedReplyBy: %v", err)
	}
	if reply.AuthorActorID != virtual.ID {
		t.Errorf("reply.AuthorActorID = %q, want VirtualActor %q", reply.AuthorActorID, virtual.ID)
	}
	if reply.Kind != domain.EntryLLMReply {
		t.Errorf("reply.Kind = %q, want llm_reply", reply.Kind)
	}
}

// TestCreateGeneratedReplyBy_InvokesCompleteInsideTheSameTransaction
// checks the whole point of the complete hook: a write it makes through
// the repos it is handed lands together with the entry, and (via the
// rollback case below) rolls back together with it too.
func TestCreateGeneratedReplyBy_InvokesCompleteInsideTheSameTransaction(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatal(err)
	}

	var completedEntryID string
	reply, err := ts.CreateGeneratedReplyBy(t.Context(), assistant.ID, root.ID, "body",
		func(ctx context.Context, repos domain.Repos, entry domain.Entry) error {
			completedEntryID = entry.ID
			return repos.Generations.Complete(ctx, gen.ID, entry.ID, entry.Body, nil, nil, ts.clock.Now())
		},
	)
	if err != nil {
		t.Fatalf("CreateGeneratedReplyBy: %v", err)
	}
	if completedEntryID != reply.ID {
		t.Errorf("complete saw entry %q, want the created reply %q", completedEntryID, reply.ID)
	}
	stored, err := ts.db.Generations.Get(t.Context(), gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.GenerationComplete || stored.ResultEntryID == nil || *stored.ResultEntryID != reply.ID {
		t.Errorf("generation = %+v, want completed with ResultEntryID %q", stored, reply.ID)
	}
}

// TestCreateGeneratedReplyBy_RollsBackEntryWhenCompleteFails mirrors
// CreateGeneratedReply_RollsBackEntryWhenGenerationNotPending: whatever
// complete does is exactly as atomic with the entry as
// Generations.Complete is in the older method, since here it is the
// same transaction rather than a second call.
func TestCreateGeneratedReplyBy_RollsBackEntryWhenCompleteFails(t *testing.T) {
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("deliberate complete failure")

	ts.clock.Advance(time.Minute)
	_, err = ts.CreateGeneratedReplyBy(t.Context(), assistant.ID, root.ID, "body",
		func(ctx context.Context, repos domain.Repos, entry domain.Entry) error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("CreateGeneratedReplyBy() error = %v, want the deliberate complete failure", err)
	}

	thread, err := ts.GetThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 || thread[0].ID != root.ID {
		t.Errorf("a failed complete left thread entries = %v, want only root", thread)
	}
	after, err := ts.db.Threads.Get(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("a failed complete touched thread at %v, want unchanged %v", after.UpdatedAt, before.UpdatedAt)
	}
}

// TestCreateGeneratedReplyBy_RejectsIneligibleAuthors is the eligibility
// gate's negative space: the owner and system actor are never eligible
// (they are not in {assistant, openwebui_model} at all), and an Open
// WebUI model actor becomes ineligible the moment its model is
// deactivated or its workspace is disabled — the same two gates
// internal/openwebui.Registry.ResolveVirtualActor checks before
// projecting a VirtualActor, so an entry can never be authored by an
// identity the wire layer could not also present.
func TestCreateGeneratedReplyBy_RejectsIneligibleAuthors(t *testing.T) {
	t.Run("owner", func(t *testing.T) {
		ts := newTestService(t)
		root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.CreateGeneratedReplyBy(t.Context(), ts.owner.ID, root.ID, "body", nil); !errors.Is(err, ErrAuthorNotEligible) {
			t.Errorf("CreateGeneratedReplyBy(owner) error = %v, want ErrAuthorNotEligible", err)
		}
	})
	t.Run("system", func(t *testing.T) {
		ts := newTestService(t)
		root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
		if err != nil {
			t.Fatal(err)
		}
		system, err := ts.db.Actors.GetByType(t.Context(), domain.ActorSystem)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.CreateGeneratedReplyBy(t.Context(), system.ID, root.ID, "body", nil); !errors.Is(err, ErrAuthorNotEligible) {
			t.Errorf("CreateGeneratedReplyBy(system) error = %v, want ErrAuthorNotEligible", err)
		}
	})
	t.Run("unknown actor id", func(t *testing.T) {
		ts := newTestService(t)
		root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.CreateGeneratedReplyBy(t.Context(), "does-not-exist", root.ID, "body", nil); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("CreateGeneratedReplyBy(unknown actor) error = %v, want ErrNotFound", err)
		}
	})
	t.Run("inactive model", func(t *testing.T) {
		ts := newTestService(t)
		root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
		if err != nil {
			t.Fatal(err)
		}
		virtual := mustCreateVirtualActor(t, ts.db, ts.clock.Now())
		model, err := ts.db.OpenWebUIModels.GetByActor(t.Context(), virtual.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.db.OpenWebUIModels.SetActive(t.Context(), model.ID, false, ts.clock.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.CreateGeneratedReplyBy(t.Context(), virtual.ID, root.ID, "body", nil); !errors.Is(err, ErrAuthorNotEligible) {
			t.Errorf("CreateGeneratedReplyBy(inactive model) error = %v, want ErrAuthorNotEligible", err)
		}
	})
	t.Run("disabled workspace", func(t *testing.T) {
		ts := newTestService(t)
		root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
		if err != nil {
			t.Fatal(err)
		}
		virtual := mustCreateVirtualActor(t, ts.db, ts.clock.Now())
		model, err := ts.db.OpenWebUIModels.GetByActor(t.Context(), virtual.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.db.OpenWebUIWorkspaces.SetEnabled(t.Context(), model.WorkspaceID, false, ts.clock.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.CreateGeneratedReplyBy(t.Context(), virtual.ID, root.ID, "body", nil); !errors.Is(err, ErrAuthorNotEligible) {
			t.Errorf("CreateGeneratedReplyBy(disabled workspace) error = %v, want ErrAuthorNotEligible", err)
		}
	})

	// None of the rejected calls above should have created an entry.
	// (Re-verified once more here against a fresh service so a bug that
	// only manifests after several ineligible attempts in one database
	// would still surface.)
	ts := newTestService(t)
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateGeneratedReplyBy(t.Context(), ts.owner.ID, root.ID, "body", nil); !errors.Is(err, ErrAuthorNotEligible) {
		t.Fatal(err)
	}
	thread, err := ts.GetThread(t.Context(), root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 {
		t.Errorf("a rejected CreateGeneratedReplyBy left thread entries = %v, want only root", thread)
	}
}

func TestCreateGeneratedReplyBy_RejectsMissingParent(t *testing.T) {
	ts := newTestService(t)
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateGeneratedReplyBy(t.Context(), assistant.ID, "missing", "body", nil); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("CreateGeneratedReplyBy() error = %v, want ErrParentNotFound", err)
	}
}

// TestCreateGeneratedReplyBy_NeverRecordsSelfMentionOrNotification pins
// the plan's deliberate omission: a generated reply is never the
// owner's own post (no self-mention row), and whether it notifies is
// left to Issue #53 (no "reply" notification here either).
func TestCreateGeneratedReplyBy_NeverRecordsSelfMentionOrNotification(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")
	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root @owner", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The owner's own root post *does* self-mention, establishing that
	// detection is active in this service instance.
	mentions, err := ts.db.Mentions.ListEntriesByMentionedActor(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mentions) != 1 {
		t.Fatalf("owner root post should have self-mentioned once, got %d", len(mentions))
	}

	virtual := mustCreateVirtualActor(t, ts.db, ts.clock.Now())
	reply, err := ts.CreateGeneratedReplyBy(t.Context(), virtual.ID, root.ID, "reply mentioning @owner", nil)
	if err != nil {
		t.Fatal(err)
	}

	afterReply, err := ts.db.Mentions.ListEntriesByMentionedActor(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterReply) != 1 {
		t.Errorf("CreateGeneratedReplyBy should never record a self-mention, got %d mention(s)", len(afterReply))
	}

	notifications, err := ts.db.Notifications.ListDesc(t.Context(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notifications {
		if n.RelatedEntryID == reply.ID {
			t.Errorf("CreateGeneratedReplyBy should not record a notification, found %+v", n)
		}
	}
}

func mustCreateExternalSourceForTest(t *testing.T, ts *testService, kind, uri string) domain.ExternalSource {
	t.Helper()
	s := domain.ExternalSource{ID: domain.NewID(), Kind: kind, URI: uri, CreatedAt: ts.clock.Now()}
	if err := ts.db.ExternalSources.Create(t.Context(), s); err != nil {
		t.Fatalf("create external source: %v", err)
	}
	return s
}

func TestCreateExternalEntry_CreatesRootEntryAndPromotesItem(t *testing.T) {
	ts := newTestService(t)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")

	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", DedupeKey: "dedupe-1"}
	entry, created, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "hello from rss")
	if err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}
	if !created {
		t.Error("created = false, want true for a brand new item")
	}
	if !entry.IsRoot() || entry.ID != entry.ThreadID {
		t.Errorf("root topology is invalid: %+v", entry)
	}
	if entry.Body != "hello from rss" {
		t.Errorf("Body = %q, want %q", entry.Body, "hello from rss")
	}
	systemActor, err := ts.db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	if entry.AuthorActorID != systemActor.ID {
		t.Errorf("AuthorActorID = %q, want system actor %q", entry.AuthorActorID, systemActor.ID)
	}

	stored, err := ts.db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.EntryID == nil || *stored.EntryID != entry.ID {
		t.Errorf("stored item EntryID = %v, want %q", stored.EntryID, entry.ID)
	}
}

// TestCreateExternalEntry_UsesSourceActorAndProvenanceURL is Issue #77
// PR4/ADR-0008's regression test: a source with its own
// ActorExternalSource (design A's per-feed host display) must author
// its entries as that actor, not the shared system actor, and the
// item's ProvenanceURL must be denormalized onto the created Entry.
func TestCreateExternalEntry_UsesSourceActorAndProvenanceURL(t *testing.T) {
	ts := newTestService(t)
	sourceActor := domain.Actor{ID: domain.NewID(), Type: domain.ActorExternalSource, CreatedAt: ts.clock.Now()}
	if err := ts.db.Actors.Create(t.Context(), sourceActor); err != nil {
		t.Fatalf("create source actor: %v", err)
	}
	host := "example.com"
	username := "example_com"
	source := domain.ExternalSource{
		ID: domain.NewID(), Kind: "rss", URI: "https://example.com/feed.xml",
		ActorID: &sourceActor.ID, Username: &username, Host: &host, CreatedAt: ts.clock.Now(),
	}
	if err := ts.db.ExternalSources.Create(t.Context(), source); err != nil {
		t.Fatalf("create external source: %v", err)
	}

	provenanceURL := "https://example.com/articles/1"
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", ProvenanceURL: &provenanceURL, DedupeKey: "dedupe-1"}
	entry, _, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "hello from rss")
	if err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}
	if entry.AuthorActorID != sourceActor.ID {
		t.Errorf("AuthorActorID = %q, want the source's own actor %q", entry.AuthorActorID, sourceActor.ID)
	}
	if entry.ProvenanceURL == nil || *entry.ProvenanceURL != provenanceURL {
		t.Errorf("ProvenanceURL = %v, want %q", entry.ProvenanceURL, provenanceURL)
	}

	// The persisted row must round-trip both fields identically.
	stored, err := ts.db.Entries.Get(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Entries.Get: %v", err)
	}
	if stored.AuthorActorID != sourceActor.ID {
		t.Errorf("stored AuthorActorID = %q, want %q", stored.AuthorActorID, sourceActor.ID)
	}
	if stored.ProvenanceURL == nil || *stored.ProvenanceURL != provenanceURL {
		t.Errorf("stored ProvenanceURL = %v, want %q", stored.ProvenanceURL, provenanceURL)
	}
}

// TestCreateExternalEntry_UsesPublishedAtAsCreatedAt is Issue #83's core
// regression test: the home timeline sorts by Entries.CreatedAt, so an
// ingested item's CreatedAt must reflect when the source published it,
// not when the poll happened to run — otherwise a same-batch ingest of
// several items (see the ingest package's own regression test) reverses
// their timeline order relative to the feed's actual publish order.
func TestCreateExternalEntry_UsesPublishedAtAsCreatedAt(t *testing.T) {
	ts := newTestService(t)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")

	ts.clock.Advance(time.Hour)
	published := ts.clock.Now().Add(-30 * time.Minute)
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", DedupeKey: "dedupe-1", PublishedAt: &published}

	entry, created, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "body")
	if err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if !entry.CreatedAt.Equal(published) {
		t.Errorf("entry.CreatedAt = %v, want PublishedAt %v", entry.CreatedAt, published)
	}
	if !entry.UpdatedAt.Equal(ts.clock.Now()) {
		t.Errorf("entry.UpdatedAt = %v, want ingest time %v", entry.UpdatedAt, ts.clock.Now())
	}

	stored, err := ts.db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.FetchedAt.Equal(ts.clock.Now()) {
		t.Errorf("stored item FetchedAt = %v, want ingest time %v (must not follow PublishedAt)", stored.FetchedAt, ts.clock.Now())
	}
}

// TestCreateExternalEntry_ClampsFuturePublishedAtToNow guards against a
// misbehaving or misconfigured feed publishing with a future timestamp:
// without the clamp, such an item would jump to the top of the home
// timeline as if it just happened, even though it has not "happened" yet
// from the portal's point of view.
func TestCreateExternalEntry_ClampsFuturePublishedAtToNow(t *testing.T) {
	ts := newTestService(t)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")

	future := ts.clock.Now().Add(24 * time.Hour)
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", DedupeKey: "dedupe-1", PublishedAt: &future}

	entry, _, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "body")
	if err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}
	if !entry.CreatedAt.Equal(ts.clock.Now()) {
		t.Errorf("entry.CreatedAt = %v, want clamped to ingest time %v", entry.CreatedAt, ts.clock.Now())
	}
}

// TestCreateExternalEntry_NilPublishedAtFallsBackToNow keeps the
// pre-Issue-#83 behavior for sources where PublishedAt could not be
// determined (unparseable or absent upstream date).
func TestCreateExternalEntry_NilPublishedAtFallsBackToNow(t *testing.T) {
	ts := newTestService(t)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")

	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", DedupeKey: "dedupe-1"}

	entry, _, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "body")
	if err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}
	if !entry.CreatedAt.Equal(ts.clock.Now()) {
		t.Errorf("entry.CreatedAt = %v, want ingest time %v when PublishedAt is nil", entry.CreatedAt, ts.clock.Now())
	}
}

// TestCreateExternalEntry_DuplicateDedupeKeyReturnsExistingEntryWithoutDuplicating
// is the dedupe safety net Issue #11 requires: re-delivering the same
// external item (a retried "external_source_poll" job, or the same item
// reappearing in a later fetch) must never create a second timeline
// entry for it.
func TestCreateExternalEntry_DuplicateDedupeKeyReturnsExistingEntryWithoutDuplicating(t *testing.T) {
	ts := newTestService(t)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", DedupeKey: "dedupe-1"}

	first, firstCreated, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "first delivery")
	if err != nil {
		t.Fatalf("first CreateExternalEntry: %v", err)
	}
	if !firstCreated {
		t.Fatal("first delivery: created = false, want true")
	}

	second, secondCreated, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "first delivery")
	if err != nil {
		t.Fatalf("second CreateExternalEntry: %v", err)
	}
	if secondCreated {
		t.Error("second delivery: created = true, want false")
	}
	if second.ID != first.ID {
		t.Errorf("second delivery returned entry %q, want the original %q", second.ID, first.ID)
	}

	timeline, err := ts.GetTimeline(t.Context(), domain.Page{Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 1 {
		t.Errorf("timeline has %d entries after duplicate delivery, want 1", len(timeline))
	}
}

func TestCreateExternalEntry_RejectsReplyAndUnknownKinds(t *testing.T) {
	ts := newTestService(t)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")

	for _, kind := range []domain.EntryKind{domain.EntryLLMReply, domain.EntryLLMFollowUp, "unknown"} {
		item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-" + string(kind), DedupeKey: "dedupe-" + string(kind)}
		_, _, err := ts.CreateExternalEntry(t.Context(), kind, item, "body")
		if !errors.Is(err, ErrInvalidKind) {
			t.Errorf("kind %q: err = %v, want ErrInvalidKind", kind, err)
		}
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

// TestGetTimelineByAuthorsDesc_FiltersToGivenAuthors is Issue #115's
// notes/user-list-timeline's own core contract, exercised at the service
// layer: only entries authored by an ID in the given set are returned,
// newest-first, and a broader author set widens the result rather than
// requiring exact membership.
func TestGetTimelineByAuthorsDesc_FiltersToGivenAuthors(t *testing.T) {
	ts := newTestService(t)
	system, err := ts.db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}

	ownerEntry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "owner post", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	systemEntry, err := ts.CreateRoot(t.Context(), domain.EntrySystem, "system post", nil)
	if err != nil {
		t.Fatal(err)
	}

	ownerOnly, err := ts.GetTimelineByAuthorsDesc(t.Context(), []string{ts.owner.ID}, nil, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerOnly) != 1 || ownerOnly[0].ID != ownerEntry.ID {
		t.Fatalf("ownerOnly = %v, want only %s", ownerOnly, ownerEntry.ID)
	}

	both, err := ts.GetTimelineByAuthorsDesc(t.Context(), []string{ts.owner.ID, system.ID}, nil, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 || both[0].ID != systemEntry.ID || both[1].ID != ownerEntry.ID {
		t.Fatalf("both = %v, want newest-first [%s, %s]", both, systemEntry.ID, ownerEntry.ID)
	}
}

// TestGetTimelineByAuthorsDesc_EmptyAuthorsReturnsEmpty pins the "zero
// members means zero entries, never the whole timeline" contract a fresh
// user list must have (Issue #115): an empty authorActorIDs must not
// silently fall back to every author.
func TestGetTimelineByAuthorsDesc_EmptyAuthorsReturnsEmpty(t *testing.T) {
	ts := newTestService(t)
	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "post", nil); err != nil {
		t.Fatal(err)
	}

	got, err := ts.GetTimelineByAuthorsDesc(t.Context(), nil, nil, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty for an empty author set", got)
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

// TestGetTimeline_CursorStableWithVirtualActorAuthoredEntries is Issue
// #52's local-stable-cursor contract test: timeline pagination is keyed
// on (created_at, id) alone, and interleaving VirtualActor-authored
// llm_reply entries among the owner's own root posts must not change
// that — no page skips, duplicates, or reorders an entry because of who
// authored it.
func TestGetTimeline_CursorStableWithVirtualActorAuthoredEntries(t *testing.T) {
	ts := newTestService(t)
	virtual := mustCreateVirtualActor(t, ts.db, ts.clock.Now())

	var ids []string // oldest to newest
	root1, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root1", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, root1.ID)
	ts.clock.Advance(time.Minute)

	reply1, err := ts.CreateGeneratedReplyBy(t.Context(), virtual.ID, root1.ID, "reply1", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, reply1.ID)
	ts.clock.Advance(time.Minute)

	root2, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root2", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, root2.ID)
	ts.clock.Advance(time.Minute)

	reply2, err := ts.CreateGeneratedReplyBy(t.Context(), virtual.ID, root2.ID, "reply2", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, reply2.ID) // ids: [root1, reply1, root2, reply2], oldest to newest

	// Ascending pagination (GetTimeline), one entry per page: order must
	// be exactly oldest-to-newest regardless of author.
	var cursor *domain.Cursor
	for i, wantID := range ids {
		page, err := ts.GetTimeline(t.Context(), domain.Page{After: cursor, Limit: 1}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 || page[0].ID != wantID {
			t.Fatalf("page %d = %v, want [%s]", i, page, wantID)
		}
		cursor = &domain.Cursor{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
	}

	// Descending pagination (GetTimelineDesc), one entry per page: order
	// must be exactly newest-to-oldest, the reverse of ids.
	var descCursor *domain.Cursor
	for i := len(ids) - 1; i >= 0; i-- {
		page, err := ts.GetTimelineDesc(t.Context(), descCursor, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 || page[0].ID != ids[i] {
			t.Fatalf("desc page = %v, want [%s]", page, ids[i])
		}
		descCursor = &domain.Cursor{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
	}

	// A full-page fetch, split at the boundary between an owner entry
	// and a VirtualActor entry, must resume exactly where the cursor
	// left off with no gap or repeat.
	firstHalf, err := ts.GetTimelineDesc(t.Context(), nil, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstHalf) != 2 || firstHalf[0].ID != ids[3] || firstHalf[1].ID != ids[2] {
		t.Fatalf("first half = %v, want [%s, %s]", firstHalf, ids[3], ids[2])
	}
	resumeCursor := &domain.Cursor{CreatedAt: firstHalf[1].CreatedAt, ID: firstHalf[1].ID}
	secondHalf, err := ts.GetTimelineDesc(t.Context(), resumeCursor, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondHalf) != 2 || secondHalf[0].ID != ids[1] || secondHalf[1].ID != ids[0] {
		t.Fatalf("second half = %v, want [%s, %s]", secondHalf, ids[1], ids[0])
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

// TestCountByAuthor_CountsAcrossThreadsExcludingHiddenAndArchived pins
// Issue #23 PR3's (docs/decisions/0004-note-delete-as-hide.md) change to
// CountByAuthor: since /api/notes/delete maps onto SetHidden, notesCount
// must decrement for a deleted note, matching real Misskey's
// delete-decrements-notesCount wire behavior.
func TestCountByAuthor_CountsAcrossThreadsExcludingHiddenAndArchived(t *testing.T) {
	ts := newTestService(t)
	first, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "two", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "three", nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetHidden(t.Context(), first.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetArchived(t.Context(), second.ID, true); err != nil {
		t.Fatal(err)
	}

	n, err := ts.CountByAuthor(t.Context(), ts.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("CountByAuthor = %d, want 1 (hidden/archived entries excluded)", n)
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
