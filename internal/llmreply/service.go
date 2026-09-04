package llmreply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// JobType is the durable job type this package's Handle registers under
// (cmd/server: jobsManager.Register(llmreply.JobType, service.Handle)).
const JobType = "llm_generation"

// jobPayload is the JSON internal/httpserver's enqueue hook stores on a
// domain.Job{JobType: JobType}. The target entry ID is deliberately not
// duplicated here: it comes from job.SourceEntryID, which
// timeline.Service's enqueueForEntry already sets atomically alongside
// the entry it was enqueued for.
type jobPayload struct {
	Kind          domain.GenerationKind `json:"kind"`
	PromptVersion string                `json:"promptVersion"`
}

// NewJobPayload encodes decision as this package's job payload, for
// internal/httpserver to attach to the domain.Job it passes into
// timeline.Service.CreateRoot/CreateReply. Keeping the encoding here
// (rather than in internal/httpserver) means only this package needs to
// change if the payload shape ever does.
func NewJobPayload(decision ReplyDecision) (string, error) {
	b, err := json.Marshal(jobPayload{Kind: decision.Kind, PromptVersion: decision.PolicyVersion})
	if err != nil {
		return "", fmt.Errorf("llmreply: encode job payload: %w", err)
	}
	return string(b), nil
}

// Config configures Service's provider identity and generation limits.
type Config struct {
	// ProviderName and Model are recorded on every domain.LLMGeneration
	// row (provider/model provenance), independent of PromptVersion.
	ProviderName    string
	Model           string
	MaxOutputTokens int
	ThreadContext   ContextBudget
	// MaxAttempts must mirror internal/jobs.Config.MaxAttempts (the same
	// value cmd/server passes to jobs.NewManager), so Handle can tell
	// whether the attempt it is running is the job's last one.
	MaxAttempts int
}

// Service implements Issue #9's "llm_generation" job: given a target
// entry and a decided kind, it builds a bounded thread-context prompt,
// calls Provider, and atomically records the result through
// timeline.Service.CreateGeneratedReply. Wire it with
// jobsManager.Register(JobType, service.Handle).
type Service struct {
	repos    domain.Repos
	timeline *timeline.Service
	provider Provider
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time
}

// NewService builds a Service. repos is the standalone (non-transactional)
// domain.Repos most callers use directly (see internal/storage/sqlite.DB);
// atomicity for the generated entry + completed generation record comes
// from timelineSvc.CreateGeneratedReply, not from repos here.
func NewService(repos domain.Repos, timelineSvc *timeline.Service, provider Provider, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repos: repos, timeline: timelineSvc, provider: provider, cfg: cfg, logger: logger, now: time.Now}
}

// Handle implements internal/jobs.Handler for JobType.
func (s *Service) Handle(ctx context.Context, job domain.Job) error {
	var payload jobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return jobs.Permanent(fmt.Errorf("llmreply: decode job payload: %w", err))
	}
	if payload.Kind != domain.GenerationReply && payload.Kind != domain.GenerationFollowUp {
		return jobs.Permanent(fmt.Errorf("llmreply: unknown generation kind %q", payload.Kind))
	}
	if job.SourceEntryID == nil || *job.SourceEntryID == "" {
		return jobs.Permanent(errors.New("llmreply: job missing source entry id"))
	}
	targetEntryID := *job.SourceEntryID

	// A deterministic generation ID keyed by job ID makes Create's
	// duplicate-delivery outcome (domain.ErrConflict) mean exactly "this
	// exact job was already attempted", independent of the
	// (target_entry_id, kind) pending-uniqueness constraint's own,
	// separate purpose (at most one concurrently in-flight generation per
	// target+kind).
	generationID := "llmgen:" + job.ID
	created, err := s.ensureGeneration(ctx, domain.LLMGeneration{
		ID:            generationID,
		TargetEntryID: targetEntryID,
		Kind:          payload.Kind,
		Provider:      s.cfg.ProviderName,
		Model:         s.cfg.Model,
		PromptVersion: payload.PromptVersion,
		Status:        domain.GenerationPending,
		JobID:         &job.ID,
		RequestedAt:   s.now().UTC(),
	})
	if err != nil {
		return err
	}
	if !created {
		s.logger.Info("llm generation already terminal, skipping duplicate delivery", "job_id", job.ID, "generation_id", generationID)
		return nil
	}

	target, err := s.repos.Entries.Get(ctx, targetEntryID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return s.failPermanent(ctx, generationID, "target_not_found", err)
		}
		return fmt.Errorf("llmreply: get target entry: %w", err)
	}

	threadEntries, err := s.repos.Entries.ListByThread(ctx, target.ThreadID)
	if err != nil {
		return fmt.Errorf("llmreply: list thread: %w", err)
	}

	threadContext := BuildThreadContext(threadEntries, target.ID, s.cfg.ThreadContext)
	messages := BuildMessages(payload.Kind, threadContext, target)

	result, completeErr := s.provider.Complete(ctx, CompletionRequest{Messages: messages, MaxOutputTokens: s.cfg.MaxOutputTokens})
	if completeErr != nil {
		return s.handleProviderFailure(ctx, generationID, job, completeErr)
	}

	body := strings.TrimSpace(result.Content)
	if body == "" {
		return s.handleProviderFailure(ctx, generationID, job, NewProviderError(CategoryMalformedResponse, errors.New("empty generated content")))
	}

	entryKind := domain.EntryLLMReply
	if payload.Kind == domain.GenerationFollowUp {
		entryKind = domain.EntryLLMFollowUp
	}
	if _, err := s.timeline.CreateGeneratedReply(ctx, target.ID, entryKind, body, generationID, result.PromptTokens, result.CompletionTokens); err != nil {
		return fmt.Errorf("llmreply: create generated reply: %w", err)
	}

	s.logger.Info("llm generation completed", "job_id", job.ID, "generation_id", generationID, "kind", string(payload.Kind))
	return nil
}

// ensureGeneration inserts gen (status pending) idempotently. created is
// false when a previous delivery of this identical job (same
// deterministic ID) already reached a terminal state, so the caller must
// treat this delivery as already handled rather than generating again
// and producing a duplicate visible reply. A conflict found still
// pending means an earlier attempt's process died before completing
// (crash, hard timeout): created is true so the caller proceeds exactly
// as it would have had Create succeeded outright.
func (s *Service) ensureGeneration(ctx context.Context, gen domain.LLMGeneration) (created bool, err error) {
	err = s.repos.Generations.Create(ctx, gen)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, domain.ErrConflict) {
		return false, fmt.Errorf("llmreply: create generation record: %w", err)
	}
	existing, getErr := s.repos.Generations.Get(ctx, gen.ID)
	if getErr != nil {
		return false, fmt.Errorf("llmreply: get existing generation record after conflict: %w", getErr)
	}
	return existing.Status == domain.GenerationPending, nil
}

// fail best-effort transitions generationID to failed. Generations.Fail's
// own WHERE status='pending' guard makes this safe to call even if
// another path already finalized the row (it then reports
// domain.ErrConflict, which is expected, not logged as a failure).
func (s *Service) fail(ctx context.Context, generationID, category string) {
	if err := s.repos.Generations.Fail(ctx, generationID, category, s.now().UTC()); err != nil && !errors.Is(err, domain.ErrConflict) {
		s.logger.Warn("llm generation fail-transition failed", "generation_id", generationID, "error_category", category)
	}
}

func (s *Service) failPermanent(ctx context.Context, generationID, category string, cause error) error {
	s.fail(ctx, generationID, category)
	return jobs.Permanent(fmt.Errorf("llmreply: %s: %w", category, cause))
}

// handleProviderFailure classifies a Provider.Complete failure and
// decides the job's outcome per Issue #9's plan:
//
//   - a permanent category (auth/client_error/malformed_response/
//     content_refusal) fails the generation immediately and returns
//     jobs.Permanent so the job reaches its own failed terminal state
//     without further retries;
//   - a retryable category (transport/timeout/rate_limit/server_error) on
//     what will be the job's last attempt also fails the generation
//     before returning, so it does not stay silently pending forever once
//     the job itself goes dead; on any earlier attempt the generation is
//     left pending and a plain error defers to the ordinary job retry.
//
// The "last attempt" check is skipped when ctx is already cancelled
// (job lease loss or worker shutdown): internal/jobs.Manager retries a
// cancelled handler unconditionally through its own shutdown path,
// independent of MaxAttempts, so pre-emptively failing the generation
// there could mark it failed even though the job itself will still be
// retried — see internal/provider/openai's Category doc for why a
// cancellation and a genuine timeout are otherwise indistinguishable at
// this layer.
func (s *Service) handleProviderFailure(ctx context.Context, generationID string, job domain.Job, err error) error {
	category := ClassifyProviderError(err)
	if category.IsPermanent() {
		s.fail(ctx, generationID, string(category))
		return jobs.Permanent(fmt.Errorf("llmreply: generation failed: %s", category))
	}

	if ctx.Err() == nil && job.Attempt+1 >= s.cfg.MaxAttempts {
		s.fail(ctx, generationID, string(category))
	}
	return fmt.Errorf("llmreply: generation attempt failed: %s", category)
}
