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
// Steps (plan §5.1): (1) restrict to a loginable author's user_post; (2)
// skip a post whose provider-facing text — entry.Body with every
// @mention omitted (Issue #70) — is empty or whitespace-only, since
// that is nothing for the provider to reply to; (3) require an enabled,
// generation-enabled workspace with an active default model and the one
// known secret_ref; (4) build entry's reply-tree path, skipping (not
// failing) the post when it is ineligible for this bridge; (5) apply
// ADR-0005 D4's branch rule; (6) enqueue the job and claim the link (new
// branch) or reuse it (continuation); (7) treat a conflict on any of
// those writes as "this turn is already enqueued" rather than an error,
// so a retried delivery of the same entry-creation path never
// double-enqueues.
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
	if strings.TrimSpace(stripMentionTagsForProvider(entry.Body)) == "" {
		// Nothing but @mentions (and/or whitespace): once the mentions
		// are omitted for the provider (Issue #70), there is no text
		// left to send as this turn's own message. Skip quietly, the
		// same as every other enqueue-time gate below — this is not a
		// failure, just nothing to start a turn over.
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
	model, err := repos.OpenWebUIModels.Get(ctx, *workspace.DefaultModelID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("openwebui: bridge: get default model: %w", err)
	}
	if !model.Active {
		return nil
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

	continuation, err := SelectBranch(ctx, repos, entry)
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
