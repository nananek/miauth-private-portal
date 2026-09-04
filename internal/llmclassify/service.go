package llmclassify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
)

// JobType is the durable job type this package's Handle registers under
// (cmd/server: jobsManager.Register(llmclassify.JobType, service.Handle)).
const JobType = "llm_classification"

// jobPayload is the JSON internal/httpserver's enqueue hook stores on a
// domain.Job{JobType: JobType}. Unlike internal/llmreply's payload, there
// is no per-post decision to encode: classification is enqueued
// unconditionally for every EntryUserPost (see package doc), so every
// payload this package ever produces is identical. The target entry ID
// is not duplicated here either, for the same reason as
// internal/llmreply's payload: it comes from job.SourceEntryID, which
// timeline.Service's enqueueForEntry sets atomically alongside the entry
// it was enqueued for.
type jobPayload struct {
	PromptVersion string `json:"promptVersion"`
}

// NewJobPayload encodes this package's fixed job payload, for
// internal/httpserver to attach to the domain.Job it passes into
// timeline.Service.CreateRoot/CreateReply. Keeping the encoding here
// (rather than in internal/httpserver) means only this package needs to
// change if the payload shape ever does.
func NewJobPayload() (string, error) {
	b, err := json.Marshal(jobPayload{PromptVersion: PromptVersion})
	if err != nil {
		return "", fmt.Errorf("llmclassify: encode job payload: %w", err)
	}
	return string(b), nil
}

// Config configures Service's provider identity, generation limits, and
// related-post candidate budget.
type Config struct {
	// ProviderName and Model are recorded on every domain.LLMClassification
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

// Service implements Issue #10's "llm_classification" job: given a
// target user_post entry, it builds a bounded same-thread candidate
// prompt, calls Provider, validates/normalizes the structured output,
// and atomically records a new classification version. Unlike
// internal/llmreply, it never depends on internal/timeline: classifying
// a post never creates a new Entry, only versioned metadata about an
// existing one. Wire it with jobsManager.Register(JobType, service.Handle).
type Service struct {
	uow      domain.UnitOfWork
	repos    domain.Repos
	provider Provider
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time
}

// NewService builds a Service. uow and repos commonly come from one
// storage adapter (see internal/storage/sqlite.DB, which implements
// both).
func NewService(uow domain.UnitOfWork, repos domain.Repos, provider Provider, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{uow: uow, repos: repos, provider: provider, cfg: cfg, logger: logger, now: time.Now}
}

// Handle implements internal/jobs.Handler for JobType.
func (s *Service) Handle(ctx context.Context, job domain.Job) error {
	var payload jobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return jobs.Permanent(fmt.Errorf("llmclassify: decode job payload: %w", err))
	}
	if payload.PromptVersion != PromptVersion {
		return jobs.Permanent(fmt.Errorf("llmclassify: unsupported prompt version %q", payload.PromptVersion))
	}
	if job.SourceEntryID == nil || *job.SourceEntryID == "" {
		return jobs.Permanent(errors.New("llmclassify: job missing source entry id"))
	}
	targetEntryID := *job.SourceEntryID

	target, err := s.repos.Entries.Get(ctx, targetEntryID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("llmclassify: target entry not found: %w", err))
		}
		return fmt.Errorf("llmclassify: get target entry: %w", err)
	}

	classificationID, version, created, err := s.ensureClassification(ctx, targetEntryID, job.ID, payload.PromptVersion)
	if err != nil {
		return err
	}
	if !created {
		s.logger.Info("llm classification already terminal, skipping duplicate delivery", "job_id", job.ID, "entry_id", targetEntryID, "version", version)
		return nil
	}

	// Best-effort observability only; correctness depends solely on the
	// (entry, job) version resolution above and Complete/Fail's own
	// pending-guarded transitions, never on this status value.
	if err := s.repos.Entries.SetProcessingStatus(ctx, targetEntryID, domain.ProcessingInProgress, s.now().UTC()); err != nil {
		s.logger.Warn("set processing status in-progress failed", "entry_id", targetEntryID, "error_category", "storage_error")
	}

	threadEntries, err := s.repos.Entries.ListByThread(ctx, target.ThreadID)
	if err != nil {
		return fmt.Errorf("llmclassify: list thread: %w", err)
	}
	candidates := BuildCandidates(threadEntries, target.ID, s.cfg.ThreadContext)
	messages := BuildMessages(candidates, target)

	result, completeErr := s.provider.Complete(ctx, CompletionRequest{Messages: messages, MaxOutputTokens: s.cfg.MaxOutputTokens})
	if completeErr != nil {
		return s.handleProviderFailure(ctx, targetEntryID, version, job, completeErr)
	}

	fields, parseErr := ParseAndNormalize(result.Content)
	if parseErr != nil {
		return s.handleProviderFailure(ctx, targetEntryID, version, job, NewProviderError(CategoryMalformedResponse, parseErr))
	}
	validRelated := validateRelatedIDs(fields.RelatedEntryIDs, candidates, target.ID)

	structuredOutput, encodeErr := fields.StructuredOutputJSON()
	if encodeErr != nil {
		return fmt.Errorf("llmclassify: encode structured output: %w", encodeErr)
	}
	summary := ""
	if fields.Summary != nil {
		summary = *fields.Summary
	}

	now := s.now().UTC()
	err = s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Classifications.Complete(ctx, targetEntryID, version, summary, structuredOutput,
			fields.Priority, fields.NotebookCandidate, fields.ReviewCandidate, fields.Unresolved,
			result.PromptTokens, result.CompletionTokens, now); err != nil {
			return err
		}
		for _, tag := range fields.Tags {
			if err := repos.Classifications.AddTag(ctx, classificationID, tag); err != nil {
				return err
			}
		}
		for _, relatedID := range validRelated {
			if err := repos.Classifications.AddRelatedEntry(ctx, classificationID, relatedID); err != nil {
				return err
			}
		}
		if err := repos.Classifications.Activate(ctx, targetEntryID, version); err != nil {
			return err
		}
		return repos.Entries.SetProcessingStatus(ctx, targetEntryID, domain.ProcessingComplete, now)
	})
	if err != nil {
		return fmt.Errorf("llmclassify: complete classification: %w", err)
	}

	s.logger.Info("llm classification completed", "job_id", job.ID, "entry_id", targetEntryID, "version", version)
	return nil
}

// ensureClassification finds or creates the classification version this
// exact job (job.ID) owns for entryID, idempotently: id and version name
// the row this attempt must act on, and created is false when a prior
// delivery of this identical job already reached a terminal
// (complete/failed) state, so the caller must skip reprocessing rather
// than classify again and produce a duplicate/conflicting version.
//
// Unlike internal/llmreply's ensureGeneration, this cannot rely on a
// deterministic primary key (LLMClassification.ID is AUTOINCREMENT, not
// caller-assignable): instead it searches existing versions for one
// already carrying this job.ID. That lookup, not a database conflict, is
// what makes "duplicate delivery after this job's classification already
// completed" detectable no matter how long after the fact the duplicate
// arrives (for example, if the job's own jobs.Manager.Succeed() write
// failed to persist after Handle already committed its result, leaving
// the job claimable again once its lease expires). A domain.ErrConflict
// from Create is still handled, as a narrower safety net for two
// deliveries racing to compute the same next version concurrently.
func (s *Service) ensureClassification(ctx context.Context, entryID, jobID, promptVersion string) (id int64, version int, created bool, err error) {
	versions, err := s.repos.Classifications.ListVersions(ctx, entryID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("llmclassify: list existing versions: %w", err)
	}
	for _, v := range versions {
		if v.JobID != nil && *v.JobID == jobID {
			return v.ID, v.Version, v.Status == domain.ClassificationPending, nil
		}
	}

	next := len(versions) + 1
	newID, createErr := s.repos.Classifications.Create(ctx, domain.LLMClassification{
		EntryID: entryID, Version: next, Provider: s.cfg.ProviderName, Model: s.cfg.Model,
		PromptVersion: promptVersion, Status: domain.ClassificationPending, JobID: &jobID,
		CreatedAt: s.now().UTC(),
	})
	if createErr == nil {
		return newID, next, true, nil
	}
	if !errors.Is(createErr, domain.ErrConflict) {
		return 0, 0, false, fmt.Errorf("llmclassify: create classification record: %w", createErr)
	}

	// Lost a race against a concurrent delivery that computed the same
	// next version first; re-read to find whichever row actually won.
	versions, listErr := s.repos.Classifications.ListVersions(ctx, entryID)
	if listErr != nil {
		return 0, 0, false, fmt.Errorf("llmclassify: list versions after conflict: %w", listErr)
	}
	for _, v := range versions {
		if v.Version == next {
			return v.ID, v.Version, v.Status == domain.ClassificationPending, nil
		}
	}
	return 0, 0, false, fmt.Errorf("llmclassify: conflicted version %d not found after re-list", next)
}

// fail best-effort transitions entryID's version to failed and the
// entry's ProcessingStatus to failed. Classifications.Fail's own WHERE
// status='pending' guard makes this safe to call even if another path
// already finalized the row (it then reports domain.ErrConflict, which
// is expected, not logged as a failure).
func (s *Service) fail(ctx context.Context, entryID string, version int, category string) {
	if err := s.repos.Classifications.Fail(ctx, entryID, version, category, s.now().UTC()); err != nil && !errors.Is(err, domain.ErrConflict) {
		s.logger.Warn("llm classification fail-transition failed", "entry_id", entryID, "version", version, "error_category", category)
	}
	if err := s.repos.Entries.SetProcessingStatus(ctx, entryID, domain.ProcessingFailed, s.now().UTC()); err != nil {
		s.logger.Warn("set processing status failed failed", "entry_id", entryID, "error_category", "storage_error")
	}
}

// handleProviderFailure classifies a Provider.Complete (or schema
// validation) failure and decides the job's outcome, mirroring
// internal/llmreply.Service.handleProviderFailure exactly:
//
//   - a permanent category fails the classification immediately and
//     returns jobs.Permanent so the job reaches its own failed terminal
//     state without further retries;
//   - a retryable category on what will be the job's last attempt also
//     fails the classification before returning, so it does not stay
//     silently pending forever once the job itself goes dead; on any
//     earlier attempt the classification is left pending and a plain
//     error defers to the ordinary job retry.
//
// The "last attempt" check is skipped when ctx is already cancelled (job
// lease loss or worker shutdown), for the same reason
// internal/llmreply's version documents: internal/jobs.Manager retries a
// cancelled handler unconditionally, independent of MaxAttempts.
func (s *Service) handleProviderFailure(ctx context.Context, entryID string, version int, job domain.Job, err error) error {
	category := ClassifyProviderError(err)
	if category.IsPermanent() {
		s.fail(ctx, entryID, version, string(category))
		return jobs.Permanent(fmt.Errorf("llmclassify: classification failed: %s", category))
	}

	if ctx.Err() == nil && job.Attempt+1 >= s.cfg.MaxAttempts {
		s.fail(ctx, entryID, version, string(category))
	}
	return fmt.Errorf("llmclassify: classification attempt failed: %s", category)
}
