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
