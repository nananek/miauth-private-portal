package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestLLMGenerationRepository_CreateAndComplete(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	target := mustCreateThreadAndRoot(t, db, actorID, now)

	g := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: target.ID, Kind: domain.GenerationReply, Provider: "test-provider",
		Model: "test-model", PromptVersion: "v1", Status: domain.GenerationPending, RequestedAt: now,
	}
	if err := db.Generations.Create(t.Context(), g); err != nil {
		t.Fatal(err)
	}

	reply := domain.Entry{
		ID: domain.NewID(), ThreadID: target.ThreadID, ParentEntryID: &target.ID, Kind: domain.EntryLLMReply,
		AuthorActorID: actorID, Body: "generated reply", ProcessingStatus: domain.ProcessingNone,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Entries.Create(t.Context(), reply); err != nil {
		t.Fatal(err)
	}

	tokens := 42
	if err := db.Generations.Complete(t.Context(), g.ID, reply.ID, "generated reply", &tokens, &tokens, now); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := db.Generations.Get(t.Context(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.GenerationComplete {
		t.Errorf("Status = %q, want complete", got.Status)
	}
	if got.ResultEntryID == nil || *got.ResultEntryID != reply.ID {
		t.Errorf("ResultEntryID = %v, want %q", got.ResultEntryID, reply.ID)
	}
	if got.PromptTokens == nil || *got.PromptTokens != tokens {
		t.Errorf("PromptTokens = %v, want %d", got.PromptTokens, tokens)
	}
}

func TestLLMGenerationRepository_Fail(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	target := mustCreateThreadAndRoot(t, db, actorID, now)

	g := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: target.ID, Kind: domain.GenerationFollowUp, Provider: "test-provider",
		Model: "test-model", PromptVersion: "v1", Status: domain.GenerationPending, RequestedAt: now,
	}
	if err := db.Generations.Create(t.Context(), g); err != nil {
		t.Fatal(err)
	}

	if err := db.Generations.Fail(t.Context(), g.ID, "provider_unavailable", now); err != nil {
		t.Fatal(err)
	}

	got, err := db.Generations.Get(t.Context(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.GenerationFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.ResultEntryID != nil {
		t.Error("a failed generation must never be linked to a result entry")
	}
}

// TestLLMGenerationRepository_Create_RejectsSecondConcurrentPending backs
// the schema's partial unique index: at most one pending generation per
// (target, kind), so a retried job cannot enqueue a duplicate attempt.
func TestLLMGenerationRepository_Create_RejectsSecondConcurrentPending(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	target := mustCreateThreadAndRoot(t, db, actorID, now)

	first := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: target.ID, Kind: domain.GenerationReply, Provider: "p",
		Model: "m", PromptVersion: "v1", Status: domain.GenerationPending, RequestedAt: now,
	}
	if err := db.Generations.Create(t.Context(), first); err != nil {
		t.Fatal(err)
	}

	second := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: target.ID, Kind: domain.GenerationReply, Provider: "p",
		Model: "m", PromptVersion: "v1", Status: domain.GenerationPending, RequestedAt: now,
	}
	err := db.Generations.Create(t.Context(), second)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create() error = %v, want ErrConflict", err)
	}
}

func TestLLMGenerationRepository_ListByTarget(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	target := mustCreateThreadAndRoot(t, db, actorID, now)

	reply := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: target.ID, Kind: domain.GenerationReply, Provider: "p", Model: "m",
		PromptVersion: "v1", Status: domain.GenerationPending, RequestedAt: now,
	}
	followUp := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: target.ID, Kind: domain.GenerationFollowUp, Provider: "p", Model: "m",
		PromptVersion: "v1", Status: domain.GenerationPending, RequestedAt: now.Add(time.Second),
	}
	if err := db.Generations.Create(t.Context(), reply); err != nil {
		t.Fatal(err)
	}
	if err := db.Generations.Create(t.Context(), followUp); err != nil {
		t.Fatal(err)
	}

	got, err := db.Generations.ListByTarget(t.Context(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
}
