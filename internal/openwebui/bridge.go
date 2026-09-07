// This file is Issue #53's (OWUI-B) enqueue path (plan §5.1): the hook
// internal/timeline's Create{Root,Reply}WithHook invokes, inside the
// same transaction as the owner's post, to atomically claim a branch's
// conversation link (or continue an existing one), record a turn, and
// enqueue the durable "openwebui_turn" job that will actually talk to
// the provider. No network call happens here — the local post's success
// never depends on the provider being reachable (AGENTS.md, roadmap).
package openwebui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// BridgeConfig configures Bridge's enqueue-time policy.
type BridgeConfig struct {
	// MaxContextMessages bounds BuildTurnPath's walk the same way
	// TurnJobConfig's field does — see PathBounds. A post whose path
	// exceeds it is skipped at enqueue time (no job, no link, no turn),
	// exactly like an ineligible path node.
	MaxContextMessages int
}

// Bridge is the enqueue-time half of Issue #53's outbound turn bridge.
// It depends on nothing but internal/domain: no HTTP client, no
// provider, no job handler logic lives here — see TurnJob for the
// durable job this enqueues.
type Bridge struct {
	cfg    BridgeConfig
	clock  Clock
	logger *slog.Logger
}

// NewBridge builds a Bridge. A nil clock defaults to the real wall
// clock; a nil logger defaults to slog.Default().
func NewBridge(cfg BridgeConfig, clock Clock, logger *slog.Logger) *Bridge {
	if clock == nil {
		clock = realClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Bridge{cfg: cfg, clock: clock, logger: logger}
}

// turnJobPayload is the JSON internal/httpserver's enqueue hook
// (through EnqueueTurn) attaches to a domain.Job{JobType: JobType}.
// ThreadID is included even though TurnJob could derive it from the
// turn/link rows, so the durable job lease/lock (ADR-0005 D5's
// per-thread single-flight) can be taken before either row is read.
type turnJobPayload struct {
	TurnID   string `json:"turnId"`
	LinkID   string `json:"linkId"`
	Revision int    `json:"revision"`
	ThreadID string `json:"threadId"`
}

// EnqueueTurn is a timeline.EntryHook (matched structurally, not by
// import, to keep this package's dependency on internal/timeline
// optional at the type level — see cmd/server's wiring). It runs inside
// the transaction that just created entry.
//
// Steps (plan §5.1, generalized by Issue #75 PR3's model routing, and
// reordered by the Issue #75 rebase onto Issue #70/#80/#82's empty-body
// skip so the two never race — see the ambiguous-vs-empty priority note
// below): (1) restrict to a loginable author's user_post; (2) require an
// enabled, generation-enabled workspace with the one known secret_ref
// and a registered default model; (3) build entry's reply-tree path,
// skipping (not failing) the post when it is ineligible for this
// bridge; (4) resolve which model entry.Body @mentions, if any
// (ResolveModelMentions) — two or more distinct mentioned models is the
// "ambiguous_model_selection" case, which records a failed link/turn and
// returns without enqueueing anything, checked *before* the empty-body
// skip in step (5) so a mention-only post with an ambiguous selection is
// always reported rather than silently dropped as empty; no mention
// falls back to the default model, exactly one mention uses that model;
// (5) for the non-ambiguous cases, skip a post whose provider-facing
// text — entry.Body with every @mention omitted (Issue #70) — is empty
// or whitespace-only, since that is nothing for the provider to reply
// to; (6) apply ADR-0005 D4's branch rule against the resolved model
// (SelectBranch); (7) enqueue the job and claim the link (new branch) or
// reuse it (continuation); (8) treat a conflict on any of those writes
// as "this turn is already enqueued" rather than an error, so a retried
// delivery of the same entry-creation path never double-enqueues.
func (b *Bridge) EnqueueTurn(ctx context.Context, repos domain.Repos, entry domain.Entry) error {
	if entry.Kind != domain.EntryUserPost {
		return nil
	}
	author, err := repos.Actors.Get(ctx, entry.AuthorActorID)
	if err != nil {
		return fmt.Errorf("openwebui: bridge: resolve entry author: %w", err)
	}
	if !author.IsLoginable() {
		return nil
	}

	workspace, err := repos.OpenWebUIWorkspaces.GetEnabled(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("openwebui: bridge: get enabled workspace: %w", err)
	}
	if !workspace.GenerationEnabled {
		return nil
	}
	if workspace.DefaultModelID == nil {
		return nil
	}
	defaultModel, err := repos.OpenWebUIModels.Get(ctx, *workspace.DefaultModelID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("openwebui: bridge: get default model: %w", err)
	}
	if workspace.SecretRef != SecretRefAPIKey {
		return fmt.Errorf("openwebui: bridge: workspace secret_ref %q is not %q", workspace.SecretRef, SecretRefAPIKey)
	}

	// Only the path's eligibility gates enqueueing here; TurnJob rebuilds
	// and actually sends it at execution time (state can change between
	// enqueue and run).
	if _, err := BuildTurnPath(ctx, repos, entry, PathBounds{MaxContextMessages: b.cfg.MaxContextMessages}); err != nil {
		b.logger.Debug("openwebui bridge: skipping enqueue, path ineligible",
			"entry_id", entry.ID, "error_category", pathErrorCategory(err))
		return nil
	}

	matched, err := ResolveModelMentions(ctx, repos, workspace, entry.Body)
	if err != nil {
		return fmt.Errorf("openwebui: bridge: resolve model mentions: %w", err)
	}

	var model domain.OpenWebUIModel
	switch len(matched) {
	case 0:
		model = defaultModel
		if !model.Active {
			return nil
		}
	case 1:
		model = matched[0]
	default:
		b.logger.Debug("openwebui bridge: ambiguous model selection, recording failed and skipping enqueue",
			"entry_id", entry.ID, "matched_models", len(matched))
		return b.recordAmbiguousModelSelection(ctx, repos, entry, workspace, defaultModel.ID)
	}

	if strings.TrimSpace(stripMentionTagsForProvider(entry.Body)) == "" {
		// Nothing but @mentions (and/or whitespace): once the mentions
		// are omitted for the provider (Issue #70), there is no text
		// left to send as this turn's own message. Skip quietly, the
		// same as every other enqueue-time gate in this function — this
		// is not a failure, just nothing to start a turn over. Checked
		// only here, after the ambiguous-selection case above has had
		// its chance to record its own explicit failure: a post that is
		// only "@modelA @modelB" must surface as
		// ambiguous_model_selection, not be swallowed by this check.
		return nil
	}

	continuation, err := SelectBranch(ctx, repos, entry, model.ID)
	if err != nil {
		return fmt.Errorf("openwebui: bridge: select branch: %w", err)
	}

	now := b.clock.Now().UTC()
	jobID := domain.NewID()
	turnID := domain.NewID()
	requestID := domain.NewID()

	var linkID, branchID string
	var remoteParentID *string
	newBranch := continuation == nil
	if newBranch {
		linkID = domain.NewID()
		branchID = domain.NewID()
	} else {
		linkID = continuation.Link.ID
		branchID = continuation.Link.BranchID
		remoteParentID = continuation.Link.RemoteCurrentID
	}

	payload, err := json.Marshal(turnJobPayload{TurnID: turnID, LinkID: linkID, Revision: 1, ThreadID: entry.ThreadID})
	if err != nil {
		return fmt.Errorf("openwebui: bridge: encode job payload: %w", err)
	}
	// Keyed on entry.ID and revision alone, deliberately never on linkID:
	// linkID is freshly minted on every call for a new branch, so
	// including it here would make two deliveries of the same entry each
	// mint their own distinct key and both succeed — exactly the
	// duplicate this key exists to prevent. entry.ID already uniquely
	// identifies "this owner post's one turn" regardless of which branch
	// ends up backing it.
	idempotencyKey := "openwebui_turn:" + entry.ID + ":1"

	job := domain.Job{
		ID:             jobID,
		JobType:        JobType,
		Payload:        string(payload),
		PayloadVersion: 1,
		State:          domain.JobPending,
		IdempotencyKey: &idempotencyKey,
		NextRunAt:      now,
		SourceEntryID:  &entry.ID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := repos.Jobs.Enqueue(ctx, job); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil
		}
		return fmt.Errorf("openwebui: bridge: enqueue job: %w", err)
	}

	if newBranch {
		link := domain.OpenWebUIConversationLink{
			ID:               linkID,
			ThreadID:         entry.ThreadID,
			BranchID:         branchID,
			WorkspaceID:      workspace.ID,
			ModelID:          model.ID,
			ClaimJobID:       &jobID,
			ClaimedAt:        now,
			LastTransitionAt: now,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := repos.OpenWebUILinks.Claim(ctx, link); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return nil
			}
			return fmt.Errorf("openwebui: bridge: claim link: %w", err)
		}
	}

	turn := domain.OpenWebUITurnLink{
		ID:             turnID,
		LinkID:         linkID,
		BranchID:       branchID,
		LocalMessageID: entry.ID,
		LocalParentID:  entry.ParentEntryID,
		RequestID:      requestID,
		Revision:       1,
		Attempt:        0,
		Status:         domain.TurnPending,
		RemoteParentID: remoteParentID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := repos.OpenWebUITurnLinks.Create(ctx, turn); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil
		}
		return fmt.Errorf("openwebui: bridge: create turn: %w", err)
	}
	return nil
}

// recordAmbiguousModelSelection is EnqueueTurn's response to an owner
// post @mentioning two or more distinct active models at once: rather
// than guess between them, it records — for owner-facing visibility
// only (cmd/openwebuictl, docs/operations/runbook.md) — a link and its
// one turn, both immediately failed with
// domain.FailureCategoryAmbiguousModelSelection, and enqueues no job at
// all, so no provider call is ever made for this post.
//
// A link row requires a model_id (the composite foreign key backing
// openwebui_workspaces.default_model_id, and the roadmap's "a branch
// records the model it was started with", both assume a link is always
// bound to exactly one real model). placeholderModelID — the workspace's
// own configured default model, which by this point is already known to
// exist — fills that requirement; it is never a claim about which model
// the post was "really" for; the link never leaves the failed state, so
// nothing ever reads placeholderModelID back as a decision.
func (b *Bridge) recordAmbiguousModelSelection(ctx context.Context, repos domain.Repos, entry domain.Entry, workspace domain.OpenWebUIWorkspace, placeholderModelID string) error {
	now := b.clock.Now().UTC()
	linkID := domain.NewID()
	branchID := domain.NewID()

	link := domain.OpenWebUIConversationLink{
		ID:               linkID,
		ThreadID:         entry.ThreadID,
		BranchID:         branchID,
		WorkspaceID:      workspace.ID,
		ModelID:          placeholderModelID,
		ClaimedAt:        now,
		LastTransitionAt: now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := repos.OpenWebUILinks.Claim(ctx, link); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil
		}
		return fmt.Errorf("openwebui: bridge: claim link for ambiguous selection: %w", err)
	}
	if err := repos.OpenWebUILinks.MarkFailed(ctx, linkID, domain.FailureCategoryAmbiguousModelSelection, now); err != nil {
		return fmt.Errorf("openwebui: bridge: mark link failed for ambiguous selection: %w", err)
	}

	turnID := domain.NewID()
	turn := domain.OpenWebUITurnLink{
		ID:             turnID,
		LinkID:         linkID,
		BranchID:       branchID,
		LocalMessageID: entry.ID,
		LocalParentID:  entry.ParentEntryID,
		RequestID:      domain.NewID(),
		Revision:       1,
		Attempt:        0,
		Status:         domain.TurnPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := repos.OpenWebUITurnLinks.Create(ctx, turn); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil
		}
		return fmt.Errorf("openwebui: bridge: create turn for ambiguous selection: %w", err)
	}
	category := domain.FailureCategoryAmbiguousModelSelection
	if err := repos.OpenWebUITurnLinks.RecordOutcome(ctx, turnID, domain.TurnOutcomeRecord{
		Status: domain.TurnFailed, FailureCategory: &category,
	}, now); err != nil {
		return fmt.Errorf("openwebui: bridge: record ambiguous selection outcome: %w", err)
	}
	return nil
}

// pathErrorCategory maps a BuildTurnPath error to the category name this
// package logs (never the log line internal/domain records — the bridge
// never writes a turn row for a path it skipped at enqueue time).
func pathErrorCategory(err error) string {
	switch {
	case errors.Is(err, ErrPathCycle):
		return domain.FailureCategoryCycle
	case errors.Is(err, ErrPathOrphan):
		return domain.FailureCategoryOrphan
	case errors.Is(err, ErrPathTooLong):
		return domain.FailureCategoryRequestTooLarge
	case errors.Is(err, ErrPathIneligible):
		return domain.FailureCategoryPathIneligible
	default:
		return "unknown"
	}
}
