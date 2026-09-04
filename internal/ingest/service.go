package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// JobType is the durable job type this package's Handle registers under
// (cmd/server: jobsManager.Register(ingest.JobType, service.Handle)).
// There is deliberately one job type for every adapter kind (not one per
// kind): Handle dispatches to the right Adapter itself via
// source.Kind, so Issue #12 adding an IMAP adapter only needs a new
// Service.RegisterAdapter call, never a second jobsManager.Register.
const JobType = "external_source_poll"

// jobPayload is the JSON Scheduler stores on a domain.Job{JobType:
// JobType}. Only the source ID is carried: everything else Handle needs
// (kind, uri, stored cursor) is looked up fresh from
// domain.ExternalSourceRepository, so a payload never goes stale even if
// a source's configuration changes between enqueue and claim.
type jobPayload struct {
	SourceID string `json:"sourceId"`
}

// NewJobPayload encodes sourceID as this package's job payload, for
// Scheduler to attach to the domain.Job it enqueues.
func NewJobPayload(sourceID string) (string, error) {
	b, err := json.Marshal(jobPayload{SourceID: sourceID})
	if err != nil {
		return "", fmt.Errorf("ingest: encode job payload: %w", err)
	}
	return string(b), nil
}

// Service implements Issue #11's "external_source_poll" job: given a
// configured domain.ExternalSource, it dispatches to the Adapter
// registered for the source's Kind, and — for each fetched item —
// atomically dedupes and promotes it into the timeline through
// timeline.Service.CreateExternalEntry. Wire it with
// jobsManager.Register(JobType, service.Handle) only once at least one
// adapter is registered.
type Service struct {
	repos    domain.Repos
	timeline *timeline.Service
	adapters map[string]Adapter
	logger   *slog.Logger
	now      func() time.Time
}

// NewService builds a Service. repos is the standalone (non-
// transactional) domain.Repos most callers use directly (see
// internal/storage/sqlite.DB); atomicity for a fetched item's dedupe and
// entry creation comes from timelineSvc.CreateExternalEntry, not from
// repos here.
func NewService(repos domain.Repos, timelineSvc *timeline.Service, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repos: repos, timeline: timelineSvc, adapters: make(map[string]Adapter), logger: logger, now: time.Now}
}

// RegisterAdapter makes a reachable under its own Kind(). Registration
// is a startup operation and must not race with Handle being invoked by
// internal/jobs.Manager.
func (s *Service) RegisterAdapter(a Adapter) {
	if a == nil {
		panic("ingest: register nil adapter")
	}
	kind := a.Kind()
	if kind == "" {
		panic("ingest: adapter Kind() must not be empty")
	}
	s.adapters[kind] = a
}

// Handle implements internal/jobs.Handler for JobType.
func (s *Service) Handle(ctx context.Context, job domain.Job) error {
	var payload jobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return jobs.Permanent(fmt.Errorf("ingest: decode job payload: %w", err))
	}
	if payload.SourceID == "" {
		return jobs.Permanent(errors.New("ingest: job missing source id"))
	}

	source, err := s.repos.ExternalSources.Get(ctx, payload.SourceID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("ingest: source not found: %w", err))
		}
		return fmt.Errorf("ingest: get source: %w", err)
	}

	adapter, ok := s.adapters[source.Kind]
	if !ok {
		// A source referencing a kind with no registered adapter (a
		// stale config after an operator disabled a feature, or a
		// source row created by a future version) can never succeed by
		// retrying, but is also not this job's fault: fail permanently
		// rather than retrying forever, matching how llmclassify/
		// llmreply treat an unrecognized payload shape.
		return jobs.Permanent(fmt.Errorf("ingest: no adapter registered for source kind %q", source.Kind))
	}

	result, fetchErr := adapter.Fetch(ctx, source, source.Cursor)
	if fetchErr != nil {
		return s.handleFetchFailure(ctx, source.ID, fetchErr)
	}

	if result.NotModified {
		if err := s.repos.ExternalSources.RecordFetchSuccess(ctx, source.ID, nil, s.now().UTC()); err != nil {
			return fmt.Errorf("ingest: record fetch success: %w", err)
		}
		s.logger.Info("external source poll completed", "job_id", job.ID, "source_id", source.ID,
			"source_kind", source.Kind, "not_modified", true)
		return nil
	}

	entryKind := entryKindForSourceKind(source.Kind)
	created := 0
	for _, item := range result.Items {
		domainItem := domain.ExternalItem{
			SourceID:      source.ID,
			ExternalID:    item.ExternalID,
			ProvenanceURL: item.ProvenanceURL,
			PublishedAt:   item.PublishedAt,
			DedupeKey:     item.DedupeKey,
		}
		_, wasCreated, err := s.timeline.CreateExternalEntry(ctx, entryKind, domainItem, item.Body)
		if err != nil {
			// A failure partway through the batch must not advance the
			// cursor past unprocessed items (they would then be lost
			// forever): record the failure for observability and return
			// a plain, retryable error so internal/jobs retries this
			// entire batch. internal/timeline.CreateExternalEntry's own
			// dedupe-key conflict handling makes re-processing the
			// items that already succeeded here idempotent.
			if failErr := s.repos.ExternalSources.RecordFetchFailure(ctx, source.ID, "storage_error", s.now().UTC()); failErr != nil {
				s.logger.Warn("record fetch failure failed", "source_id", source.ID, "error_category", "storage_error")
			}
			return fmt.Errorf("ingest: create external entry: %w", err)
		}
		if wasCreated {
			created++
		}
	}

	var cursor *string
	if result.NextCursor != "" {
		cursor = &result.NextCursor
	}
	if err := s.repos.ExternalSources.RecordFetchSuccess(ctx, source.ID, cursor, s.now().UTC()); err != nil {
		return fmt.Errorf("ingest: record fetch success: %w", err)
	}

	s.logger.Info("external source poll completed", "job_id", job.ID, "source_id", source.ID,
		"source_kind", source.Kind, "items_fetched", len(result.Items), "items_created", created)
	return nil
}

func (s *Service) handleFetchFailure(ctx context.Context, sourceID string, fetchErr error) error {
	category := ClassifyFetchError(fetchErr)
	if err := s.repos.ExternalSources.RecordFetchFailure(ctx, sourceID, string(category), s.now().UTC()); err != nil {
		s.logger.Warn("record fetch failure failed", "source_id", sourceID, "error_category", "storage_error")
	}
	if category.IsPermanent() {
		return jobs.Permanent(fmt.Errorf("ingest: fetch failed: %s", category))
	}
	return fmt.Errorf("ingest: fetch failed: %s", category)
}

// entryKindForSourceKind maps a domain.ExternalSource.Kind to the
// domain.EntryKind its ingested items are created as. "imap" (Issue
// #12) maps to EntryMail; every other registered kind, including "rss",
// maps to EntryNews.
func entryKindForSourceKind(sourceKind string) domain.EntryKind {
	if sourceKind == "imap" {
		return domain.EntryMail
	}
	return domain.EntryNews
}
