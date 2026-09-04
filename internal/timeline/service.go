// Package timeline implements the post, reply, edit, visibility, and
// stable timeline use cases. It keeps entry/thread invariants above the
// persistence layer and depends only on internal/domain, never on HTTP,
// SQLite, or an LLM provider.
package timeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

var (
	// ErrParentNotFound reports that CreateReply's parent entry does not
	// exist. CreateReply accepts no separate thread ID: deriving it from
	// the parent makes a cross-thread parent impossible to express.
	ErrParentNotFound = errors.New("timeline: parent entry not found")
	// ErrInvalidKind reports an unknown entry kind or a kind that is not
	// valid for the requested topology, such as an LLM reply at a thread
	// root.
	ErrInvalidKind = errors.New("timeline: kind cannot be created this way")
	// ErrNotEditable reports an attempt to edit anything other than the
	// owner actor's own user-authored post.
	ErrNotEditable = errors.New("timeline: only the author's own user_post can be edited")
)

var authorActorTypeForKind = map[domain.EntryKind]domain.ActorType{
	domain.EntryUserPost:    domain.ActorOwner,
	domain.EntryLLMReply:    domain.ActorAssistant,
	domain.EntryLLMFollowUp: domain.ActorAssistant,
	domain.EntryNews:        domain.ActorSystem,
	domain.EntryMail:        domain.ActorSystem,
	domain.EntrySystem:      domain.ActorSystem,
}

// Config supplies optional timeline service dependencies.
type Config struct {
	// Clock defaults to the real wall clock when nil.
	Clock Clock
}

// Service enforces timeline business rules while composing domain
// repositories through atomic units of work.
type Service struct {
	uow   domain.UnitOfWork
	repos domain.Repos
	clock Clock
}

// NewService builds a timeline Service. uow and repos commonly come
// from one storage adapter, but no concrete adapter type crosses this
// package boundary.
func NewService(uow domain.UnitOfWork, repos domain.Repos, cfg Config) *Service {
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Service{uow: uow, repos: repos, clock: clock}
}

// CreateRoot creates a thread and its root entry atomically. User posts
// and ingestion/system entries may be roots; LLM replies and follow-up
// questions must attach to an existing entry through CreateReply.
//
// Each non-nil job in jobs is enqueued in the same transaction with
// SourceEntryID set to the new entry; a nil element is skipped, so
// callers can pass a conditionally-nil job without filtering it out
// themselves. All other job fields remain under the caller's control
// because concrete job types belong to later worker issues.
func (s *Service) CreateRoot(ctx context.Context, kind domain.EntryKind, body string, jobs ...*domain.Job) (domain.Entry, error) {
	actorType, ok := authorActorTypeForKind[kind]
	if !ok || kind == domain.EntryLLMReply || kind == domain.EntryLLMFollowUp {
		return domain.Entry{}, ErrInvalidKind
	}

	now := s.clock.Now().UTC()
	id := domain.NewID()
	entry := domain.Entry{
		ID:               id,
		ThreadID:         id,
		Kind:             kind,
		Body:             body,
		ProcessingStatus: domain.ProcessingNone,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		actor, err := repos.Actors.GetByType(ctx, actorType)
		if err != nil {
			return fmt.Errorf("resolve root author: %w", err)
		}
		entry.AuthorActorID = actor.ID

		if err := repos.Threads.Create(ctx, domain.Thread{ID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := repos.Entries.Create(ctx, entry); err != nil {
			return err
		}
		return enqueueForEntry(ctx, repos, jobs, entry.ID)
	})
	if err != nil {
		return domain.Entry{}, err
	}
	return entry, nil
}

// CreateReply creates a direct child of parentEntryID. There is
// deliberately no threadID argument: the reply always inherits its
// parent's ThreadID, making cross-thread parent relationships
// impossible through this use-case API.
func (s *Service) CreateReply(ctx context.Context, parentEntryID string, kind domain.EntryKind, body string, jobs ...*domain.Job) (domain.Entry, error) {
	now := s.clock.Now().UTC()
	var entry domain.Entry

	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		var err error
		entry, err = createReplyEntry(ctx, repos, parentEntryID, kind, body, now)
		if err != nil {
			return err
		}
		return enqueueForEntry(ctx, repos, jobs, entry.ID)
	})
	if err != nil {
		return domain.Entry{}, err
	}
	return entry, nil
}

// CreateGeneratedReply atomically creates a new llm_reply/llm_follow_up
// entry replying to targetEntryID and marks generationID's pending
// LLMGeneration complete, linked to the new entry, in one transaction —
// the same atomicity AGENTS.md's "Commit the post and durable job intent
// atomically" requires of CreateReply's job intent, applied here to the
// generation record a retried job must not be able to duplicate. Callers
// (internal/llmreply's job handler) call this only after a provider call
// has already succeeded; kind must be EntryLLMReply or
// EntryLLMFollowUp, and generationID must currently be pending or this
// returns domain.ErrConflict (see LLMGenerationRepository.Complete).
func (s *Service) CreateGeneratedReply(ctx context.Context, targetEntryID string, kind domain.EntryKind, body, generationID string, promptTokens, completionTokens *int) (domain.Entry, error) {
	if kind != domain.EntryLLMReply && kind != domain.EntryLLMFollowUp {
		return domain.Entry{}, ErrInvalidKind
	}

	now := s.clock.Now().UTC()
	var entry domain.Entry

	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		var err error
		entry, err = createReplyEntry(ctx, repos, targetEntryID, kind, body, now)
		if err != nil {
			return err
		}
		return repos.Generations.Complete(ctx, generationID, entry.ID, body, promptTokens, completionTokens, now)
	})
	if err != nil {
		return domain.Entry{}, err
	}
	return entry, nil
}

// createReplyEntry builds and persists one reply entry (Entries.Create,
// Threads.Touch) against repos, which the caller must already be running
// inside a UnitOfWork.WithinTx transaction. CreateReply and
// CreateGeneratedReply share this helper so each can commit its own
// additional transactional write (a durable job intent, or
// Generations.Complete) atomically alongside the entry without
// duplicating the entry-creation logic itself.
func createReplyEntry(ctx context.Context, repos domain.Repos, parentEntryID string, kind domain.EntryKind, body string, now time.Time) (domain.Entry, error) {
	actorType, ok := authorActorTypeForKind[kind]
	if !ok {
		return domain.Entry{}, ErrInvalidKind
	}

	entry := domain.Entry{
		ID:               domain.NewID(),
		ParentEntryID:    &parentEntryID,
		Kind:             kind,
		Body:             body,
		ProcessingStatus: domain.ProcessingNone,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	parent, err := repos.Entries.Get(ctx, parentEntryID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Entry{}, ErrParentNotFound
	}
	if err != nil {
		return domain.Entry{}, fmt.Errorf("get parent entry: %w", err)
	}
	entry.ThreadID = parent.ThreadID

	actor, err := repos.Actors.GetByType(ctx, actorType)
	if err != nil {
		return domain.Entry{}, fmt.Errorf("resolve reply author: %w", err)
	}
	entry.AuthorActorID = actor.ID

	if err := repos.Entries.Create(ctx, entry); err != nil {
		return domain.Entry{}, err
	}
	if err := repos.Threads.Touch(ctx, entry.ThreadID, now); err != nil {
		return domain.Entry{}, err
	}
	return entry, nil
}

// EditPost replaces the body of the owner actor's own user_post and,
// when supplied, atomically enqueues the caller-defined reprocessing
// job(s). Generated and ingested entries can never reach UpdateBody
// through this use-case path.
func (s *Service) EditPost(ctx context.Context, entryID, editorActorID, newBody string, jobs ...*domain.Job) (domain.Entry, error) {
	now := s.clock.Now().UTC()
	var entry domain.Entry

	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		var err error
		entry, err = repos.Entries.Get(ctx, entryID)
		if err != nil {
			return err
		}
		if entry.Kind != domain.EntryUserPost {
			return ErrNotEditable
		}

		owner, err := repos.Actors.GetByType(ctx, domain.ActorOwner)
		if err != nil {
			return fmt.Errorf("resolve editor actor: %w", err)
		}
		if editorActorID != owner.ID || entry.AuthorActorID != editorActorID {
			return ErrNotEditable
		}

		if err := repos.Entries.UpdateBody(ctx, entry.ID, newBody, now); err != nil {
			return err
		}
		if err := enqueueForEntry(ctx, repos, jobs, entry.ID); err != nil {
			return err
		}

		entry.Body = newBody
		entry.UpdatedAt = now
		return nil
	})
	if err != nil {
		return domain.Entry{}, err
	}
	return entry, nil
}

// SetArchived toggles an entry's archived state at the service clock's
// current time.
func (s *Service) SetArchived(ctx context.Context, entryID string, archived bool) error {
	return s.repos.Entries.SetArchived(ctx, entryID, archived, s.clock.Now().UTC())
}

// SetHidden toggles an entry's hidden state at the service clock's
// current time.
func (s *Service) SetHidden(ctx context.Context, entryID string, hidden bool) error {
	return s.repos.Entries.SetHidden(ctx, entryID, hidden, s.clock.Now().UTC())
}

// GetTimeline returns the stable (created_at, id)-ordered timeline. By
// default callers pass includeHidden=false to omit both archived and
// hidden entries.
func (s *Service) GetTimeline(ctx context.Context, page domain.Page, includeHidden bool) ([]domain.Entry, error) {
	return s.repos.Entries.ListTimeline(ctx, page, includeHidden)
}

// GetTimelineDesc returns the newest-first timeline page: up to limit
// entries, the most recent page when before is nil or the entries
// strictly older than before otherwise. Issue #7's home timeline uses
// this instead of GetTimeline; see EntryRepository.ListTimelineDesc.
func (s *Service) GetTimelineDesc(ctx context.Context, before *domain.Cursor, limit int, includeHidden bool) ([]domain.Entry, error) {
	return s.repos.Entries.ListTimelineDesc(ctx, before, limit, includeHidden)
}

// GetThread returns the full oldest-first conversation for threadID,
// including archived and hidden entries.
func (s *Service) GetThread(ctx context.Context, threadID string) ([]domain.Entry, error) {
	return s.repos.Entries.ListByThread(ctx, threadID)
}

// GetChildren returns only parentEntryID's direct children in
// deterministic (created_at, id) order, not deeper descendants.
func (s *Service) GetChildren(ctx context.Context, parentEntryID string) ([]domain.Entry, error) {
	return s.repos.Entries.ListChildren(ctx, parentEntryID)
}

// GetEntry returns one entry by ID, including archived/hidden entries.
// Callers that must enforce visibility (Issue #7's notes/show and its
// siblings) do so themselves rather than this general-purpose lookup
// silently hiding rows.
func (s *Service) GetEntry(ctx context.Context, id string) (domain.Entry, error) {
	return s.repos.Entries.Get(ctx, id)
}

// CountByAuthor returns actorID's total entry count, including archived
// and hidden entries (see EntryRepository.CountByAuthor).
func (s *Service) CountByAuthor(ctx context.Context, actorID string) (int, error) {
	return s.repos.Entries.CountByAuthor(ctx, actorID)
}

// ResolveAuthor returns the Actor an entry's AuthorActorID names, so
// callers projecting an Entry onto a Misskey-compatible wire type (Note.
// user) can determine whether it is the owner or one of the reserved
// assistant/system presentation actors without depending on
// internal/domain/storage directly. It never returns the owner's real
// profile (username, display name): that lives in miauth.Service's
// configuration, not here.
func (s *Service) ResolveAuthor(ctx context.Context, actorID string) (domain.Actor, error) {
	return s.repos.Actors.Get(ctx, actorID)
}

// enqueueForEntry enqueues each non-nil job in jobs against entryID, in
// the same transaction the caller is already running inside. Since jobs
// is variadic, a caller passing a single possibly-nil job (as every
// pre-Issue-#10 call site does) produces a one-element slice containing
// nil, not a nil slice — so nil elements are skipped individually here,
// not filtered by checking the slice itself.
func enqueueForEntry(ctx context.Context, repos domain.Repos, jobs []*domain.Job, entryID string) error {
	for _, job := range jobs {
		if job == nil {
			continue
		}
		intent := *job
		intent.SourceEntryID = &entryID
		if err := repos.Jobs.Enqueue(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}
