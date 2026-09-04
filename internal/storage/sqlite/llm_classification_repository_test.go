package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestLLMClassificationRepository_CreateActivateAndGetActive(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	summary := "a summary"
	id, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
		Status: domain.ClassificationComplete, Summary: &summary, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("expected a nonzero generated classification ID")
	}

	if err := db.Classifications.AddTag(t.Context(), id, "go"); err != nil {
		t.Fatal(err)
	}
	if err := db.Classifications.AddTag(t.Context(), id, "sqlite"); err != nil {
		t.Fatal(err)
	}

	if err := db.Classifications.Activate(t.Context(), entry.ID, 1); err != nil {
		t.Fatalf("activate: %v", err)
	}

	got, err := db.Classifications.GetActive(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsActive {
		t.Error("GetActive() returned a classification that is not active")
	}
	if len(got.Tags) != 2 {
		t.Errorf("len(Tags) = %d, want 2", len(got.Tags))
	}
}

// TestLLMClassificationRepository_Activate_DeactivatesPreviousVersion
// backs the "user-authored data and LLM classification data are
// version-managed" acceptance criterion: activating a new version must
// leave exactly one active version per entry (enforced by the schema's
// partial unique index).
func TestLLMClassificationRepository_Activate_DeactivatesPreviousVersion(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	if _, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
		Status: domain.ClassificationComplete, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Classifications.Activate(t.Context(), entry.ID, 1); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 2, Provider: "p", Model: "m", PromptVersion: "v2",
		Status: domain.ClassificationComplete, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Classifications.Activate(t.Context(), entry.ID, 2); err != nil {
		t.Fatal(err)
	}

	active, err := db.Classifications.GetActive(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Version != 2 {
		t.Errorf("active version = %d, want 2", active.Version)
	}

	versions, err := db.Classifications.ListVersions(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeCount := 0
	for _, v := range versions {
		if v.IsActive {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("active version count = %d, want exactly 1", activeCount)
	}
}

func TestLLMClassificationRepository_Create_RejectsDuplicateVersion(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	if _, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
		Status: domain.ClassificationComplete, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
		Status: domain.ClassificationComplete, CreatedAt: now,
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create() error = %v, want ErrConflict", err)
	}
}

func TestLLMClassificationRepository_AddRelatedEntry(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)
	related := mustCreateThreadAndRoot(t, db, actorID, now.Add(time.Minute))

	id, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
		Status: domain.ClassificationComplete, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Classifications.AddRelatedEntry(t.Context(), id, related.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Classifications.Activate(t.Context(), entry.ID, 1); err != nil {
		t.Fatal(err)
	}

	got, err := db.Classifications.GetActive(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RelatedEntryIDs) != 1 || got.RelatedEntryIDs[0] != related.ID {
		t.Errorf("RelatedEntryIDs = %v, want [%s]", got.RelatedEntryIDs, related.ID)
	}
}

func TestLLMClassificationRepository_CompleteAndFail(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	if _, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
		Status: domain.ClassificationPending, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	priority := "high"
	tokens := 7
	if err := db.Classifications.Complete(t.Context(), entry.ID, 1, "summary", `{"subject":"x"}`,
		&priority, true, true, true, &tokens, &tokens, now); err != nil {
		t.Fatalf("complete: %v", err)
	}

	versions, err := db.Classifications.ListVersions(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	got := versions[0]
	if got.Status != domain.ClassificationComplete {
		t.Errorf("Status = %q, want complete", got.Status)
	}
	if got.Summary == nil || *got.Summary != "summary" {
		t.Errorf("Summary = %v, want %q", got.Summary, "summary")
	}
	if got.Priority == nil || *got.Priority != "high" {
		t.Errorf("Priority = %v, want high", got.Priority)
	}
	if !got.NotebookCandidate || !got.ReviewCandidate || !got.Unresolved {
		t.Errorf("flags = (%v, %v, %v), want all true", got.NotebookCandidate, got.ReviewCandidate, got.Unresolved)
	}
	if got.PromptTokens == nil || *got.PromptTokens != tokens {
		t.Errorf("PromptTokens = %v, want %d", got.PromptTokens, tokens)
	}

	// A second Complete against the now-terminal row must report
	// ErrConflict, mirroring LLMGenerationRepository.Complete's
	// duplicate-delivery guard.
	if err := db.Classifications.Complete(t.Context(), entry.ID, 1, "again", "{}", nil, false, false, false, nil, nil, now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second Complete() error = %v, want ErrConflict", err)
	}
}

func TestLLMClassificationRepository_Fail(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	if _, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
		EntryID: entry.ID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
		Status: domain.ClassificationPending, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.Classifications.Fail(t.Context(), entry.ID, 1, "malformed_response", now); err != nil {
		t.Fatalf("fail: %v", err)
	}

	versions, err := db.Classifications.ListVersions(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].Status != domain.ClassificationFailed {
		t.Fatalf("versions = %+v, want one failed classification", versions)
	}
	if versions[0].ErrorCategory == nil || *versions[0].ErrorCategory != "malformed_response" {
		t.Errorf("ErrorCategory = %v, want malformed_response", versions[0].ErrorCategory)
	}

	// A second Fail against the now-terminal row must report ErrConflict.
	if err := db.Classifications.Fail(t.Context(), entry.ID, 1, "timeout", now); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second Fail() error = %v, want ErrConflict", err)
	}
}

func TestLLMClassificationRepository_Complete_ConflictWhenNotPending(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	err := db.Classifications.Complete(t.Context(), entry.ID, 1, "summary", "{}", nil, false, false, false, nil, nil, now)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Complete() on nonexistent row error = %v, want ErrConflict", err)
	}
}

func TestLLMClassificationRepository_ListCandidates(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	reviewEntry := mustCreateThreadAndRoot(t, db, actorID, now)
	notebookEntry := mustCreateThreadAndRoot(t, db, actorID, now.Add(time.Minute))
	unresolvedEntry := mustCreateThreadAndRoot(t, db, actorID, now.Add(2*time.Minute))
	plainEntry := mustCreateThreadAndRoot(t, db, actorID, now.Add(3*time.Minute))

	createActive := func(entryID string, notebook, review, unresolved bool) {
		t.Helper()
		if _, err := db.Classifications.Create(t.Context(), domain.LLMClassification{
			EntryID: entryID, Version: 1, Provider: "p", Model: "m", PromptVersion: "v1",
			Status: domain.ClassificationPending, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Classifications.Complete(t.Context(), entryID, 1, "s", "{}", nil, notebook, review, unresolved, nil, nil, now); err != nil {
			t.Fatal(err)
		}
		if err := db.Classifications.Activate(t.Context(), entryID, 1); err != nil {
			t.Fatal(err)
		}
	}

	createActive(reviewEntry.ID, false, true, false)
	createActive(notebookEntry.ID, true, false, false)
	createActive(unresolvedEntry.ID, false, false, true)
	createActive(plainEntry.ID, false, false, false)

	reviewCandidates, err := db.Classifications.ListReviewCandidates(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewCandidates) != 1 || reviewCandidates[0].EntryID != reviewEntry.ID {
		t.Errorf("ListReviewCandidates() = %+v, want only %s", reviewCandidates, reviewEntry.ID)
	}

	notebookCandidates, err := db.Classifications.ListNotebookCandidates(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notebookCandidates) != 1 || notebookCandidates[0].EntryID != notebookEntry.ID {
		t.Errorf("ListNotebookCandidates() = %+v, want only %s", notebookCandidates, notebookEntry.ID)
	}

	unresolved, err := db.Classifications.ListUnresolved(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 || unresolved[0].EntryID != unresolvedEntry.ID {
		t.Errorf("ListUnresolved() = %+v, want only %s", unresolved, unresolvedEntry.ID)
	}
}
