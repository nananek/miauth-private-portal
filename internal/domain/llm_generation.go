package domain

import (
	"context"
	"time"
)

// GenerationKind distinguishes an LLM reply from a follow-up question, so
// Aria-facing code can present them differently and a user's answer to a
// follow-up can be routed back into the same thread.
type GenerationKind string

const (
	GenerationReply    GenerationKind = "reply"
	GenerationFollowUp GenerationKind = "follow_up_question"
)

// GenerationStatus tracks one generation attempt.
type GenerationStatus string

const (
	GenerationPending  GenerationStatus = "pending"
	GenerationComplete GenerationStatus = "complete"
	GenerationFailed   GenerationStatus = "failed"
)

// LLMGeneration is the attempt/audit record for one LLM reply or
// follow-up-question generation. Only a Complete generation is linked to
// a produced Entry (ResultEntryID); pending or failed generations never
// appear as timeline entries, so a stopped LLM cannot make a user's post
// disappear, and a retried generation cannot duplicate a visible reply.
type LLMGeneration struct {
	ID            string
	TargetEntryID string
	// ResultEntryID is set only once Complete produces a new Entry.
	ResultEntryID    *string
	Kind             GenerationKind
	Provider         string
	Model            string
	PromptVersion    string
	Status           GenerationStatus
	ErrorCategory    *string
	Body             *string
	PromptTokens     *int
	CompletionTokens *int
	JobID            *string
	RequestedAt      time.Time
	GeneratedAt      *time.Time
}

// LLMGenerationRepository persists LLM generation attempts.
type LLMGenerationRepository interface {
	Create(ctx context.Context, g LLMGeneration) error
	Get(ctx context.Context, id string) (LLMGeneration, error)
	ListByTarget(ctx context.Context, targetEntryID string) ([]LLMGeneration, error)
	// Complete marks g as successfully generated and links it to
	// resultEntryID. Call together with EntryRepository.Create for
	// resultEntryID inside the same UnitOfWork transaction.
	Complete(ctx context.Context, id, resultEntryID, body string, promptTokens, completionTokens *int, at time.Time) error
	Fail(ctx context.Context, id, errorCategory string, at time.Time) error
}
