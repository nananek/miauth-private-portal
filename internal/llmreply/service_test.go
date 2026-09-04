package llmreply

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

type fakeProvider struct {
	calls int
	fn    func(ctx context.Context, req CompletionRequest) (CompletionResult, error)
}

func (f *fakeProvider) Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	f.calls++
	return f.fn(ctx, req)
}

func newTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second, MaxOpenConns: 4,
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now().UTC()}
	if err := db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatalf("create owner actor: %v", err)
	}
	return db
}

// newEnqueuedTestJob persists a job row for sourceEntryID (Handle itself
// never touches repos.Jobs, but LLMGenerationRepository.Create's job_id
// column is a foreign key into jobs, so a test job must actually exist
// there before Handle can record a generation against it).
func newEnqueuedTestJob(t *testing.T, db *sqlite.DB, sourceEntryID, payload string, attempt int) domain.Job {
	t.Helper()
	now := time.Now().UTC()
	job := domain.Job{
		ID:             domain.NewID(),
		JobType:        JobType,
		Payload:        payload,
		PayloadVersion: 1,
		State:          domain.JobPending,
		Attempt:        attempt,
		SourceEntryID:  &sourceEntryID,
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.Jobs.Enqueue(t.Context(), job); err != nil {
		t.Fatalf("enqueue test job: %v", err)
	}
	return job
}

func mustPayload(t *testing.T, kind domain.GenerationKind) string {
	t.Helper()
	p, err := NewJobPayload(ReplyDecision{ShouldGenerate: true, Kind: kind, PolicyVersion: "test-v1"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestSetup(t *testing.T, provider Provider, maxAttempts int) (*Service, *sqlite.DB, *timeline.Service) {
	t.Helper()
	db := newTestDB(t)
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})
	svc := NewService(db.Repos, timelineSvc, provider, Config{
		ProviderName:    "openai",
		Model:           "test-model",
		MaxOutputTokens: 256,
		ThreadContext:   ContextBudget{MaxMessages: 20, MaxChars: 8000},
		MaxAttempts:     maxAttempts,
	}, nil)
	return svc, db, timelineSvc
}

func TestHandle_Success_Reply(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		pt, ct := 10, 5
		return CompletionResult{Content: "here is a reply", PromptTokens: &pt, CompletionTokens: &ct}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)

	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)

	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1", provider.calls)
	}

	children, err := timelineSvc.GetChildren(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("len(children) = %d, want 1", len(children))
	}
	reply := children[0]
	if reply.Kind != domain.EntryLLMReply {
		t.Errorf("reply.Kind = %q, want %q", reply.Kind, domain.EntryLLMReply)
	}
	if reply.Body != "here is a reply" {
		t.Errorf("reply.Body = %q, want %q", reply.Body, "here is a reply")
	}

	gen, err := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gen.Status != domain.GenerationComplete {
		t.Errorf("generation Status = %q, want complete", gen.Status)
	}
	if gen.ResultEntryID == nil || *gen.ResultEntryID != reply.ID {
		t.Errorf("generation ResultEntryID = %v, want %q", gen.ResultEntryID, reply.ID)
	}
	if gen.PromptTokens == nil || *gen.PromptTokens != 10 || gen.CompletionTokens == nil || *gen.CompletionTokens != 5 {
		t.Errorf("generation tokens = (%v, %v), want (10, 5)", gen.PromptTokens, gen.CompletionTokens)
	}
	if gen.Provider != "openai" || gen.Model != "test-model" {
		t.Errorf("generation provenance = (%q, %q), want (openai, test-model)", gen.Provider, gen.Model)
	}
}

func TestHandle_Success_FollowUp(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: "what made you decide that?"}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)

	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "still not sure about this design", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationFollowUp), 0)

	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	children, err := timelineSvc.GetChildren(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Kind != domain.EntryLLMFollowUp {
		t.Fatalf("children = %+v, want one EntryLLMFollowUp", children)
	}
}

func TestHandle_HighRiskTargetAddsDisclaimerToPrompt(t *testing.T) {
	var seenSystemPrompt string
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		seenSystemPrompt = req.Messages[0].Content
		return CompletionResult{Content: "qualified reply"}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)

	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "should I get legal advice about this contract?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)
	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if seenSystemPrompt == "" {
		t.Fatal("provider never received a system prompt")
	}
	if !strings.Contains(seenSystemPrompt, highRiskDisclaimerInstruction) {
		t.Errorf("system prompt = %q, want the high-risk disclaimer", seenSystemPrompt)
	}
}

func TestHandle_PermanentProviderErrorFailsGenerationAndReturnsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryAuth, errors.New("invalid api key"))
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)

	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)

	err = svc.Handle(t.Context(), job)
	if err == nil {
		t.Fatal("Handle() error = nil, want an error")
	}
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v (%T), want *jobs.PermanentError", err, err)
	}

	gen, getErr := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if gen.Status != domain.GenerationFailed {
		t.Errorf("generation Status = %q, want failed", gen.Status)
	}
	if gen.ErrorCategory == nil || *gen.ErrorCategory != string(CategoryAuth) {
		t.Errorf("generation ErrorCategory = %v, want %q", gen.ErrorCategory, CategoryAuth)
	}

	children, listErr := timelineSvc.GetChildren(t.Context(), root.ID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(children) != 0 {
		t.Errorf("children = %v, want none created after a permanent failure", children)
	}
}

func TestHandle_ContentRefusalIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryContentRefusal, errors.New("refused"))
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)

	err = svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
	gen, _ := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if gen.Status != domain.GenerationFailed || gen.ErrorCategory == nil || *gen.ErrorCategory != string(CategoryContentRefusal) {
		t.Errorf("generation = %+v, want failed/content_refusal", gen)
	}
}

func TestHandle_EmptyGeneratedContentIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: "   "}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)

	err = svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
	gen, _ := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if gen.Status != domain.GenerationFailed {
		t.Errorf("generation Status = %q, want failed", gen.Status)
	}
}

func TestHandle_RetryableFailureNotLastAttemptLeavesGenerationPending(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryServerError, errors.New("boom"))
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 3)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0) // attempt+1=1 < MaxAttempts=3

	err = svc.Handle(t.Context(), job)
	if err == nil {
		t.Fatal("Handle() error = nil, want an error")
	}
	var perm *jobs.PermanentError
	if errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want a plain retryable error, not PermanentError", err)
	}
	gen, getErr := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if gen.Status != domain.GenerationPending {
		t.Errorf("generation Status = %q, want pending (not yet last attempt)", gen.Status)
	}
}

func TestHandle_RetryableFailureOnLastAttemptFailsGeneration(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryRateLimit, errors.New("rate limited"))
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 3)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 2) // attempt+1=3 >= MaxAttempts=3

	err = svc.Handle(t.Context(), job)
	if err == nil {
		t.Fatal("Handle() error = nil, want an error")
	}
	var perm *jobs.PermanentError
	if errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want a plain error (jobs.Manager itself decides dead), not PermanentError", err)
	}
	gen, getErr := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if gen.Status != domain.GenerationFailed {
		t.Errorf("generation Status = %q, want failed on the last attempt", gen.Status)
	}
	if gen.ErrorCategory == nil || *gen.ErrorCategory != string(CategoryRateLimit) {
		t.Errorf("generation ErrorCategory = %v, want %q", gen.ErrorCategory, CategoryRateLimit)
	}
}

// TestHandle_CancelledContextOnLastAttemptDoesNotFailGeneration documents
// the guard in handleProviderFailure: internal/jobs.Manager retries a
// cancelled handler unconditionally through its own shutdown path,
// independent of MaxAttempts, so pre-emptively failing the generation
// merely because this happened to be attempt MaxAttempts-1 would be
// wrong — the job itself will still be retried regardless.
func TestHandle_CancelledContextOnLastAttemptDoesNotFailGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		cancel() // simulate lease loss/shutdown racing the provider call's return
		return CompletionResult{}, NewProviderError(CategoryTimeout, errors.New("boom"))
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 3)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 2) // last attempt

	err = svc.Handle(ctx, job)
	if err == nil {
		t.Fatal("Handle() error = nil, want an error")
	}
	gen, getErr := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if gen.Status != domain.GenerationPending {
		t.Errorf("generation Status = %q, want pending (cancellation must not pre-emptively fail it)", gen.Status)
	}
}

func TestHandle_DuplicateDeliveryAfterCompleteSkipsRegeneration(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: "first reply"}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)

	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	provider.fn = func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		t.Fatal("provider must not be called again for a duplicate delivery of the same completed job")
		return CompletionResult{}, nil
	}
	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("duplicate Handle: %v", err)
	}
	children, err := timelineSvc.GetChildren(t.Context(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Errorf("children = %v, want exactly one reply despite duplicate delivery", children)
	}
}

func TestHandle_DuplicateDeliveryAfterPermanentFailureSkipsRetry(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryAuth, errors.New("invalid api key"))
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)

	if err := svc.Handle(t.Context(), job); err == nil {
		t.Fatal("first Handle() error = nil, want a permanent error")
	}
	callsBefore := provider.calls
	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("duplicate Handle after permanent failure: %v", err)
	}
	if provider.calls != callsBefore {
		t.Errorf("provider.calls = %d, want unchanged %d (no retry after permanent failure)", provider.calls, callsBefore)
	}
}

// TestHandle_DuplicateDeliveryWhileStillPendingRetriesNormally covers the
// "worker crashed after Create but before completing" case: a pending
// generation row already exists for this exact job ID, so Handle must
// treat it as its own in-progress attempt and proceed, not skip it.
func TestHandle_DuplicateDeliveryWhileStillPendingRetriesNormally(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: "reply after crash recovery"}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "what do you think?", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)

	if err := db.Generations.Create(t.Context(), domain.LLMGeneration{
		ID:            "llmgen:" + job.ID,
		TargetEntryID: root.ID,
		Kind:          domain.GenerationReply,
		Provider:      "openai",
		Model:         "test-model",
		PromptVersion: "test-v1",
		Status:        domain.GenerationPending,
		JobID:         &job.ID,
		RequestedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1", provider.calls)
	}
	gen, err := db.Generations.Get(t.Context(), "llmgen:"+job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gen.Status != domain.GenerationComplete {
		t.Errorf("generation Status = %q, want complete", gen.Status)
	}
}

func TestHandle_MalformedPayloadIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		t.Fatal("provider must not be called for a malformed payload")
		return CompletionResult{}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, "not-json", 0)

	err = svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
}

func TestHandle_UnknownKindIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		t.Fatal("provider must not be called for an unknown kind")
		return CompletionResult{}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, `{"kind":"bogus","promptVersion":"v1"}`, 0)

	err = svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
}

func TestHandle_MissingSourceEntryIDIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		t.Fatal("provider must not be called when source entry id is missing")
		return CompletionResult{}, nil
	}}
	svc, db, timelineSvc := newTestSetup(t, provider, 5)
	root, err := timelineSvc.CreateRoot(t.Context(), domain.EntryUserPost, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := newEnqueuedTestJob(t, db, root.ID, mustPayload(t, domain.GenerationReply), 0)
	job.SourceEntryID = nil

	err = svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
}
