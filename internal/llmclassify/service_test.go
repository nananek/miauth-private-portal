package llmclassify

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
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

// mustCreatePost inserts a root user_post entry directly against the
// repositories, bypassing internal/timeline: this package's production
// code never depends on it (see service.go's doc comment), so tests
// build fixtures the same narrow way.
func mustCreatePost(t *testing.T, db *sqlite.DB, body string, at time.Time) domain.Entry {
	t.Helper()
	owner, err := db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatalf("get owner actor: %v", err)
	}
	id := domain.NewID()
	entry := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryUserPost, AuthorActorID: owner.ID, Body: body,
		ProcessingStatus: domain.ProcessingNone, CreatedAt: at, UpdatedAt: at,
	}
	if err := db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	return entry
}

// mustCreateReply inserts a user_post reply attached to parent, in the
// same thread.
func mustCreateReply(t *testing.T, db *sqlite.DB, parent domain.Entry, body string, at time.Time) domain.Entry {
	t.Helper()
	owner, err := db.Actors.GetByType(t.Context(), domain.ActorOwner)
	if err != nil {
		t.Fatalf("get owner actor: %v", err)
	}
	id := domain.NewID()
	entry := domain.Entry{
		ID: id, ThreadID: parent.ThreadID, ParentEntryID: &parent.ID, Kind: domain.EntryUserPost,
		AuthorActorID: owner.ID, Body: body, ProcessingStatus: domain.ProcessingNone, CreatedAt: at, UpdatedAt: at,
	}
	if err := db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create reply entry: %v", err)
	}
	return entry
}

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

func mustPayload(t *testing.T) string {
	t.Helper()
	p, err := NewJobPayload()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestSetup(t *testing.T, provider Provider, maxAttempts int) (*Service, *sqlite.DB) {
	t.Helper()
	db := newTestDB(t)
	svc := NewService(db, db.Repos, provider, Config{
		ProviderName:    "openai",
		Model:           "test-model",
		MaxOutputTokens: 256,
		ThreadContext:   ContextBudget{MaxMessages: 20, MaxChars: 8000},
		MaxAttempts:     maxAttempts,
	}, nil)
	return svc, db
}

func TestHandle_Success(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		pt, ct := 10, 5
		return CompletionResult{
			Content:          `{"subject":"go","summary":"a summary","tags":["go","learning"],"priority":"high","notebookCandidate":true}`,
			PromptTokens:     &pt,
			CompletionTokens: &ct,
		}, nil
	}}
	svc, db := newTestSetup(t, provider, 5)

	post := mustCreatePost(t, db, "learning about go generics today", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)

	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1", provider.calls)
	}

	got, err := db.Classifications.GetActive(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ClassificationComplete {
		t.Errorf("Status = %q, want complete", got.Status)
	}
	if got.Summary == nil || *got.Summary != "a summary" {
		t.Errorf("Summary = %v, want %q", got.Summary, "a summary")
	}
	if got.Priority == nil || *got.Priority != "high" {
		t.Errorf("Priority = %v, want high", got.Priority)
	}
	if !got.NotebookCandidate {
		t.Error("NotebookCandidate = false, want true")
	}
	if len(got.Tags) != 2 {
		t.Errorf("Tags = %v, want 2 entries", got.Tags)
	}
	if got.PromptTokens == nil || *got.PromptTokens != 10 {
		t.Errorf("PromptTokens = %v, want 10", got.PromptTokens)
	}
	if got.Provider != "openai" || got.Model != "test-model" {
		t.Errorf("provenance = (%q, %q), want (openai, test-model)", got.Provider, got.Model)
	}

	entry, err := db.Entries.Get(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ProcessingStatus != domain.ProcessingComplete {
		t.Errorf("ProcessingStatus = %q, want complete", entry.ProcessingStatus)
	}
}

func TestHandle_RelatedEntryIDs_ExcludesSelfAndHallucinationsKeepsValidCandidate(t *testing.T) {
	now := time.Now()

	provider := &fakeProvider{}
	svc, db := newTestSetup(t, provider, 5)

	post := mustCreatePost(t, db, "root post", now)
	sibling := mustCreateReply(t, db, post, "a related sibling reply", now.Add(time.Minute))

	provider.fn = func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: `{"relatedEntryIds":["` + sibling.ID + `","` + post.ID + `","nonexistent-hallucinated"]}`}, nil
	}
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)

	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, err := db.Classifications.GetActive(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RelatedEntryIDs) != 1 || got.RelatedEntryIDs[0] != sibling.ID {
		t.Errorf("RelatedEntryIDs = %v, want only [%s]", got.RelatedEntryIDs, sibling.ID)
	}
}

// TestHandle_EditLikeReclassificationCreatesNewVersionAndDeactivatesOld
// backs the "edit re-triggers classification" acceptance criterion: a
// second, distinct job for the same entry (simulating a job enqueued
// again after EditPost) produces a new version and activates it,
// deactivating the first, while both remain visible via ListVersions.
func TestHandle_EditLikeReclassificationCreatesNewVersionAndDeactivatesOld(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: `{"summary":"v1"}`}, nil
	}}
	svc, db := newTestSetup(t, provider, 5)

	post := mustCreatePost(t, db, "original body", time.Now())
	firstJob := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)
	if err := svc.Handle(t.Context(), firstJob); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	provider.fn = func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: `{"summary":"v2"}`}, nil
	}
	secondJob := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)
	if err := svc.Handle(t.Context(), secondJob); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	versions, err := db.Classifications.ListVersions(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(versions))
	}
	if versions[0].IsActive || !versions[1].IsActive {
		t.Errorf("versions active flags = (%v, %v), want (false, true)", versions[0].IsActive, versions[1].IsActive)
	}

	active, err := db.Classifications.GetActive(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Version != 2 || active.Summary == nil || *active.Summary != "v2" {
		t.Errorf("active = %+v, want version 2 with summary v2", active)
	}
}

func TestHandle_PermanentProviderErrorFailsClassificationAndReturnsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryAuth, errors.New("invalid api key"))
	}}
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)

	err := svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}

	versions, getErr := db.Classifications.ListVersions(t.Context(), post.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if len(versions) != 1 || versions[0].Status != domain.ClassificationFailed {
		t.Fatalf("versions = %+v, want one failed classification", versions)
	}
	if versions[0].ErrorCategory == nil || *versions[0].ErrorCategory != string(CategoryAuth) {
		t.Errorf("ErrorCategory = %v, want %q", versions[0].ErrorCategory, CategoryAuth)
	}

	entry, err := db.Entries.Get(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ProcessingStatus != domain.ProcessingFailed {
		t.Errorf("ProcessingStatus = %q, want failed", entry.ProcessingStatus)
	}
}

// TestHandle_MalformedResponseIsPermanent backs schema validation's own
// unrecoverable failure mode: a response that cannot be parsed as JSON at
// all fails the classification permanently, distinct from every
// normalize-in-place defect (which ParseAndNormalize repairs instead).
func TestHandle_MalformedResponseIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: "not-json"}, nil
	}}
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)

	err := svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
	versions, _ := db.Classifications.ListVersions(t.Context(), post.ID)
	if len(versions) != 1 || versions[0].Status != domain.ClassificationFailed {
		t.Fatalf("versions = %+v, want one failed classification", versions)
	}
	if versions[0].ErrorCategory == nil || *versions[0].ErrorCategory != string(CategoryMalformedResponse) {
		t.Errorf("ErrorCategory = %v, want %q", versions[0].ErrorCategory, CategoryMalformedResponse)
	}
}

func TestHandle_RetryableFailureNotLastAttemptLeavesClassificationPending(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryServerError, errors.New("boom"))
	}}
	svc, db := newTestSetup(t, provider, 3)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0) // attempt+1=1 < MaxAttempts=3

	err := svc.Handle(t.Context(), job)
	if err == nil {
		t.Fatal("Handle() error = nil, want an error")
	}
	var perm *jobs.PermanentError
	if errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want a plain retryable error, not PermanentError", err)
	}
	versions, _ := db.Classifications.ListVersions(t.Context(), post.ID)
	if len(versions) != 1 || versions[0].Status != domain.ClassificationPending {
		t.Fatalf("versions = %+v, want one pending classification (not yet last attempt)", versions)
	}
}

func TestHandle_RetryableFailureOnLastAttemptFailsClassification(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryRateLimit, errors.New("rate limited"))
	}}
	svc, db := newTestSetup(t, provider, 3)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 2) // attempt+1=3 >= MaxAttempts=3

	err := svc.Handle(t.Context(), job)
	if err == nil {
		t.Fatal("Handle() error = nil, want an error")
	}
	var perm *jobs.PermanentError
	if errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want a plain error (jobs.Manager itself decides dead), not PermanentError", err)
	}
	versions, _ := db.Classifications.ListVersions(t.Context(), post.ID)
	if len(versions) != 1 || versions[0].Status != domain.ClassificationFailed {
		t.Fatalf("versions = %+v, want one failed classification on the last attempt", versions)
	}
}

// TestHandle_CancelledContextOnLastAttemptDoesNotFailClassification
// mirrors internal/llmreply's identically-named test: a cancellation
// racing the provider call must not pre-emptively fail the
// classification even on what would otherwise be the last attempt,
// since internal/jobs.Manager retries a cancelled handler unconditionally.
func TestHandle_CancelledContextOnLastAttemptDoesNotFailClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		cancel()
		return CompletionResult{}, NewProviderError(CategoryTimeout, errors.New("boom"))
	}}
	svc, db := newTestSetup(t, provider, 3)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 2) // last attempt

	err := svc.Handle(ctx, job)
	if err == nil {
		t.Fatal("Handle() error = nil, want an error")
	}
	versions, getErr := db.Classifications.ListVersions(t.Context(), post.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if len(versions) != 1 || versions[0].Status != domain.ClassificationPending {
		t.Errorf("versions = %+v, want one pending classification (cancellation must not pre-emptively fail it)", versions)
	}
}

// TestHandle_DuplicateDeliveryWhilePendingResumesSameVersion covers "the
// worker crashed after ensureClassification's Create but before
// completing": a pending classification row already exists with this
// exact job's ID, so Handle must resume it (not create a second version)
// and proceed normally.
func TestHandle_DuplicateDeliveryWhilePendingResumesSameVersion(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: `{"summary":"resumed"}`}, nil
	}}
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)

	if _, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: post.ID, Version: 1, Provider: "openai", Model: "test-model", PromptVersion: PromptVersion,
		Status: domain.ClassificationPending, JobID: &job.ID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.Handle(t.Context(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1", provider.calls)
	}
	versions, err := db.Classifications.ListVersions(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1 (resumed, not duplicated)", len(versions))
	}
	if versions[0].Status != domain.ClassificationComplete {
		t.Errorf("Status = %q, want complete", versions[0].Status)
	}
}

// TestHandle_DuplicateDeliveryAfterCompleteSkipsRegeneration is the
// scenario this package's ensureClassification doc comment names
// directly: the job's own jobs.Manager.Succeed() write failed to persist
// after Handle already committed a complete, activated classification,
// so the same job is redelivered once its lease expires. Handle must
// recognize its own job.ID already reached a terminal state and skip
// reprocessing entirely - the gap a naive next-version-number scheme
// would miss (see this branch's commit history).
func TestHandle_DuplicateDeliveryAfterCompleteSkipsRegeneration(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{Content: `{"summary":"first"}`}, nil
	}}
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)

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

	versions, err := db.Classifications.ListVersions(t.Context(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1 despite duplicate delivery", len(versions))
	}
	if versions[0].Summary == nil || *versions[0].Summary != "first" {
		t.Errorf("Summary = %v, want unchanged %q", versions[0].Summary, "first")
	}
}

func TestHandle_DuplicateDeliveryAfterPermanentFailureSkipsRetry(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, NewProviderError(CategoryAuth, errors.New("invalid api key"))
	}}
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)

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

func TestHandle_MalformedPayloadIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		t.Fatal("provider must not be called for a malformed payload")
		return CompletionResult{}, nil
	}}
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, "not-json", 0)

	err := svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
}

func TestHandle_UnsupportedPromptVersionIsPermanent(t *testing.T) {
	provider := &fakeProvider{fn: func(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
		t.Fatal("provider must not be called for an unsupported prompt version")
		return CompletionResult{}, nil
	}}
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, `{"promptVersion":"bogus"}`, 0)

	err := svc.Handle(t.Context(), job)
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
	svc, db := newTestSetup(t, provider, 5)
	post := mustCreatePost(t, db, "body", time.Now())
	job := newEnqueuedTestJob(t, db, post.ID, mustPayload(t), 0)
	job.SourceEntryID = nil

	err := svc.Handle(t.Context(), job)
	var perm *jobs.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Handle() error = %v, want *jobs.PermanentError", err)
	}
}
