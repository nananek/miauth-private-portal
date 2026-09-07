// This file is Issue #53's (OWUI-B) durable job (plan §5.4): given a
// turn Bridge.EnqueueTurn already recorded, it talks to a Provider,
// classifies the outcome per the plan's judgment table, and — on
// success — creates the VirtualActor-authored reply through
// internal/timeline. internal/jobs.Manager dispatches claimed jobs to
// Handle; cmd/server registers it only while Issue #53's generation gate
// is on (§5.6).
package openwebui

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
// (cmd/server: jobsManager.Register(openwebui.JobType, turnJob.Handle)).
const JobType = "openwebui_turn"

// TurnJobConfig bounds a TurnJob's provider calls and path construction.
type TurnJobConfig struct {
	// MaxAttempts must mirror the jobs.Config.MaxAttempts cmd/server
	// passes to jobs.NewManager, the same way llmreply.Config.MaxAttempts
	// does, so Handle can tell whether the attempt it is running is the
	// job's last one.
	MaxAttempts int
	// MaxContextMessages mirrors BridgeConfig's field: the job rebuilds
	// and re-validates the path at execution time (state can change
	// between enqueue and run) rather than trusting the bridge's
	// enqueue-time check.
	MaxContextMessages int
	// WebSearchOverride mirrors OPENWEBUI_WEB_SEARCH_ENABLED (ADR-0005
	// D21, Issue #75 AC#11): nil means unset — resolveWebSearchEnabled
	// then falls back to the selected model's own synced
	// defaultFeatureIds (featureCache) — while a non-nil value overrides
	// every model uniformly, on or off, regardless of that model's own
	// default.
	WebSearchOverride *bool
	// ViewerBaseURL mirrors OPENWEBUI_VIEWER_BASE_URL (Issue #84,
	// ADR-0005 D23). Empty (the default, unset) means the whole feature
	// is off: StartChat never asks for title generation, and complete()
	// never persists a title, so an unconfigured deployment reproduces
	// pre-#84 behavior exactly. TurnJob itself never builds the
	// "<ViewerBaseURL>/c/<remote_chat_id>" link text — that is the wire-
	// projection layer's job (internal/httpserver's projectNote) — this
	// field only gates whether a title is ever requested or recorded at
	// all, deliberately the same on/off switch for both, per the plan's
	// "the title and the URL are always enabled together" decision.
	ViewerBaseURL string
}

// TurnJob implements internal/jobs.Handler for JobType. It depends on
// internal/jobs and internal/timeline the same way internal/llmreply
// does (plan §1): both are use-case packages this one composes, not
// storage or transport.
type TurnJob struct {
	repos        domain.Repos
	timeline     *timeline.Service
	provider     Provider
	toolCache    *ToolConfigCache
	featureCache *FeatureDefaultCache
	cfg          TurnJobConfig
	locks        *threadLocks
	clock        Clock
	logger       *slog.Logger
}

// NewTurnJob builds a TurnJob. repos is the standalone (non-
// transactional) domain.Repos most callers use directly (see
// internal/storage/sqlite.DB); the atomicity CreateGeneratedReplyBy's
// complete hook needs for a successful turn comes from
// timeline.Service.CreateGeneratedReplyBy itself, not from a UnitOfWork
// held here.
//
// toolCache is Issue #75 PR5's per-model tool_ids (Registry.SyncCatalog
// is its only writer; cmd/server shares one instance between it and this
// TurnJob). A nil toolCache — or a cache miss for this turn's model, an
// unsynced or newly discovered one — resolves to no tool_ids at all on
// every request this job sends, the same safe default an inaccessible
// or unconfigured id already falls back to.
//
// featureCache is Issue #75 AC#11's per-model web_search default
// (ADR-0005 D21), read by resolveWebSearchEnabled only when
// cfg.WebSearchOverride is nil (OPENWEBUI_WEB_SEARCH_ENABLED left
// unset); a nil featureCache resolves the same as an unsynced model
// there too — false, never guessing a feature on.
func NewTurnJob(repos domain.Repos, timelineSvc *timeline.Service, provider Provider, toolCache *ToolConfigCache, featureCache *FeatureDefaultCache, cfg TurnJobConfig, clock Clock, logger *slog.Logger) *TurnJob {
	if clock == nil {
		clock = realClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TurnJob{repos: repos, timeline: timelineSvc, provider: provider, toolCache: toolCache, featureCache: featureCache, cfg: cfg, locks: newThreadLocks(), clock: clock, logger: logger}
}

// resolveToolIDs looks up externalModelID's cached tool_ids, or nil if
// there is no cache at all or no entry for this model yet.
func (j *TurnJob) resolveToolIDs(externalModelID string) []string {
	if j.toolCache == nil {
		return nil
	}
	return j.toolCache.Get(externalModelID)
}

// resolveWebSearchEnabled applies ADR-0005 D21's priority rule for one
// model: an explicit cfg.WebSearchOverride (OPENWEBUI_WEB_SEARCH_ENABLED
// set to true or false) always wins, applied uniformly regardless of
// which model this turn resolved to; left nil (unset), it defers to
// externalModelID's own most recently synced defaultFeatureIds via
// featureCache — false for a nil cache or a cache miss, the same
// "never guess a feature on" default resolveToolIDs already applies to
// tool_ids.
func (j *TurnJob) resolveWebSearchEnabled(externalModelID string) bool {
	if j.cfg.WebSearchOverride != nil {
		return *j.cfg.WebSearchOverride
	}
	if j.featureCache == nil {
		return false
	}
	return j.featureCache.Get(externalModelID)
}

// Handle implements internal/jobs.Handler for JobType. See the package
// doc comment and plan §5.4 for the full step-by-step rationale; in
// order: decode the payload; load the turn/link/workspace/model/entry/
// author; skip a duplicate delivery of an already-terminal turn; re-check
// generation eligibility (state can have changed since enqueue); take
// this thread's single-flight lock; rebuild and re-validate the reply-
// tree path; then dispatch on the link's state to either its one-shot
// StartChat or a ContinueTurn (itself possibly a lookup-first retry).
func (j *TurnJob) Handle(ctx context.Context, job domain.Job) error {
	var payload turnJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return jobs.Permanent(fmt.Errorf("openwebui: turn: decode payload: %w", err))
	}
	if job.PayloadVersion != 1 {
		return jobs.Permanent(fmt.Errorf("openwebui: turn: unsupported payload version %d", job.PayloadVersion))
	}

	turn, err := j.repos.OpenWebUITurnLinks.Get(ctx, payload.TurnID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("openwebui: turn: turn %s not found: %w", payload.TurnID, err))
		}
		return fmt.Errorf("openwebui: turn: get turn: %w", err)
	}

	// Duplicate delivery: a prior run of this exact job already reached a
	// terminal outcome (or crashed after creating the assistant entry but
	// before its own transition committed the job as succeeded). Neither
	// case may run the provider again.
	if turn.Status.IsTerminal() || turn.AssistantEntryID != nil {
		j.logger.Info("openwebui turn already terminal, skipping duplicate delivery", "job_id", job.ID, "turn_id", turn.ID)
		return nil
	}

	link, err := j.repos.OpenWebUILinks.Get(ctx, turn.LinkID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("openwebui: turn: link %s not found: %w", turn.LinkID, err))
		}
		return fmt.Errorf("openwebui: turn: get link: %w", err)
	}
	workspace, err := j.repos.OpenWebUIWorkspaces.Get(ctx, link.WorkspaceID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return j.failPermanent(ctx, turn, link, domain.FailureCategoryPolicyDenied, "workspace no longer exists")
		}
		return fmt.Errorf("openwebui: turn: get workspace: %w", err)
	}
	model, err := j.repos.OpenWebUIModels.Get(ctx, link.ModelID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return j.failPermanent(ctx, turn, link, domain.FailureCategoryPolicyDenied, "model no longer exists")
		}
		return fmt.Errorf("openwebui: turn: get model: %w", err)
	}
	entry, err := j.repos.Entries.Get(ctx, turn.LocalMessageID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return j.failPermanent(ctx, turn, link, domain.FailureCategoryOrphan, "source entry no longer exists")
		}
		return fmt.Errorf("openwebui: turn: get entry: %w", err)
	}
	author, err := j.repos.Actors.Get(ctx, entry.AuthorActorID)
	if err != nil {
		return fmt.Errorf("openwebui: turn: get author: %w", err)
	}

	if !eligibleForGeneration(workspace, model, link, author, entry) {
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryPolicyDenied, "generation eligibility no longer holds")
	}

	unlock, err := j.locks.Lock(ctx, link.ThreadID)
	if err != nil {
		return fmt.Errorf("openwebui: turn: acquire thread lock: %w", err)
	}
	defer unlock()

	path, err := BuildTurnPath(ctx, j.repos, entry, PathBounds{MaxContextMessages: j.cfg.MaxContextMessages})
	if err != nil {
		return j.failPermanent(ctx, turn, link, pathErrorCategory(err), err.Error())
	}

	switch link.State {
	case domain.LinkCreationPending:
		return j.handleCreationPending(ctx, job, turn, link, workspace, model, entry, path)
	case domain.LinkReady:
		return j.handleReady(ctx, job, turn, link, model, entry, path)
	default:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryLinkNotReady, fmt.Sprintf("link state %q does not allow a turn", link.State))
	}
}

// eligibleForGeneration re-checks, at execution time, every condition
// that gated this turn's enqueue: the workspace and its generation flag,
// its credential reference, the model's active/ownership state, the
// entry author's eligibility, and the entry's own visibility. Any of
// these can have changed since Bridge.EnqueueTurn ran in the same
// transaction as the owner's post.
func eligibleForGeneration(workspace domain.OpenWebUIWorkspace, model domain.OpenWebUIModel, link domain.OpenWebUIConversationLink, author domain.Actor, entry domain.Entry) bool {
	if !workspace.Enabled || !workspace.GenerationEnabled {
		return false
	}
	if workspace.SecretRef != SecretRefAPIKey {
		return false
	}
	if !model.Active || model.ID != link.ModelID || model.WorkspaceID != workspace.ID {
		return false
	}
	if !author.IsLoginable() {
		return false
	}
	if entry.HiddenAt != nil || entry.ArchivedAt != nil {
		return false
	}
	return true
}

// handleCreationPending issues the link's single, never-repeated
// StartChat, or — if a previous attempt's outcome is unknown — freezes
// the link ambiguous without ever calling the provider again (ADR-0005
// D7: a creation_pending StartChat is never replayed).
func (j *TurnJob) handleCreationPending(
	ctx context.Context, job domain.Job, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink,
	workspace domain.OpenWebUIWorkspace, model domain.OpenWebUIModel, entry domain.Entry, path TurnPath,
) error {
	if !link.AllowsInitialStartChat(job.ID) {
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryLinkNotReady, "link's creation claim does not belong to this job")
	}
	if turn.Attempt > 0 {
		// A prior attempt already called StartChat; its outcome is
		// unknown (a crash or lease loss interrupted this handler before
		// it could record one). Chat creation is never replayed, so the
		// link is frozen for owner recovery instead.
		return j.failAmbiguous(ctx, turn, link, strPtr(domain.FailureCategoryCreationLost))
	}

	userMsgID := newRemoteMessageID()
	assistantMsgID := newRemoteMessageID()
	now := j.clock.Now().UTC()

	turn, err := j.setCorrelation(ctx, turn, domain.OpenWebUITurnCorrelation{
		RemoteMessageID: &userMsgID, RemoteAssistantMessageID: &assistantMsgID,
	}, now)
	if err != nil {
		return fmt.Errorf("openwebui: turn: set remote correlation: %w", err)
	}
	if err := j.repos.OpenWebUITurnLinks.BeginAttempt(ctx, turn.ID, turn.Attempt+1, now); err != nil {
		return fmt.Errorf("openwebui: turn: begin attempt: %w", err)
	}

	result, err := j.provider.StartChat(ctx, StartChatRequest{
		ModelID:               model.ExternalModelID,
		Messages:              ProviderMessages(path),
		NewTurn:               Message{Role: pathRoleUser, Content: stripMentionTagsForProvider(entry.Body)},
		IDs:                   TurnIDs{UserMessageID: userMsgID, AssistantMessageID: assistantMsgID},
		CorrelationID:         turn.RequestID,
		SentAt:                now,
		ToolIDs:               j.resolveToolIDs(model.ExternalModelID),
		WebSearchEnabled:      j.resolveWebSearchEnabled(model.ExternalModelID),
		EnableTitleGeneration: j.cfg.ViewerBaseURL != "",
		OnChatCreated: func(hookCtx context.Context, remoteChatID string) error {
			confirmedAt := j.clock.Now().UTC()
			if err := j.repos.OpenWebUILinks.MarkReady(hookCtx, link.ID, remoteChatID, nil, confirmedAt); err != nil {
				return fmt.Errorf("mark link ready: %w", err)
			}
			// Route this write through setCorrelation (not a direct
			// SetRemoteCorrelation call) so it also updates the outer
			// turn variable this closure captures. SetRemoteCorrelation
			// replaces all five correlation columns rather than merging,
			// so complete()'s own later call — which reads RemoteChatID
			// off that same turn variable — would otherwise null this
			// chat id straight back out after a successful first turn.
			updated, err := j.setCorrelation(hookCtx, turn, domain.OpenWebUITurnCorrelation{
				RemoteChatID: &remoteChatID, RemoteMessageID: &userMsgID, RemoteAssistantMessageID: &assistantMsgID,
			}, confirmedAt)
			if err != nil {
				return fmt.Errorf("set remote correlation: %w", err)
			}
			turn = updated
			if workspace.ChatCreateStatus != domain.CapabilityVerified {
				if err := j.repos.OpenWebUIWorkspaces.SetCapabilityStatus(hookCtx, workspace.ID, domain.CapabilityVerified, workspace.ChatContinueStatus, confirmedAt); err != nil {
					return fmt.Errorf("record chat-create capability: %w", err)
				}
			}
			return nil
		},
	})
	if err != nil {
		return j.handleCreateError(ctx, job, turn, link, err)
	}
	return j.complete(ctx, turn, link, model, entry, result, false)
}

// handleCreateError classifies a StartChat failure per the plan's
// judgment table. A non-*ProviderError is the OnChatCreated hook's own
// error (a local bookkeeping failure, never a provider classification):
// it is left retryable and the turn/link untouched, since BeginAttempt
// already recorded the attempt — the next delivery's turn.Attempt > 0
// branch resolves it as creation_lost rather than replaying the call.
//
// StartChat bundles two calls (createChat, then the first runTurn) into
// one, so an error coming back from it can belong to either phase —
// exactly what ProviderError.Phase exists to tell apart (see its doc
// comment). A pe.Phase other than PhaseCreate means createChat and
// OnChatCreated already succeeded (the link is ready in storage by now),
// so the failure is this turn's, not the chat's: it gets the same
// bounded-retry treatment handleTurnError gives a ready link's
// continuation, rather than being frozen ambiguous on the very first
// attempt the way an actual creation failure is.
func (j *TurnJob) handleCreateError(ctx context.Context, job domain.Job, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink, err error) error {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return fmt.Errorf("openwebui: turn: create: onChatCreated: %w", err)
	}
	if pe.Phase != PhaseCreate {
		return j.handleTurnError(ctx, job, turn, link, err)
	}
	switch pe.Category {
	case CategoryAuthFailed:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryAuthFailed, "chat creation rejected the credential")
	case CategoryClientRejected:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryClientRejected, "chat creation rejected the request")
	case CategoryPolicyViolation:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryPolicyViolation, "chat creation refused by local policy")
	default: // rate_limited, server_error, transport, timeout, ambiguous, contract_failed
		// A creation call is never replayed (ADR-0005 D7), so this branch
		// does not consult isLastAttempt the way handleTurnError's own
		// default case does — an actual remote-side failure here is
		// ambiguous on the very first attempt, not the last one. But
		// plan §5.4 step 8's cancellation rule still applies: a failure
		// that surfaces only because ctx was already cancelled (Manager
		// shutting down, or this job's lease expiring mid-call) must
		// leave the turn exactly as BeginAttempt left it — pending,
		// attempt already recorded — rather than freezing the link
		// ambiguous on this delivery. The *next* delivery's turn.Attempt
		// > 0 check above (an outcome truly unknown, whatever the
		// reason) is what performs that freeze instead, the same way it
		// already does for a lost creation response.
		if ctx.Err() != nil {
			return fmt.Errorf("openwebui: turn: create: %s", pe.Category)
		}
		return j.failAmbiguous(ctx, turn, link, strPtr(domain.FailureCategoryCreationLost))
	}
}

// handleReady dispatches a turn on an already-confirmed remote chat:
// either its first ContinueTurn, or — if a previous attempt's outcome is
// unknown — a lookup-first retry (ADR-0005 D7's addendum).
func (j *TurnJob) handleReady(
	ctx context.Context, job domain.Job, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink,
	model domain.OpenWebUIModel, entry domain.Entry, path TurnPath,
) error {
	if turn.Attempt > 0 && turn.RemoteAssistantMessageID != nil {
		return j.handleReadyRetry(ctx, job, turn, link, model, entry, path)
	}
	if link.RemoteCurrentID == nil || link.RemoteChatID == nil {
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryLinkNotReady, "ready link has no confirmed remote turn to continue from")
	}

	userMsgID := newRemoteMessageID()
	assistantMsgID := newRemoteMessageID()
	now := j.clock.Now().UTC()

	turn, err := j.setCorrelation(ctx, turn, domain.OpenWebUITurnCorrelation{
		RemoteChatID: link.RemoteChatID, RemoteMessageID: &userMsgID, RemoteAssistantMessageID: &assistantMsgID, RemoteParentID: link.RemoteCurrentID,
	}, now)
	if err != nil {
		return fmt.Errorf("openwebui: turn: set remote correlation: %w", err)
	}
	if err := j.repos.OpenWebUITurnLinks.BeginAttempt(ctx, turn.ID, turn.Attempt+1, now); err != nil {
		return fmt.Errorf("openwebui: turn: begin attempt: %w", err)
	}

	result, err := j.provider.ContinueTurn(ctx, ContinueTurnRequest{
		RemoteChatID:     *link.RemoteChatID,
		ModelID:          model.ExternalModelID,
		Messages:         ProviderMessages(path),
		NewTurn:          Message{Role: pathRoleUser, Content: stripMentionTagsForProvider(entry.Body)},
		IDs:              TurnIDs{UserMessageID: userMsgID, AssistantMessageID: assistantMsgID, ParentAssistantID: link.RemoteCurrentID},
		CorrelationID:    turn.RequestID,
		SentAt:           now,
		ToolIDs:          j.resolveToolIDs(model.ExternalModelID),
		WebSearchEnabled: j.resolveWebSearchEnabled(model.ExternalModelID),
	})
	if err != nil {
		return j.handleTurnError(ctx, job, turn, link, err)
	}
	return j.complete(ctx, turn, link, model, entry, result, true)
}

// handleReadyRetry resolves a continuation whose previous attempt's
// outcome is unknown by looking it up before ever resending (ADR-0005
// D7's addendum): a confirmed success is adopted, a confirmed failure or
// a message the provider does not (yet) have is resent under the same
// ids (safe: compat's completions endpoint overwrites in place rather
// than duplicating), and a still-generating turn is polled again rather
// than resent.
func (j *TurnJob) handleReadyRetry(
	ctx context.Context, job domain.Job, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink,
	model domain.OpenWebUIModel, entry domain.Entry, path TurnPath,
) error {
	turn, remoteChatID, ok := resolveRemoteChatID(turn, link)
	if !ok {
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryLinkNotReady, "turn has no known remote chat id to look up")
	}
	outcome, err := j.provider.LookupTurnOutcome(ctx, remoteChatID, *turn.RemoteAssistantMessageID)
	if err != nil {
		return j.handleLookupError(ctx, job, turn, link, err)
	}
	switch {
	case outcome.Found && outcome.Done && !outcome.HasError && outcome.Content != "":
		return j.complete(ctx, turn, link, model, entry, TurnResult{
			Content: outcome.Content, RemoteCurrentID: outcome.RemoteCurrentID,
			PromptTokens: outcome.PromptTokens, CompletionTokens: outcome.CompletionTokens,
			// Title, but never Sources: a turn recovered through this
			// lookup-only path never gets sources attached — see
			// TurnOutcome.Sources's absence, documented on TurnOutcome
			// itself, for why.
			Title: outcome.Title,
		}, true)
	case outcome.HasError || !outcome.Found || (outcome.Done && outcome.Content == ""):
		return j.resendContinue(ctx, job, turn, link, model, entry, path)
	default: // Found && !Done && !HasError: still generating.
		if j.isLastAttempt(ctx, job) {
			return j.failAmbiguous(ctx, turn, link, nil)
		}
		return errors.New("openwebui: turn: lookup: still generating")
	}
}

// resolveRemoteChatID returns the remote chat id a lookup-or-resend
// should target, preferring turn's own recorded correlation and falling
// back to link's — a ready link's own remote_chat_id, which MarkReady
// always sets — when the turn's own write for it was lost: OnChatCreated
// can commit MarkReady and then fail or be interrupted before the
// companion SetRemoteCorrelation on this same turn runs (a DB error, a
// crash, an expired lease), leaving turn.attempt already at 1 and
// turn.RemoteAssistantMessageID already recorded, but turn.RemoteChatID
// still nil. Dereferencing turn.RemoteChatID unconditionally in that
// state is a nil-pointer panic jobs.Manager does not recover from — it
// takes the whole process down, not just the one job. When it falls
// back, it also updates turn's in-memory copy so a later complete() call
// persists the recovered id instead of writing NULL over it again (see
// SetRemoteCorrelation's own doc comment on why that matters). ok is
// false only when neither the turn nor its ready link knows a chat id,
// which the caller must treat as link_not_ready.
func resolveRemoteChatID(turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink) (domain.OpenWebUITurnLink, string, bool) {
	if turn.RemoteChatID != nil {
		return turn, *turn.RemoteChatID, true
	}
	if link.RemoteChatID == nil {
		return turn, "", false
	}
	turn.RemoteChatID = link.RemoteChatID
	return turn, *link.RemoteChatID, true
}

// resendContinue re-sends a continuation under the exact ids already
// recorded for this turn. compat's completions endpoint overwrites the
// existing assistant message in place when called with the same ids
// (ADR-0005 D7's addendum), so this never adds a second node.
func (j *TurnJob) resendContinue(
	ctx context.Context, job domain.Job, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink,
	model domain.OpenWebUIModel, entry domain.Entry, path TurnPath,
) error {
	// Guarded independently of handleReadyRetry's own call to
	// resolveRemoteChatID (which already ran before this is reached, on
	// resendContinue's only call path today): this stays safe on its own
	// if another caller is ever added, rather than trusting the caller to
	// have resolved it first.
	turn, remoteChatID, ok := resolveRemoteChatID(turn, link)
	if !ok {
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryLinkNotReady, "turn has no known remote chat id to resend against")
	}
	now := j.clock.Now().UTC()
	if err := j.repos.OpenWebUITurnLinks.BeginAttempt(ctx, turn.ID, turn.Attempt+1, now); err != nil {
		return fmt.Errorf("openwebui: turn: begin attempt: %w", err)
	}
	result, err := j.provider.ContinueTurn(ctx, ContinueTurnRequest{
		RemoteChatID:     remoteChatID,
		ModelID:          model.ExternalModelID,
		Messages:         ProviderMessages(path),
		NewTurn:          Message{Role: pathRoleUser, Content: stripMentionTagsForProvider(entry.Body)},
		IDs:              TurnIDs{UserMessageID: *turn.RemoteMessageID, AssistantMessageID: *turn.RemoteAssistantMessageID, ParentAssistantID: turn.RemoteParentID},
		CorrelationID:    turn.RequestID,
		SentAt:           now,
		ToolIDs:          j.resolveToolIDs(model.ExternalModelID),
		WebSearchEnabled: j.resolveWebSearchEnabled(model.ExternalModelID),
	})
	if err != nil {
		return j.handleTurnError(ctx, job, turn, link, err)
	}
	return j.complete(ctx, turn, link, model, entry, result, true)
}

// handleTurnError classifies a ContinueTurn failure per the plan's
// judgment table. A definitive category fails the turn permanently
// without disturbing the (still ready) link; a transient one is left
// retryable until the job's last attempt, when it freezes the link
// ambiguous instead of retrying forever.
func (j *TurnJob) handleTurnError(ctx context.Context, job domain.Job, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink, err error) error {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return fmt.Errorf("openwebui: turn: continue: %w", err)
	}
	switch pe.Category {
	case CategoryAuthFailed:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryAuthFailed, "continuation rejected the credential")
	case CategoryClientRejected:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryClientRejected, "continuation rejected the request")
	case CategoryContractFailed:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryContractFailed, "continuation response did not match the pinned contract")
	case CategoryPolicyViolation:
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryPolicyViolation, "continuation refused by local policy")
	case CategoryTurnFailed:
		if j.isLastAttempt(ctx, job) {
			return j.failPermanent(ctx, turn, link, domain.FailureCategoryTurnFailed, "provider reported an error for this turn")
		}
		return fmt.Errorf("openwebui: turn: continue: %s", CategoryTurnFailed)
	default: // rate_limited, server_error, transport, timeout, ambiguous
		if j.isLastAttempt(ctx, job) {
			return j.failAmbiguous(ctx, turn, link, nil)
		}
		return fmt.Errorf("openwebui: turn: continue: %s", pe.Category)
	}
}

// handleLookupError classifies a LookupTurnOutcome failure the same way
// handleTurnError does for ContinueTurn: a rejected credential fails the
// turn outright, everything else is retried (lookup again, never resend)
// until the job's last attempt.
func (j *TurnJob) handleLookupError(ctx context.Context, job domain.Job, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink, err error) error {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return fmt.Errorf("openwebui: turn: lookup: %w", err)
	}
	if pe.Category == CategoryAuthFailed {
		return j.failPermanent(ctx, turn, link, domain.FailureCategoryAuthFailed, "outcome lookup rejected the credential")
	}
	if j.isLastAttempt(ctx, job) {
		return j.failAmbiguous(ctx, turn, link, nil)
	}
	return fmt.Errorf("openwebui: turn: lookup: %s", pe.Category)
}

// complete records a successful turn and creates the VirtualActor-
// authored reply atomically alongside it (ADR-0005 D6's notification
// policy: a reply notification only ever follows a genuine success).
// isContinuation marks a turn that succeeded through an actual
// ContinueTurn call (handleReady's first attempt, resendContinue, or a
// prior attempt's outcome adopted via handleReadyRetry's lookup) rather
// than through StartChat's own bundled first turn: only then does
// complete record the workspace's chat_continue capability as verified
// (once, the same way handleCreationPending's OnChatCreated hook already
// records chat_create) — plan §5.4 step 7's "初回なら
// SetCapabilityStatus(chatContinue=verified)".
func (j *TurnJob) complete(
	ctx context.Context, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink,
	model domain.OpenWebUIModel, entry domain.Entry, result TurnResult, isContinuation bool,
) error {
	now := j.clock.Now().UTC()
	_, err := j.timeline.CreateGeneratedReplyBy(ctx, model.ActorID, entry.ID, result.Content,
		func(cctx context.Context, repos domain.Repos, assistantEntry domain.Entry) error {
			if err := repos.OpenWebUITurnLinks.SetAssistantEntry(cctx, turn.ID, assistantEntry.ID, now); err != nil {
				return fmt.Errorf("set assistant entry: %w", err)
			}
			var title *string
			if j.cfg.ViewerBaseURL != "" {
				// Gated the same way EnableTitleGeneration is: an
				// unconfigured deployment never persists a title, even
				// if the provider's own response happened to carry one
				// (Open WebUI is known to overwrite the placeholder with
				// the raw first message on its own — see
				// precreatedChatTitle's doc comment in
				// internal/provider/openwebui/client.go), so #84 stays
				// strictly opt-in end to end.
				title = result.Title
			}
			if err := repos.OpenWebUITurnLinks.RecordOutcome(cctx, turn.ID, domain.TurnOutcomeRecord{
				Status: domain.TurnSucceeded, PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
				FinishReason: result.FinishReason, Title: title, Sources: convertSources(result.Sources),
			}, now); err != nil {
				return fmt.Errorf("record outcome: %w", err)
			}
			if err := repos.OpenWebUITurnLinks.SetRemoteCorrelation(cctx, turn.ID, domain.OpenWebUITurnCorrelation{
				RemoteChatID: turn.RemoteChatID, RemoteMessageID: turn.RemoteMessageID, RemoteAssistantMessageID: turn.RemoteAssistantMessageID,
				RemoteParentID: turn.RemoteParentID, RemoteCurrentID: result.RemoteCurrentID,
			}, now); err != nil {
				return fmt.Errorf("set remote correlation: %w", err)
			}
			if err := repos.OpenWebUILinks.SetRemoteCurrent(cctx, link.ID, result.RemoteCurrentID, now); err != nil {
				return fmt.Errorf("set remote current: %w", err)
			}
			if isContinuation {
				workspace, err := repos.OpenWebUIWorkspaces.Get(cctx, link.WorkspaceID)
				if err != nil {
					return fmt.Errorf("get workspace: %w", err)
				}
				if workspace.ChatContinueStatus != domain.CapabilityVerified {
					if err := repos.OpenWebUIWorkspaces.SetCapabilityStatus(cctx, workspace.ID, workspace.ChatCreateStatus, domain.CapabilityVerified, now); err != nil {
						return fmt.Errorf("record chat-continue capability: %w", err)
					}
				}
			}
			return repos.Notifications.Create(cctx, domain.Notification{
				ID: domain.NewID(), Type: domain.NotificationReply, RelatedEntryID: assistantEntry.ID, CreatedAt: now,
			})
		})
	if err != nil {
		if errors.Is(err, timeline.ErrAuthorNotEligible) {
			return j.failPermanent(ctx, turn, link, domain.FailureCategoryPolicyDenied, "generation author no longer eligible")
		}
		return fmt.Errorf("openwebui: turn: create generated reply: %w", err)
	}
	j.logger.Info("openwebui turn succeeded", "turn_id", turn.ID, "link_id", link.ID)
	j.logSources(turn.ID, result.Sources)
	return nil
}

// logSources records ADR-0005 D22's own logging requirement: one INFO
// line per source this turn's completions response carried, naming only
// its kind and display name — plus, for a tool call only, its arguments
// — mirroring D6's existing rule that provider response bodies (here,
// document[]'s raw tool/page text, never captured into Source at all;
// see openwebui.Source's own doc comment) are never logged verbatim.
func (j *TurnJob) logSources(turnID string, sources []Source) {
	for _, src := range sources {
		if src.Kind == SourceKindTool {
			j.logger.Info("openwebui turn source", "turn_id", turnID, "kind", src.Kind, "display_name", src.DisplayName, "arguments", src.Arguments)
			continue
		}
		j.logger.Info("openwebui turn source", "turn_id", turnID, "kind", src.Kind, "display_name", src.DisplayName)
	}
}

// setCorrelation writes corr through SetRemoteCorrelation and mirrors
// the non-nil fields onto turn's in-memory copy, so callers threading
// turn through the rest of Handle see the values they just persisted
// without a re-read.
func (j *TurnJob) setCorrelation(ctx context.Context, turn domain.OpenWebUITurnLink, corr domain.OpenWebUITurnCorrelation, at time.Time) (domain.OpenWebUITurnLink, error) {
	if err := j.repos.OpenWebUITurnLinks.SetRemoteCorrelation(ctx, turn.ID, corr, at); err != nil {
		return turn, err
	}
	if corr.RemoteChatID != nil {
		turn.RemoteChatID = corr.RemoteChatID
	}
	if corr.RemoteMessageID != nil {
		turn.RemoteMessageID = corr.RemoteMessageID
	}
	if corr.RemoteAssistantMessageID != nil {
		turn.RemoteAssistantMessageID = corr.RemoteAssistantMessageID
	}
	if corr.RemoteParentID != nil {
		turn.RemoteParentID = corr.RemoteParentID
	}
	if corr.RemoteCurrentID != nil {
		turn.RemoteCurrentID = corr.RemoteCurrentID
	}
	turn.UpdatedAt = at
	return turn, nil
}

// isLastAttempt reports whether job's next delivery, if any, would be
// its final one — the same "will MaxAttempts be exhausted" check
// internal/llmreply.Service.handleProviderFailure makes, including its
// ctx.Err() == nil guard (plan §5.4 step 8): a failure that surfaces
// only because ctx was already cancelled (jobs.Manager shutting down, or
// this job's lease expiring mid-call) must not consume the job's own
// attempt budget by freezing a link ambiguous on what looks like the
// last attempt — Manager redelivers it on its own restart schedule
// instead, and that redelivery gets the ordinary attempt count.
func (j *TurnJob) isLastAttempt(ctx context.Context, job domain.Job) bool {
	return ctx.Err() == nil && job.Attempt+1 >= j.cfg.MaxAttempts
}

// failPermanent records turn as failed with category, freezes the link
// with the same category only while it is still creation_pending
// (repository's own CAS guard is a no-op — ErrConflict, ignored — from
// any other state, matching the rule that a failure after a chat exists
// belongs to the turn, not the link), and returns a jobs.Permanent
// error so the job is never retried.
func (j *TurnJob) failPermanent(ctx context.Context, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink, category string, reason string) error {
	now := j.clock.Now().UTC()
	cat := category
	if err := j.repos.OpenWebUITurnLinks.RecordOutcome(ctx, turn.ID, domain.TurnOutcomeRecord{Status: domain.TurnFailed, FailureCategory: &cat}, now); err != nil {
		return fmt.Errorf("openwebui: turn: record outcome: %w", err)
	}
	if link.State == domain.LinkCreationPending {
		if err := j.repos.OpenWebUILinks.MarkFailed(ctx, link.ID, category, now); err != nil && !errors.Is(err, domain.ErrConflict) {
			return fmt.Errorf("openwebui: turn: mark link failed: %w", err)
		}
	}
	return jobs.Permanent(fmt.Errorf("openwebui: turn: %s: %s", category, reason))
}

// failAmbiguous records turn as ambiguous (with category if non-nil —
// matching a link's own MarkAmbiguous, an ambiguous outcome need not
// have a known category), freezes the link ambiguous from either
// creation_pending or ready, and returns a jobs.Permanent error: an
// ambiguous outcome is resolved only by an explicit owner recovery
// action (Issue #53's later PR), never by another automatic attempt.
func (j *TurnJob) failAmbiguous(ctx context.Context, turn domain.OpenWebUITurnLink, link domain.OpenWebUIConversationLink, category *string) error {
	now := j.clock.Now().UTC()
	if err := j.repos.OpenWebUITurnLinks.RecordOutcome(ctx, turn.ID, domain.TurnOutcomeRecord{Status: domain.TurnAmbiguous, FailureCategory: category}, now); err != nil {
		return fmt.Errorf("openwebui: turn: record outcome: %w", err)
	}
	if err := j.repos.OpenWebUILinks.MarkAmbiguous(ctx, link.ID, now); err != nil && !errors.Is(err, domain.ErrConflict) {
		return fmt.Errorf("openwebui: turn: mark link ambiguous: %w", err)
	}
	return jobs.Permanent(errors.New("openwebui: turn: ambiguous"))
}

func strPtr(s string) *string { return &s }

// convertSources maps this package's own provider-facing Source (Issue
// #81; see provider.go's doc comment on why it is a distinct type from
// domain.Source) onto the persisted domain shape RecordOutcome takes. A
// nil/empty input yields nil, matching domain.EncodeSources' own
// "nothing to store" case.
func convertSources(sources []Source) []domain.Source {
	if len(sources) == 0 {
		return nil
	}
	out := make([]domain.Source, len(sources))
	for i, s := range sources {
		out[i] = domain.Source{Kind: s.Kind, DisplayName: s.DisplayName, URL: s.URL, Arguments: s.Arguments}
	}
	return out
}
