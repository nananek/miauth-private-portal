// This file is Issue #53's (OWUI-B) owner recovery use case (plan
// §5.5): the five Registry methods cmd/openwebuictl (PR4) drives to
// inspect and resolve the conversation links Issue #53's durable job
// left in a state it may never advance automatically —
// LinkAmbiguous (an uncertain remote outcome) needs a person to say
// what actually happened, and LinkCreationPending can outlive its claim
// job (a crash, a killed process) without ever reaching a terminal
// state on its own.
//
// Every method goes through ownerOnly or requireOwner (ConfirmLink's
// own two-phase version of it — see requireOwner's doc comment), the
// same guard every other owner-only Registry method already uses:
// there is still no HTTP path to any of this (the roadmap's "Manual
// resolution ... is an explicit owner/operator action" keeps it CLI-
// only), but the guard has to already be in place for whenever one is
// built, exactly as registry.go's own doc comment says.
package openwebui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

var (
	// ErrRecoveryNeedsChatID reports that ConfirmLink cannot proceed
	// because neither the link nor the caller knows the remote chat id.
	// Recovery never lists the provider's chats to find one — that is
	// exactly the read ADR-0005 D1's addendum excludes, which is why a
	// lost chat-creation response stays ambiguous in the first place —
	// so a creation loss with no owner-supplied id can only be resolved
	// by AbandonLink.
	ErrRecoveryNeedsChatID = errors.New("openwebui: recovery needs an owner-supplied remote chat id")
	// ErrStillAmbiguous reports that ConfirmLink's provider lookup did
	// not resolve the turn's outcome (found, but not yet done, and not
	// erroring either): nothing was changed. The caller may try again
	// later, once the generation has had time to finish, or fall back to
	// AbandonLink.
	ErrStillAmbiguous = errors.New("openwebui: provider lookup did not resolve the turn's outcome")
	// ErrNoRecoverableTurn reports that an ambiguous link has no
	// non-tombstoned pending or ambiguous turn for ConfirmLink to act on
	// — a state the durable job's own MarkAmbiguous call should never
	// produce (it always freezes the link and its own turn together),
	// so this is defensive fail-closed handling for data this method
	// cannot otherwise make sense of, not an expected outcome.
	ErrNoRecoverableTurn = errors.New("openwebui: link has no recoverable turn")
	// ErrClaimJobStillActive reports that FreezeLink's target link's
	// claim job has not reached a terminal state: it is still pending or
	// running, so its StartChat may yet confirm the chat on its own.
	// Freezing the link now could race that in-flight call.
	ErrClaimJobStillActive = errors.New("openwebui: link's claim job is still pending or running")
)

// ListLinks returns links matching filter for owner-facing recovery
// tooling, most recently created first (OpenWebUIConversationLinkRepository
// .List's own order).
func (r *Registry) ListLinks(ctx context.Context, actorID string, filter domain.OpenWebUILinkFilter) ([]domain.OpenWebUIConversationLink, error) {
	var links []domain.OpenWebUIConversationLink
	err := r.ownerOnly(ctx, actorID, func(ctx context.Context, repos domain.Repos, _ domain.OpenWebUIWorkspace, _ time.Time) error {
		var err error
		links, err = repos.OpenWebUILinks.List(ctx, filter)
		return err
	})
	return links, err
}

// DescribeLink returns one link and every turn recorded against it, in
// the same (created_at, id) order ConfirmLink and AbandonLink read them
// in.
func (r *Registry) DescribeLink(ctx context.Context, actorID, linkID string) (domain.OpenWebUIConversationLink, []domain.OpenWebUITurnLink, error) {
	var link domain.OpenWebUIConversationLink
	var turns []domain.OpenWebUITurnLink
	err := r.ownerOnly(ctx, actorID, func(ctx context.Context, repos domain.Repos, _ domain.OpenWebUIWorkspace, _ time.Time) error {
		var err error
		link, err = repos.OpenWebUILinks.Get(ctx, linkID)
		if err != nil {
			return err
		}
		turns, err = repos.OpenWebUITurnLinks.ListByLink(ctx, linkID)
		return err
	})
	return link, turns, err
}

// ConfirmLink resolves an ambiguous link into a definitive, owner-
// verified outcome (plan §5.5). It never replays a lost chat creation:
// if the target turn never got as far as a client-generated assistant
// message id, there is nothing to look up, and this method only ever
// accepts the chat id the link (or the caller) already asserts. If a
// message id was recorded, it looks up that one turn's outcome —
// through provider, outside any transaction, since holding one open
// across a network call is not this method's call to make — and adopts
// a genuine success (creating the assistant reply exactly the way
// TurnJob.complete would have), records a genuine failure, or, if the
// provider still cannot say, changes nothing and reports
// ErrStillAmbiguous.
//
// remoteChatID is the id the owner identified out-of-band (an Open
// WebUI UI, an operator's own note); nil is fine when the link already
// has one recorded. When both are present they must agree, or this
// returns domain.ErrConflict rather than silently preferring one:
// MarkReady's own compare-and-set enforces the same rule at the write,
// but failing here gives a clearer reason than a bare conflict from a
// write two steps later would.
//
// It returns the recovered assistant entry on a genuine success
// (entry.ID != ""); on every other successful outcome (a definitive
// failure adopted, or the creation-lost branch) it returns a zero
// domain.Entry and a nil error.
func (r *Registry) ConfirmLink(ctx context.Context, actorID, linkID string, remoteChatID *string) (domain.Entry, error) {
	if err := r.requireOwner(ctx, actorID); err != nil {
		return domain.Entry{}, err
	}

	link, err := r.repos.OpenWebUILinks.Get(ctx, linkID)
	if err != nil {
		return domain.Entry{}, fmt.Errorf("openwebui: confirm: get link: %w", err)
	}
	if link.State != domain.LinkAmbiguous {
		return domain.Entry{}, domain.ErrInvalidLinkTransition
	}
	if link.RemoteChatID != nil && remoteChatID != nil && *remoteChatID != *link.RemoteChatID {
		return domain.Entry{}, domain.ErrConflict
	}
	chatID := link.RemoteChatID
	if chatID == nil {
		chatID = remoteChatID
	}
	if chatID == nil {
		return domain.Entry{}, ErrRecoveryNeedsChatID
	}

	turn, ok, err := latestRecoverableTurn(ctx, r.repos, linkID)
	if err != nil {
		return domain.Entry{}, fmt.Errorf("openwebui: confirm: list turns: %w", err)
	}
	if !ok {
		return domain.Entry{}, ErrNoRecoverableTurn
	}

	now := r.clock.Now().UTC()

	if turn.RemoteAssistantMessageID == nil {
		// The creation response itself was lost: there is no
		// client-generated assistant message id to look up, so the
		// owner's (or the link's own) chat id is trusted as-is rather
		// than verified. The provider is never called for this branch.
		if err := r.recordTurnFailedAndLinkReady(ctx, linkID, turn.ID, domain.FailureCategoryCreationLost, *chatID, nil, now); err != nil {
			return domain.Entry{}, fmt.Errorf("openwebui: confirm: %w", err)
		}
		return domain.Entry{}, nil
	}

	outcome, err := r.provider.LookupTurnOutcome(ctx, *chatID, *turn.RemoteAssistantMessageID)
	if err != nil {
		return domain.Entry{}, fmt.Errorf("openwebui: confirm: look up turn outcome: %w", err)
	}

	switch {
	case outcome.Found && outcome.Done && !outcome.HasError && outcome.Content != "":
		entry, err := r.completeRecoveredTurn(ctx, link, turn, *chatID, outcome, now)
		if err != nil {
			return domain.Entry{}, fmt.Errorf("openwebui: confirm: %w", err)
		}
		return entry, nil
	case !outcome.Found || outcome.HasError || (outcome.Done && outcome.Content == ""):
		// Confirmed not to have produced a usable result: the message
		// does not exist on this chat, the provider recorded an error
		// for it, or it is done with nothing in it.
		if err := r.recordTurnFailedAndLinkReady(ctx, linkID, turn.ID, domain.FailureCategoryLost, *chatID, nil, now); err != nil {
			return domain.Entry{}, fmt.Errorf("openwebui: confirm: %w", err)
		}
		return domain.Entry{}, nil
	default: // Found && !Done && !HasError: still generating.
		return domain.Entry{}, ErrStillAmbiguous
	}
}

// completeRecoveredTurn creates the recovered assistant reply the same
// way TurnJob.complete does for a live success — the same writes, in one
// transaction, including the reply notification (ADR-0005 D6: a
// notification only ever follows a genuine success) — so a confirmed
// recovery is indistinguishable, from Aria's point of view, from a turn
// that had simply taken a while.
func (r *Registry) completeRecoveredTurn(
	ctx context.Context, link domain.OpenWebUIConversationLink, turn domain.OpenWebUITurnLink, chatID string, outcome TurnOutcome, now time.Time,
) (domain.Entry, error) {
	model, err := r.repos.OpenWebUIModels.Get(ctx, link.ModelID)
	if err != nil {
		return domain.Entry{}, fmt.Errorf("get model: %w", err)
	}
	entry, err := r.timeline.CreateGeneratedReplyBy(ctx, model.ActorID, turn.LocalMessageID, outcome.Content,
		func(cctx context.Context, repos domain.Repos, assistantEntry domain.Entry) error {
			if err := repos.OpenWebUITurnLinks.SetAssistantEntry(cctx, turn.ID, assistantEntry.ID, now); err != nil {
				return fmt.Errorf("set assistant entry: %w", err)
			}
			if err := repos.OpenWebUITurnLinks.RecordOutcome(cctx, turn.ID, domain.TurnOutcomeRecord{
				Status: domain.TurnSucceeded, PromptTokens: outcome.PromptTokens, CompletionTokens: outcome.CompletionTokens,
			}, now); err != nil {
				return fmt.Errorf("record outcome: %w", err)
			}
			if err := repos.OpenWebUITurnLinks.SetRemoteCorrelation(cctx, turn.ID, domain.OpenWebUITurnCorrelation{
				RemoteChatID: &chatID, RemoteMessageID: turn.RemoteMessageID, RemoteAssistantMessageID: turn.RemoteAssistantMessageID,
				RemoteParentID: turn.RemoteParentID, RemoteCurrentID: outcome.RemoteCurrentID,
			}, now); err != nil {
				return fmt.Errorf("set remote correlation: %w", err)
			}
			if err := repos.OpenWebUILinks.MarkReady(cctx, link.ID, chatID, outcome.RemoteCurrentID, now); err != nil {
				return fmt.Errorf("mark link ready: %w", err)
			}
			return repos.Notifications.Create(cctx, domain.Notification{
				ID: domain.NewID(), Type: domain.NotificationReply, RelatedEntryID: assistantEntry.ID, CreatedAt: now,
			})
		})
	if err != nil {
		return domain.Entry{}, fmt.Errorf("create generated reply: %w", err)
	}
	return entry, nil
}

// recordTurnFailedAndLinkReady is ConfirmLink's shared write for both of
// its non-success outcomes (a lost chat-creation response, and a
// definitively-not-recovered continuation): the turn is recorded failed
// under category, and the link is confirmed ready onto chatID —
// MarkReady's own compare-and-set is what actually enforces that this
// never re-points a link already carrying a different remote_chat_id
// onto a new one. remoteCurrentID is nil in both of ConfirmLink's own
// calls (this branch never learns a new current-message pointer), which
// MarkReady treats as "leave whatever is already recorded alone", not as
// clearing it.
func (r *Registry) recordTurnFailedAndLinkReady(ctx context.Context, linkID, turnID, category, chatID string, remoteCurrentID *string, now time.Time) error {
	return r.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		cat := category
		if err := repos.OpenWebUITurnLinks.RecordOutcome(ctx, turnID, domain.TurnOutcomeRecord{Status: domain.TurnFailed, FailureCategory: &cat}, now); err != nil {
			return fmt.Errorf("record outcome: %w", err)
		}
		if err := repos.OpenWebUILinks.MarkReady(ctx, linkID, chatID, remoteCurrentID, now); err != nil {
			return fmt.Errorf("mark link ready: %w", err)
		}
		return nil
	})
}

// AbandonLink gives up on an ambiguous branch rather than adopting its
// uncertain remote chat: the link is marked dead
// (FailureCategoryOwnerAbandoned), and every non-tombstoned turn still
// pending or ambiguous on it is recorded cancelled and tombstoned. A
// turn that already reached a definitive outcome (succeeded, failed, or
// a prior revision already superseded) is left exactly as it is — this
// only ever concludes turns that were themselves still open.
func (r *Registry) AbandonLink(ctx context.Context, actorID, linkID string) error {
	return r.ownerOnly(ctx, actorID, func(ctx context.Context, repos domain.Repos, _ domain.OpenWebUIWorkspace, now time.Time) error {
		link, err := repos.OpenWebUILinks.Get(ctx, linkID)
		if err != nil {
			return err
		}
		if link.State != domain.LinkAmbiguous {
			return domain.ErrInvalidLinkTransition
		}
		if err := repos.OpenWebUILinks.MarkDead(ctx, linkID, domain.FailureCategoryOwnerAbandoned, now); err != nil {
			return fmt.Errorf("mark link dead: %w", err)
		}
		turns, err := repos.OpenWebUITurnLinks.ListByLink(ctx, linkID)
		if err != nil {
			return fmt.Errorf("list turns: %w", err)
		}
		for _, t := range turns {
			if t.TombstonedAt != nil || !isRecoverableTurnStatus(t.Status) {
				continue
			}
			if err := repos.OpenWebUITurnLinks.RecordOutcome(ctx, t.ID, domain.TurnOutcomeRecord{Status: domain.TurnCancelled}, now); err != nil {
				return fmt.Errorf("cancel turn %s: %w", t.ID, err)
			}
			if err := repos.OpenWebUITurnLinks.Tombstone(ctx, t.ID, now); err != nil {
				return fmt.Errorf("tombstone turn %s: %w", t.ID, err)
			}
		}
		return nil
	})
}

// FreezeLink is the operator-driven version of what an expired job
// lease would otherwise eventually force automatically (the roadmap's
// "expired lease before a definitive result -> ambiguous"): a
// creation_pending link whose claim job has already reached a terminal
// state — dead, failed, or gone entirely (a very old row a retention
// policy already swept, though this deployment keeps no such policy
// today) — is frozen ambiguous, so ConfirmLink or AbandonLink can act on
// it instead of it sitting unclaimable forever. A claim job still
// pending or running (or one that succeeded, which — since a succeeded
// StartChat always moves its link to ready — would mean this link's own
// creation_pending state is itself inconsistent with its job) is left
// alone: freezing it now could race, or paper over, a job that has not
// finished having its say.
func (r *Registry) FreezeLink(ctx context.Context, actorID, linkID string) error {
	return r.ownerOnly(ctx, actorID, func(ctx context.Context, repos domain.Repos, _ domain.OpenWebUIWorkspace, now time.Time) error {
		link, err := repos.OpenWebUILinks.Get(ctx, linkID)
		if err != nil {
			return err
		}
		if link.State != domain.LinkCreationPending {
			return domain.ErrInvalidLinkTransition
		}
		if link.ClaimJobID != nil {
			job, err := repos.Jobs.Get(ctx, *link.ClaimJobID)
			switch {
			case errors.Is(err, domain.ErrNotFound):
				// no claim job left to race against
			case err != nil:
				return fmt.Errorf("get claim job: %w", err)
			case job.State == domain.JobDead || job.State == domain.JobFailed:
				// terminal: safe to freeze
			default:
				return ErrClaimJobStillActive
			}
		}
		return repos.OpenWebUILinks.MarkAmbiguous(ctx, linkID, now)
	})
}

// isRecoverableTurnStatus reports whether status is one ConfirmLink or
// AbandonLink may still act on: a turn that has not yet reached a
// definitive outcome. TurnProviderStatus.IsTerminal's own notion of
// "terminal" is narrower and job-retry-specific (an ambiguous turn stops
// automatic retries, so the job handler treats it as no-longer-actionable
// too — see its own doc comment) but is exactly the state this recovery
// path exists to still act on, so recovery.go defines its own predicate
// rather than reusing it.
func isRecoverableTurnStatus(status domain.TurnProviderStatus) bool {
	return status == domain.TurnPending || status == domain.TurnAmbiguous
}

// latestRecoverableTurn returns linkID's latest non-tombstoned pending or
// ambiguous turn — the (created_at, id)-last match in
// OpenWebUITurnLinkRepository.ListByLink's own stable order, the same
// "last match wins" rule path.go's linkHead applies to a link's succeeded
// turns.
func latestRecoverableTurn(ctx context.Context, repos domain.Repos, linkID string) (domain.OpenWebUITurnLink, bool, error) {
	turns, err := repos.OpenWebUITurnLinks.ListByLink(ctx, linkID)
	if err != nil {
		return domain.OpenWebUITurnLink{}, false, err
	}
	var found domain.OpenWebUITurnLink
	ok := false
	for _, t := range turns {
		if t.TombstonedAt != nil || !isRecoverableTurnStatus(t.Status) {
			continue
		}
		found = t
		ok = true
	}
	return found, ok, nil
}
