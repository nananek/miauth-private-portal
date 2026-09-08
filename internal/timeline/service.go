// Package timeline implements the post, reply, edit, visibility, and
// stable timeline use cases. It keeps entry/thread invariants above the
// persistence layer and depends only on internal/domain, never on HTTP,
// SQLite, or an LLM provider.
package timeline

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	// ErrAuthorNotEligible reports that CreateGeneratedReplyBy's
	// caller-supplied authorActorID may not author a generated reply:
	// it is neither the assistant actor nor an active Open WebUI model
	// actor whose workspace is enabled (Issue #52). Unlike
	// ErrInvalidKind, this is about who is asking, not what kind of
	// entry was requested.
	ErrAuthorNotEligible = errors.New("timeline: actor is not eligible to author a generated reply")
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
	// OwnerUsername enables Issue #23 PR5's self-mention detection: a
	// user_post whose Body contains "@" + OwnerUsername, word-bounded
	// (not a plain substring match, so a username that is itself a
	// prefix of a longer @-handle in the body never false-positives —
	// see recordSelfMentionIfAny), records a domain.Mention pointing
	// back at its own author. Detection never runs on llm_reply/
	// llm_follow_up/news/mail entries, and never backfills entries
	// created before this Config took effect (docs/compat/
	// aria-v1.5.11.md's "POST /api/notes/mentions" section;
	// owner-confirmed scope, 2026-09-06). An empty value (the zero
	// Config, matching every timeline.NewService caller before PR5)
	// disables detection entirely rather than matching every post.
	OwnerUsername string
	// Broadcaster announces every newly created entry for Issue #95
	// PR2's live push delivery (internal/streamhub.Hub satisfies this).
	// nil (the zero Config, matching every timeline.NewService caller
	// before this field existed) disables it entirely — the same
	// nil-means-unchanged convention Clock/OwnerUsername already use.
	// This package never imports internal/streamhub directly (AGENTS.md:
	// "Domain/use-case code must not depend on HTTP handlers" — the same
	// layering rule applies symmetrically to the neutral broker package
	// both this package and internal/httpserver depend on); only this
	// narrow EntryBroadcaster interface crosses the boundary.
	Broadcaster EntryBroadcaster
}

// EntryBroadcaster is the narrow interface Service uses to announce a
// just-committed entry for live push delivery. Its single method's name
// and signature match internal/streamhub.Hub's Publish exactly, so a
// *streamhub.Hub satisfies this interface structurally with no adapter
// — cmd/server/main.go passes one straight into Config.Broadcaster.
// Publish must never block or fail from the caller's point of view:
// Service calls it only after the entry's own transaction has already
// committed, as a best-effort side effect, mirroring AGENTS.md's "local
// post success must not depend on provider success" applied to push
// delivery instead of an LLM/external provider.
type EntryBroadcaster interface {
	Publish(entry domain.Entry)
}

// Service enforces timeline business rules while composing domain
// repositories through atomic units of work.
type Service struct {
	uow   domain.UnitOfWork
	repos domain.Repos
	clock Clock
	// mentionPattern is nil when Config.OwnerUsername is unset,
	// disabling self-mention detection entirely.
	mentionPattern *regexp.Regexp
	// broadcaster is nil when Config.Broadcaster is unset, disabling
	// live push delivery entirely (see broadcastCreated).
	broadcaster EntryBroadcaster
}

// NewService builds a timeline Service. uow and repos commonly come
// from one storage adapter, but no concrete adapter type crosses this
// package boundary.
func NewService(uow domain.UnitOfWork, repos domain.Repos, cfg Config) *Service {
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	svc := &Service{uow: uow, repos: repos, clock: clock, broadcaster: cfg.Broadcaster}
	if cfg.OwnerUsername != "" {
		// Word-bounded: the character immediately before "@" (if any)
		// and immediately after the username (if any) must not be part
		// of Misskey's username charset ([A-Za-z0-9_], see
		// internal/config's ownerUsernamePattern). This keeps a longer
		// @-handle that merely starts with the owner's username (e.g.
		// "@ownersecondary") from false-positiving, and incidentally
		// keeps an embedded email-like token ("name@owner.example")
		// from matching either, since the character right before "@"
		// there is alphanumeric.
		svc.mentionPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_])@` + regexp.QuoteMeta(cfg.OwnerUsername) + `([^A-Za-z0-9_]|$)`)
	}
	return svc
}

// recordSelfMentionIfAny persists a domain.Mention row when e is a
// user_post whose Body contains an @-mention of the owner's own
// username (Issue #23 PR5's real-detection option). single-owner means
// the mentioned actor is always e's own author — there is no other
// login-capable local actor a post could mention — so this never needs
// a separate actor lookup. It is a no-op when e.Kind is not
// EntryUserPost or mention detection is disabled
// (Config.OwnerUsername unset). It is called unconditionally from
// every entry-creation path below (CreateRoot, CreateReply,
// CreateGeneratedReply, CreateExternalEntry) so a future scope change
// needs only touch this one function, not each call site (see
// plan-issue-23's PR5 notes); every kind but EntryUserPost's own
// creation paths currently no-op here immediately on the Kind check.
// Callers must run this inside the same transaction as the
// Entries.Create it follows.
func (s *Service) recordSelfMentionIfAny(ctx context.Context, repos domain.Repos, e domain.Entry) error {
	if e.Kind != domain.EntryUserPost || s.mentionPattern == nil || !s.mentionPattern.MatchString(e.Body) {
		return nil
	}
	return repos.Mentions.Create(ctx, domain.Mention{
		ID:               domain.NewID(),
		EntryID:          e.ID,
		MentionedActorID: e.AuthorActorID,
		CreatedAt:        e.CreatedAt,
	})
}

// recordNotification persists an Issue #23 PR6 notification of notifType
// for relatedEntryID at createdAt. Callers must run this inside the same
// transaction as the entry create it follows, so a notification never
// outlives (or is missing for) the entry it names. Unlike
// recordSelfMentionIfAny, there is no disabling condition or kind check
// here: every call site below already restricts itself to exactly the
// two creation paths plan-issue-23's PR6 section designates (a generated
// reply/follow-up, or a genuinely new external item), so this is an
// unconditional insert.
func recordNotification(ctx context.Context, repos domain.Repos, notifType domain.NotificationType, relatedEntryID string, createdAt time.Time) error {
	return repos.Notifications.Create(ctx, domain.Notification{
		ID:             domain.NewID(),
		Type:           notifType,
		RelatedEntryID: relatedEntryID,
		CreatedAt:      createdAt,
	})
}

// broadcastCreated announces entry's creation for Issue #95 PR2's live
// push delivery, if a Broadcaster is configured (see Config.Broadcaster
// and EntryBroadcaster). Callers must invoke this only after entry's own
// transaction has already committed successfully — never from inside
// WithinTx — since a push is a confirmed-committed side effect, unlike
// an EntryHook, which still runs as part of the transaction it might
// roll back.
func (s *Service) broadcastCreated(entry domain.Entry) {
	if s.broadcaster == nil {
		return
	}
	s.broadcaster.Publish(entry)
}

// EntryHook is a caller-supplied extension point invoked inside the same
// transaction as a just-created entry, after its self-mention and
// enqueueForEntry writes. It exists for Issue #53's Open WebUI bridge:
// claiming a conversation link, recording a turn, and enqueueing the
// "openwebui_turn" job all need to commit atomically alongside the owner's
// post, exactly like a durable job intent does, but their shape (a link
// claim keyed by branch, a turn row, one job) does not fit the plain
// jobs-to-enqueue list CreateRoot/CreateReply already take. A hook's error
// rolls back the whole transaction — including the entry itself — so it
// must only ever report a bug in the hook's own bookkeeping, never that a
// remote provider is unreachable: no network call happens inside a hook
// (AGENTS.md/roadmap: local post success must not depend on provider
// success).
type EntryHook func(ctx context.Context, repos domain.Repos, entry domain.Entry) error

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
	return s.CreateRootWithHook(ctx, kind, body, nil, jobs...)
}

// CreateRootWithHook is CreateRoot with an additional EntryHook run inside
// the same transaction, after the entry, its self-mention record, and its
// jobs are all written. hook may be nil, in which case this is exactly
// CreateRoot's behavior.
func (s *Service) CreateRootWithHook(ctx context.Context, kind domain.EntryKind, body string, hook EntryHook, jobs ...*domain.Job) (domain.Entry, error) {
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
		if err := s.recordSelfMentionIfAny(ctx, repos, entry); err != nil {
			return err
		}
		if err := enqueueForEntry(ctx, repos, jobs, entry.ID); err != nil {
			return err
		}
		if hook == nil {
			return nil
		}
		return hook(ctx, repos, entry)
	})
	if err != nil {
		return domain.Entry{}, err
	}
	s.broadcastCreated(entry)
	return entry, nil
}

// CreateReply creates a direct child of parentEntryID. There is
// deliberately no threadID argument: the reply always inherits its
// parent's ThreadID, making cross-thread parent relationships
// impossible through this use-case API.
func (s *Service) CreateReply(ctx context.Context, parentEntryID string, kind domain.EntryKind, body string, jobs ...*domain.Job) (domain.Entry, error) {
	return s.CreateReplyWithHook(ctx, parentEntryID, kind, body, nil, jobs...)
}

// CreateReplyWithHook is CreateReply with an additional EntryHook run
// inside the same transaction, after the entry, its self-mention record,
// and its jobs are all written. hook may be nil, in which case this is
// exactly CreateReply's behavior.
func (s *Service) CreateReplyWithHook(ctx context.Context, parentEntryID string, kind domain.EntryKind, body string, hook EntryHook, jobs ...*domain.Job) (domain.Entry, error) {
	now := s.clock.Now().UTC()
	var entry domain.Entry

	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		var err error
		entry, err = createReplyEntry(ctx, repos, parentEntryID, kind, body, now)
		if err != nil {
			return err
		}
		if err := s.recordSelfMentionIfAny(ctx, repos, entry); err != nil {
			return err
		}
		if err := enqueueForEntry(ctx, repos, jobs, entry.ID); err != nil {
			return err
		}
		if hook == nil {
			return nil
		}
		return hook(ctx, repos, entry)
	})
	if err != nil {
		return domain.Entry{}, err
	}
	s.broadcastCreated(entry)
	return entry, nil
}

// CreateGeneratedReply atomically creates a new llm_reply/llm_follow_up
// entry replying to targetEntryID, records an Issue #23 PR6 "reply"
// notification for it, and marks generationID's pending LLMGeneration
// complete, linked to the new entry, in one transaction — the same
// atomicity AGENTS.md's "Commit the post and durable job intent
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
		if err := s.recordSelfMentionIfAny(ctx, repos, entry); err != nil {
			return err
		}
		if err := recordNotification(ctx, repos, domain.NotificationReply, entry.ID, now); err != nil {
			return err
		}
		return repos.Generations.Complete(ctx, generationID, entry.ID, body, promptTokens, completionTokens, now)
	})
	if err != nil {
		return domain.Entry{}, err
	}
	s.broadcastCreated(entry)
	return entry, nil
}

// CreateGeneratedReplyBy atomically creates a new llm_reply entry
// replying to targetEntryID, authored by authorActorID rather than the
// fixed assistant actor CreateGeneratedReply always resolves. It exists
// for Issue #52/#53's Open WebUI bridge, whose completions are authored
// by a VirtualActor (a per-model presentation actor, not the shared
// assistant) and whose own turn-link bookkeeping — not an
// internal/domain.LLMGeneration row — is what needs completing
// atomically alongside the entry; complete is that hook, invoked inside
// the same transaction as the entry's own creation and thread touch, and
// may be nil.
//
// authorActorID must resolve to the assistant actor or to an Open WebUI
// model actor (domain.ActorOpenWebUIModel) whose model is active and
// whose workspace is enabled; anything else, including the owner or the
// system actor, is ErrAuthorNotEligible. The three VirtualActor gates
// mirror internal/openwebui.Registry.ResolveVirtualActor's — this
// package does not import that one (AGENTS.md: use-case packages stay
// narrow), so the check is repeated here against the same domain
// repositories rather than shared.
//
// Unlike CreateGeneratedReply, this method records no self-mention (a
// generated reply is never the owner's own post) and no Issue #23 PR6
// "reply" notification: whether an Open WebUI-authored reply notifies is
// Issue #53's call to make, not this one's.
func (s *Service) CreateGeneratedReplyBy(
	ctx context.Context, authorActorID, targetEntryID, body string,
	complete func(ctx context.Context, repos domain.Repos, entry domain.Entry) error,
) (domain.Entry, error) {
	now := s.clock.Now().UTC()
	var entry domain.Entry

	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		author, err := repos.Actors.Get(ctx, authorActorID)
		if err != nil {
			return fmt.Errorf("resolve reply author: %w", err)
		}
		if err := checkGeneratedReplyAuthorEligible(ctx, repos, author); err != nil {
			return err
		}

		parent, err := repos.Entries.Get(ctx, targetEntryID)
		if errors.Is(err, domain.ErrNotFound) {
			return ErrParentNotFound
		}
		if err != nil {
			return fmt.Errorf("get parent entry: %w", err)
		}

		entry = domain.Entry{
			ID:               domain.NewID(),
			ThreadID:         parent.ThreadID,
			ParentEntryID:    &targetEntryID,
			Kind:             domain.EntryLLMReply,
			AuthorActorID:    author.ID,
			Body:             body,
			ProcessingStatus: domain.ProcessingNone,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := repos.Entries.Create(ctx, entry); err != nil {
			return err
		}
		if err := repos.Threads.Touch(ctx, entry.ThreadID, now); err != nil {
			return err
		}
		if complete == nil {
			return nil
		}
		return complete(ctx, repos, entry)
	})
	if err != nil {
		return domain.Entry{}, err
	}
	s.broadcastCreated(entry)
	return entry, nil
}

// checkGeneratedReplyAuthorEligible enforces CreateGeneratedReplyBy's
// author eligibility rule. The assistant actor is always eligible (the
// same actor CreateGeneratedReply itself resolves); an Open WebUI model
// actor is eligible only while its model is active and its workspace is
// enabled — the same two gates internal/openwebui.Registry.
// ResolveVirtualActor checks before projecting a VirtualActor, so an
// entry is never authored by an identity the wire layer could not also
// present. Every other actor type, including the owner and the system
// actor, is never eligible.
func checkGeneratedReplyAuthorEligible(ctx context.Context, repos domain.Repos, author domain.Actor) error {
	switch author.Type {
	case domain.ActorAssistant:
		return nil
	case domain.ActorOpenWebUIModel:
		model, err := repos.OpenWebUIModels.GetByActor(ctx, author.ID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return ErrAuthorNotEligible
			}
			return fmt.Errorf("resolve model for reply author: %w", err)
		}
		if !model.Active {
			return ErrAuthorNotEligible
		}
		workspace, err := repos.OpenWebUIWorkspaces.Get(ctx, model.WorkspaceID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return ErrAuthorNotEligible
			}
			return fmt.Errorf("resolve workspace for reply author: %w", err)
		}
		if !workspace.Enabled {
			return ErrAuthorNotEligible
		}
		return nil
	default:
		return ErrAuthorNotEligible
	}
}

// CreateExternalEntry atomically dedupes an ingested item and, only when
// it is new, creates its root EntryNews/EntryMail entry and promotes the
// item to it. item's ID/FetchedAt/CreatedAt are assigned here (the
// caller, internal/ingest's job handler, supplies only SourceID,
// ExternalID, ProvenanceURL, PublishedAt, and DedupeKey); body is the
// entry's Body, kept separate from item because ExternalItem itself
// carries no content, only provenance/dedupe metadata.
//
// The created entry's CreatedAt is derived from item.PublishedAt (the
// source's own publish/receive time) rather than ingest time, so the
// home timeline sorts by when content actually happened instead of by
// poll/batch order. It falls back to ingest time when PublishedAt is nil
// (unparseable or absent upstream) or in the future (a bad feed
// timestamp should not vault an entry to the top as if it just
// happened). item.CreatedAt/FetchedAt still record actual ingest time.
//
// Returns the existing, already-promoted entry and created=false when
// item.DedupeKey already exists — the same duplicate-delivery safety net
// CreateGeneratedReply gives Issue #9's job handler, applied here to
// ingestion so a retried "external_source_poll" job attempt (or the same
// item reappearing in a later fetch) never creates a second entry for
// the same external item.
func (s *Service) CreateExternalEntry(ctx context.Context, kind domain.EntryKind, item domain.ExternalItem, body string) (domain.Entry, bool, error) {
	actorType, ok := authorActorTypeForKind[kind]
	if !ok || kind == domain.EntryLLMReply || kind == domain.EntryLLMFollowUp {
		return domain.Entry{}, false, ErrInvalidKind
	}

	now := s.clock.Now().UTC()
	createdAt := now
	if item.PublishedAt != nil {
		createdAt = item.PublishedAt.UTC()
		if createdAt.After(now) {
			// Feed-side timestamp is in the future (bad clock, flying
			// publish date): fall back to ingest time rather than let
			// the entry jump to the top of the timeline as if it just
			// happened.
			createdAt = now
		}
	}
	id := domain.NewID()
	entry := domain.Entry{
		ID:               id,
		ThreadID:         id,
		Kind:             kind,
		Body:             body,
		ProcessingStatus: domain.ProcessingNone,
		ProvenanceURL:    item.ProvenanceURL,
		CreatedAt:        createdAt,
		UpdatedAt:        now,
	}
	item.ID = domain.NewID()
	item.FetchedAt = now
	item.CreatedAt = now

	created := false
	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		createErr := repos.ExternalItems.Create(ctx, item)
		if createErr == nil {
			created = true
			actorID, err := s.resolveExternalEntryAuthor(ctx, repos, item.SourceID, actorType)
			if err != nil {
				return fmt.Errorf("resolve external entry author: %w", err)
			}
			entry.AuthorActorID = actorID

			if err := repos.Threads.Create(ctx, domain.Thread{ID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := repos.Entries.Create(ctx, entry); err != nil {
				return err
			}
			if err := s.recordSelfMentionIfAny(ctx, repos, entry); err != nil {
				return err
			}
			if err := recordNotification(ctx, repos, domain.NotificationApp, entry.ID, now); err != nil {
				return err
			}
			return repos.ExternalItems.Promote(ctx, item.ID, entry.ID)
		}
		if !errors.Is(createErr, domain.ErrConflict) {
			return createErr
		}

		// Lost a race, or this is a genuine re-delivery of an
		// already-ingested item: find the item this dedupe key actually
		// belongs to and return its already-promoted entry instead.
		existing, getErr := repos.ExternalItems.GetByDedupeKey(ctx, item.DedupeKey)
		if getErr != nil {
			return fmt.Errorf("get existing external item after conflict: %w", getErr)
		}
		if existing.EntryID == nil {
			// The item row exists but a prior attempt crashed between
			// Create and Promote (or lost a concurrent promote race).
			// There is no entry yet to report as this delivery's
			// result, and creating a second one would violate the
			// dedupe key's own uniqueness once that promote finally
			// lands, so surface this as a retryable error instead of
			// silently reporting created=false with a zero entry.
			return fmt.Errorf("external item %s exists but is not yet promoted to an entry", existing.ID)
		}
		existingEntry, err := repos.Entries.Get(ctx, *existing.EntryID)
		if err != nil {
			return fmt.Errorf("get promoted entry: %w", err)
		}
		entry = existingEntry
		created = false
		return nil
	})
	if err != nil {
		return domain.Entry{}, false, err
	}
	if created {
		// Not for the created==false dedupe-replay branch above: a
		// redelivered/raced duplicate must not push the same entry to
		// live connections a second time (plan-issue-95 §3.3).
		s.broadcastCreated(entry)
	}
	return entry, created, nil
}

// resolveExternalEntryAuthor returns the actor ID a news/mail entry from
// sourceID should be authored by: that source's own dedicated
// ActorExternalSource actor (Issue #77 PR4/ADR-0008's design-A host
// display) when it has one, otherwise the shared singleton actor
// fallbackType names — the same authorActorTypeForKind[kind] lookup
// every external entry used before PR4. A source's ActorID is nil for
// every imap-kind source (ADR-0008's design-A host display is RSS-only)
// and for an rss-kind source pre-dating PR4's migration, so this
// fallback is not merely defensive: it is the real, ongoing behavior
// for every kind that does not get its own actor.
func (s *Service) resolveExternalEntryAuthor(ctx context.Context, repos domain.Repos, sourceID string, fallbackType domain.ActorType) (string, error) {
	source, err := repos.ExternalSources.Get(ctx, sourceID)
	if err != nil {
		return "", fmt.Errorf("get external source: %w", err)
	}
	if source.ActorID != nil {
		return *source.ActorID, nil
	}
	actor, err := repos.Actors.GetByType(ctx, fallbackType)
	if err != nil {
		return "", err
	}
	return actor.ID, nil
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

// CountByAuthor returns actorID's entry count, excluding archived and
// hidden entries (see EntryRepository.CountByAuthor).
func (s *Service) CountByAuthor(ctx context.Context, actorID string) (int, error) {
	return s.repos.Entries.CountByAuthor(ctx, actorID)
}

// CountAll returns the total entry count across every author, including
// archived and hidden entries (see EntryRepository.CountAll). It backs
// POST /api/stats' server-wide notesCount (Issue #23 PR2).
func (s *Service) CountAll(ctx context.Context) (int, error) {
	return s.repos.Entries.CountAll(ctx)
}

// SetReaction creates or replaces reactorActorID's reaction to entryID
// with emoji (Issue #23 PR4). Callers are responsible for restricting
// entryID to a visible entry and emoji to a plain Unicode emoji (the
// httpserver layer does both, mirroring how SetHidden/SetArchived above
// leave visibility/ownership checks to their own callers). See
// domain.ReactionRepository.Create's doc comment for why a second call
// for the same (entry, actor) pair overwrites rather than conflicts.
func (s *Service) SetReaction(ctx context.Context, entryID, reactorActorID, emoji string) error {
	return s.repos.Reactions.Create(ctx, domain.Reaction{
		ID:             domain.NewID(),
		EntryID:        entryID,
		ReactorActorID: reactorActorID,
		Emoji:          emoji,
		CreatedAt:      s.clock.Now().UTC(),
	})
}

// RemoveReaction removes reactorActorID's reaction to entryID, if any
// (Issue #23 PR4). It is idempotent: removing an absent reaction is not
// an error (see domain.ReactionRepository.Delete's doc comment).
func (s *Service) RemoveReaction(ctx context.Context, entryID, reactorActorID string) error {
	return s.repos.Reactions.Delete(ctx, entryID, reactorActorID)
}

// ReactionCounts returns entryID's reaction counts keyed by emoji (Issue
// #23 PR4), backing Note.reactions.
func (s *Service) ReactionCounts(ctx context.Context, entryID string) (map[string]int, error) {
	return s.repos.Reactions.CountsByEmoji(ctx, entryID)
}

// MyReaction returns viewerActorID's own reaction emoji to entryID, or
// nil if they have not reacted (Issue #23 PR4), backing Note.myReaction.
func (s *Service) MyReaction(ctx context.Context, entryID, viewerActorID string) (*string, error) {
	r, err := s.repos.Reactions.GetByActor(ctx, entryID, viewerActorID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r.Emoji, nil
}

// CountAllReactions returns the total number of reactions across every
// entry (Issue #23 PR4), backing POST /api/stats' reactionsCount.
func (s *Service) CountAllReactions(ctx context.Context) (int, error) {
	return s.repos.Reactions.CountAll(ctx)
}

// ListReactions returns entryID's reactions, newest-first, optionally
// filtered to one emoji (Issue #23 PR4's "who reacted" sheet). See
// domain.ReactionRepository.ListByEntry's doc comment for before/limit.
func (s *Service) ListReactions(ctx context.Context, entryID string, emoji *string, before *domain.Cursor, limit int) ([]domain.Reaction, error) {
	return s.repos.Reactions.ListByEntry(ctx, entryID, emoji, before, limit)
}

// GetReaction returns one reaction by its opaque ID (Issue #23 PR4),
// used only to resolve ListReactions' untilId pagination anchor.
func (s *Service) GetReaction(ctx context.Context, id string) (domain.Reaction, error) {
	return s.repos.Reactions.Get(ctx, id)
}

// ListMentions returns actorID's newest-first, archived/hidden-excluded
// self-mentions (Issue #23 PR5's POST /api/notes/mentions) — the same
// paging contract as GetTimelineDesc: before nil returns the most
// recent page, otherwise entries strictly older than before.
func (s *Service) ListMentions(ctx context.Context, actorID string, before *domain.Cursor, limit int) ([]domain.Entry, error) {
	return s.repos.Mentions.ListEntriesByMentionedActor(ctx, actorID, before, limit)
}

// ListNotifications returns newest-first, paginated notifications (Issue
// #23 PR6's POST /api/i/notifications) — the same paging contract as
// GetTimelineDesc/ListMentions: before nil returns the most recent page,
// otherwise notifications strictly older than before.
func (s *Service) ListNotifications(ctx context.Context, before *domain.Cursor, limit int) ([]domain.Notification, error) {
	return s.repos.Notifications.ListDesc(ctx, before, limit)
}

// GetNotification returns one notification by its opaque ID (Issue #23
// PR6), used only to resolve ListNotifications' untilId pagination
// anchor.
func (s *Service) GetNotification(ctx context.Context, id string) (domain.Notification, error) {
	return s.repos.Notifications.Get(ctx, id)
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

// GetActorByType returns the singleton actor of actorType (owner,
// assistant, system) — the same repos.Actors.GetByType lookup
// ResolveAuthor makes for one already-known actor ID, but keyed by type
// instead, for a caller enumerating the known actor set rather than
// resolving one entry's author (Issue #65's user search).
func (s *Service) GetActorByType(ctx context.Context, actorType domain.ActorType) (domain.Actor, error) {
	return s.repos.Actors.GetByType(ctx, actorType)
}

// ListActorsByType returns every actor of actorType, in
// ActorRepository.ListByType's own stable order. Issue #65's user search
// uses it to enumerate every Open WebUI model actor
// (domain.ActorOpenWebUIModel), the one non-singleton actor type.
func (s *Service) ListActorsByType(ctx context.Context, actorType domain.ActorType) ([]domain.Actor, error) {
	return s.repos.Actors.ListByType(ctx, actorType)
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
