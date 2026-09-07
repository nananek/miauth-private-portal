package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// This file implements the conversation-link and turn-link repositories
// (migration 0018).
//
// Two properties are the reason this layer exists rather than the
// callers writing their own SQL:
//
//   - Every state change is a compare-and-set whose WHERE clause spells
//     out the states it may be applied from, mirroring
//     domain.LinkTransition. domain.LinkTransition answers the question
//     without a write; these answer it against whatever the row actually
//     says right now, so two racing callers cannot both win.
//   - No statement in this file orders, filters for identity, or
//     authorizes by a remote_* column (ADR-0005 D2). The only remote
//     value any WHERE clause names is GetByRemoteChat's, which exists
//     for owner-driven recovery of an ambiguous link.

type openWebUIConversationLinkRepository struct{ q querier }

const openWebUIConversationLinkSelectColumns = `SELECT id, thread_id, branch_id, workspace_id, model_id,
	state, claim_job_id, remote_chat_id, remote_current_id, failure_category,
	claimed_at, ready_at, last_transition_at, created_at, updated_at
	FROM openwebui_conversation_links`

// Claim inserts the link that represents a branch's single
// remote-chat-creation authorization. The state is written as a literal
// rather than taken from l: a claim always starts pending, and reading
// it from the caller's struct would make "claim a link straight into
// ready" expressible.
//
// The unique (thread_id, branch_id) constraint is what makes this the
// atomic claim — a second attempt on the same branch is domain.
// ErrConflict, never a second link that could create a second remote
// chat.
func (r *openWebUIConversationLinkRepository) Claim(ctx context.Context, l domain.OpenWebUIConversationLink) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO openwebui_conversation_links (id, thread_id, branch_id, workspace_id, model_id,
			state, claim_job_id, claimed_at, last_transition_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'creation_pending', ?, ?, ?, ?, ?)`,
		l.ID, l.ThreadID, l.BranchID, l.WorkspaceID, l.ModelID,
		nullableString(l.ClaimJobID), formatTime(l.ClaimedAt), formatTime(l.LastTransitionAt),
		formatTime(l.CreatedAt), formatTime(l.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *openWebUIConversationLinkRepository) Get(ctx context.Context, id string) (domain.OpenWebUIConversationLink, error) {
	return scanOpenWebUIConversationLink(r.q.QueryRowContext(ctx,
		openWebUIConversationLinkSelectColumns+` WHERE id = ?`, id))
}

func (r *openWebUIConversationLinkRepository) GetByThreadBranch(ctx context.Context, threadID, branchID string) (domain.OpenWebUIConversationLink, error) {
	return scanOpenWebUIConversationLink(r.q.QueryRowContext(ctx,
		openWebUIConversationLinkSelectColumns+` WHERE thread_id = ? AND branch_id = ?`, threadID, branchID))
}

// GetByRemoteChat is the one lookup in this package keyed by a provider
// value. It backs owner-driven recovery — "this remote chat exists; which
// branch, if any, claimed it?" — and is not on any request path. The
// partial unique index over (workspace_id, remote_chat_id) is what makes
// the answer single-valued.
func (r *openWebUIConversationLinkRepository) GetByRemoteChat(ctx context.Context, workspaceID, remoteChatID string) (domain.OpenWebUIConversationLink, error) {
	return scanOpenWebUIConversationLink(r.q.QueryRowContext(ctx,
		openWebUIConversationLinkSelectColumns+` WHERE workspace_id = ? AND remote_chat_id = ?`,
		workspaceID, remoteChatID))
}

func (r *openWebUIConversationLinkRepository) ListByThread(ctx context.Context, threadID string) ([]domain.OpenWebUIConversationLink, error) {
	rows, err := r.q.QueryContext(ctx,
		openWebUIConversationLinkSelectColumns+` WHERE thread_id = ? ORDER BY created_at, id`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []domain.OpenWebUIConversationLink
	for rows.Next() {
		l, err := scanOpenWebUIConversationLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// List returns links matching filter in a stable (created_at, id) order,
// backing owner-facing recovery tooling (Issue #53's openwebuictl)
// rather than any request path — the reason it is a general filter
// rather than another Get-by-something, unlike every other lookup in
// this file.
func (r *openWebUIConversationLinkRepository) List(ctx context.Context, filter domain.OpenWebUILinkFilter) ([]domain.OpenWebUIConversationLink, error) {
	query := openWebUIConversationLinkSelectColumns + ` WHERE 1 = 1`
	var args []any
	if filter.State != nil {
		query += ` AND state = ?`
		args = append(args, string(*filter.State))
	}
	if filter.ThreadID != nil {
		query += ` AND thread_id = ?`
		args = append(args, *filter.ThreadID)
	}
	query += ` ORDER BY created_at, id`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []domain.OpenWebUIConversationLink
	for rows.Next() {
		l, err := scanOpenWebUIConversationLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// MarkReady records a confirmed remote chat, from either a pending link
// (the claimed StartChat succeeded) or an ambiguous one an owner has
// resolved onto the same chat. Every other state is domain.ErrConflict.
//
// Two COALESCEs encode facts about what "ready" means here. ready_at
// keeps its first value, so a link that went ready, became ambiguous,
// and was confirmed again still records when it first had a chat. And a
// nil remoteCurrentID leaves the stored pointer alone rather than
// clearing it: the current-message pointer is only ever recorded once
// the provider reports a turn done (ADR-0005 D3), so "I do not have one
// to give you" must not erase one that was already earned. Clearing it
// deliberately is SetRemoteCurrent's job.
//
// The WHERE clause's remote_chat_id pin is the domain doc comment's
// "refuses to move a link onto a different remote chat than the one it
// already recorded": a link with no remote_chat_id yet accepts any
// value (the ordinary creation-confirmed path), but a link that already
// has one only accepts being confirmed onto that same value again, so
// owner recovery (Issue #53) confirming an ambiguous link cannot
// silently re-point it at a second chat.
func (r *openWebUIConversationLinkRepository) MarkReady(ctx context.Context, id, remoteChatID string, remoteCurrentID *string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_conversation_links
		 SET state = 'ready', remote_chat_id = ?, remote_current_id = COALESCE(?, remote_current_id),
			ready_at = COALESCE(ready_at, ?), last_transition_at = ?, updated_at = ?
		 WHERE id = ? AND state IN ('creation_pending', 'ambiguous')
			AND (remote_chat_id IS NULL OR remote_chat_id = ?)`,
		remoteChatID, nullableString(remoteCurrentID), formatTime(at), formatTime(at), formatTime(at), id, remoteChatID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// MarkAmbiguous freezes a link whose remote outcome is unknown. It
// applies from creation_pending (a lost creation response or an expired
// lease) and from ready (an uncertain continuation), and from nowhere
// else. It records no failure category: an ambiguous link has no known
// outcome to categorise.
func (r *openWebUIConversationLinkRepository) MarkAmbiguous(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_conversation_links
		 SET state = 'ambiguous', last_transition_at = ?, updated_at = ?
		 WHERE id = ? AND state IN ('creation_pending', 'ready')`,
		formatTime(at), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// MarkFailed records a definitive creation failure, which only a pending
// link can suffer: once a chat exists, a failure belongs to the turn and
// leaves the link ready.
//
// failureCategory is a local classification, never provider error text —
// ADR-0005 D6 requires that text be discarded, since the observed
// instance echoes upstream credentials into it verbatim.
func (r *openWebUIConversationLinkRepository) MarkFailed(ctx context.Context, id, failureCategory string, at time.Time) error {
	return r.markTerminal(ctx, id, "failed", "creation_pending", failureCategory, at)
}

// MarkDead records an owner's decision to abandon an ambiguous branch.
// Only an ambiguous link reaches it, and only a person causes it: no
// automatic path may conclude that an uncertain remote chat should be
// given up on.
func (r *openWebUIConversationLinkRepository) MarkDead(ctx context.Context, id, failureCategory string, at time.Time) error {
	return r.markTerminal(ctx, id, "dead", "ambiguous", failureCategory, at)
}

// markTerminal is MarkFailed and MarkDead's shared write. Both terminal
// states are reached from exactly one source state and record exactly
// one category, so the two differ only in which pair of literals they
// name.
func (r *openWebUIConversationLinkRepository) markTerminal(ctx context.Context, id, toState, fromState, failureCategory string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_conversation_links
		 SET state = ?, failure_category = ?, last_transition_at = ?, updated_at = ?
		 WHERE id = ? AND state = ?`,
		toState, failureCategory, formatTime(at), formatTime(at), id, fromState,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// SetRemoteCurrent updates a ready link's opaque current-message
// pointer. It leaves last_transition_at alone: recording a correlation
// value is not a state change, and overwriting the transition timestamp
// with it would make the state machine's history unreadable.
func (r *openWebUIConversationLinkRepository) SetRemoteCurrent(ctx context.Context, id string, remoteCurrentID *string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_conversation_links SET remote_current_id = ?, updated_at = ?
		 WHERE id = ? AND state = 'ready'`,
		nullableString(remoteCurrentID), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func scanOpenWebUIConversationLink(row rowScanner) (domain.OpenWebUIConversationLink, error) {
	var l domain.OpenWebUIConversationLink
	var state, claimedAt, lastTransitionAt, createdAt, updatedAt string
	var claimJobID, remoteChatID, remoteCurrentID, failureCategory, readyAt sql.NullString
	if err := row.Scan(&l.ID, &l.ThreadID, &l.BranchID, &l.WorkspaceID, &l.ModelID,
		&state, &claimJobID, &remoteChatID, &remoteCurrentID, &failureCategory,
		&claimedAt, &readyAt, &lastTransitionAt, &createdAt, &updatedAt,
	); err != nil {
		return domain.OpenWebUIConversationLink{}, mapReadError(err)
	}
	l.State = domain.LinkState(state)
	l.ClaimJobID = stringPtr(claimJobID)
	l.RemoteChatID = stringPtr(remoteChatID)
	l.RemoteCurrentID = stringPtr(remoteCurrentID)
	l.FailureCategory = stringPtr(failureCategory)

	var err error
	if l.ClaimedAt, err = parseTime(claimedAt); err != nil {
		return domain.OpenWebUIConversationLink{}, err
	}
	if l.ReadyAt, err = parseTimePtr(readyAt); err != nil {
		return domain.OpenWebUIConversationLink{}, err
	}
	if l.LastTransitionAt, err = parseTime(lastTransitionAt); err != nil {
		return domain.OpenWebUIConversationLink{}, err
	}
	if l.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.OpenWebUIConversationLink{}, err
	}
	if l.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.OpenWebUIConversationLink{}, err
	}
	return l, nil
}

type openWebUITurnLinkRepository struct{ q querier }

const openWebUITurnLinkSelectColumns = `SELECT id, link_id, branch_id, local_message_id, local_parent_id,
	assistant_entry_id, request_id, revision, attempt, provider_status,
	remote_chat_id, remote_message_id, remote_assistant_message_id, remote_parent_id, remote_current_id,
	failure_category, prompt_tokens, completion_tokens, finish_reason, remote_chat_title, sources_json,
	last_attempt_at, completed_at, tombstoned_at, created_at, updated_at
	FROM openwebui_turn_links`

// Create inserts one turn. Two constraints make it safe to call from a
// retryable path: request_id is unique, and (link_id, local_message_id,
// revision) is the logical turn key, so neither a replayed request nor a
// second recording of the same revision produces a duplicate turn.
func (r *openWebUITurnLinkRepository) Create(ctx context.Context, t domain.OpenWebUITurnLink) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO openwebui_turn_links (id, link_id, branch_id, local_message_id, local_parent_id,
			assistant_entry_id, request_id, revision, attempt, provider_status,
			remote_chat_id, remote_message_id, remote_assistant_message_id, remote_parent_id, remote_current_id,
			tombstoned_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.LinkID, t.BranchID, t.LocalMessageID, nullableString(t.LocalParentID),
		nullableString(t.AssistantEntryID), t.RequestID, t.Revision, t.Attempt, string(t.Status),
		nullableString(t.RemoteChatID), nullableString(t.RemoteMessageID), nullableString(t.RemoteAssistantMessageID),
		nullableString(t.RemoteParentID), nullableString(t.RemoteCurrentID),
		formatTimePtr(t.TombstonedAt), formatTime(t.CreatedAt), formatTime(t.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *openWebUITurnLinkRepository) Get(ctx context.Context, id string) (domain.OpenWebUITurnLink, error) {
	return scanOpenWebUITurnLink(r.q.QueryRowContext(ctx, openWebUITurnLinkSelectColumns+` WHERE id = ?`, id))
}

func (r *openWebUITurnLinkRepository) GetByRequestID(ctx context.Context, requestID string) (domain.OpenWebUITurnLink, error) {
	return scanOpenWebUITurnLink(r.q.QueryRowContext(ctx,
		openWebUITurnLinkSelectColumns+` WHERE request_id = ?`, requestID))
}

// GetByAssistantEntry resolves the turn that authored assistantEntryID.
// The partial unique index migration 0021 adds on assistant_entry_id
// (WHERE assistant_entry_id IS NOT NULL) is what makes this a single-row
// lookup the same way request_id's unique constraint makes GetByRequestID
// one.
func (r *openWebUITurnLinkRepository) GetByAssistantEntry(ctx context.Context, assistantEntryID string) (domain.OpenWebUITurnLink, error) {
	return scanOpenWebUITurnLink(r.q.QueryRowContext(ctx,
		openWebUITurnLinkSelectColumns+` WHERE assistant_entry_id = ?`, assistantEntryID))
}

// ListByLink orders by (created_at, id). The turns of a branch are read
// back in the order they were recorded locally; nothing about a remote
// id, including whether one exists at all, can move a turn in this list.
func (r *openWebUITurnLinkRepository) ListByLink(ctx context.Context, linkID string) ([]domain.OpenWebUITurnLink, error) {
	rows, err := r.q.QueryContext(ctx,
		openWebUITurnLinkSelectColumns+` WHERE link_id = ? ORDER BY created_at, id`, linkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var turns []domain.OpenWebUITurnLink
	for rows.Next() {
		t, err := scanOpenWebUITurnLink(rows)
		if err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// SetRemoteCorrelation replaces all five correlation columns with what
// corr holds, rather than merging field by field. A caller records the
// correlation for one turn as a whole — the client-generated message ids
// it sent, the chat it sent them to, and the pointers the provider
// confirmed — so a partial write here would mean it had lost track of
// values it minted itself.
func (r *openWebUITurnLinkRepository) SetRemoteCorrelation(ctx context.Context, id string, corr domain.OpenWebUITurnCorrelation, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_turn_links
		 SET remote_chat_id = ?, remote_message_id = ?, remote_assistant_message_id = ?,
			remote_parent_id = ?, remote_current_id = ?, updated_at = ?
		 WHERE id = ?`,
		nullableString(corr.RemoteChatID), nullableString(corr.RemoteMessageID),
		nullableString(corr.RemoteAssistantMessageID), nullableString(corr.RemoteParentID),
		nullableString(corr.RemoteCurrentID), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

// SetProviderStatus records a turn's outcome and the attempt it came
// from. Unlike a link's state, a turn's status is a record rather than a
// machine (ADR-0005 D6 classifies it from the chat's stored state), so
// this is a plain write: an unknown id is ErrNotFound, and no prior
// status is required.
func (r *openWebUITurnLinkRepository) SetProviderStatus(ctx context.Context, id string, status domain.TurnProviderStatus, attempt int, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_turn_links SET provider_status = ?, attempt = ?, updated_at = ? WHERE id = ?`,
		string(status), attempt, formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

// BeginAttempt records that a provider attempt for this turn is starting,
// before the call is made: it advances attempt and last_attempt_at in
// the same write, so a crash or lease expiry after this point but
// before a result is known is distinguishable, on the next run, from a
// turn that never attempted anything.
func (r *openWebUITurnLinkRepository) BeginAttempt(ctx context.Context, id string, attempt int, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_turn_links SET attempt = ?, last_attempt_at = ?, updated_at = ? WHERE id = ?`,
		attempt, formatTime(at), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

// RecordOutcome writes a turn's status together with the outcome
// metadata that explains it — failure category, provider usage,
// finish reason — in one statement, so a caller can never update the
// status while leaving a stale category or usage figure from a previous
// attempt behind. completed_at is written only when o.Status.IsTerminal
// (via COALESCE(?, completed_at): the parameter is nil for a non-terminal
// write), and once written is overwritten by any later terminal write
// rather than kept at its first value — unlike a link's ready_at, a
// turn's completion time is meant to reflect its most recent
// determination, including one an owner recovery action changes after an
// earlier ambiguous outcome.
func (r *openWebUITurnLinkRepository) RecordOutcome(ctx context.Context, id string, o domain.TurnOutcomeRecord, at time.Time) error {
	var completedAt any
	if o.Status.IsTerminal() {
		completedAt = formatTime(at)
	}
	var sourcesJSON *string
	if encoded := domain.EncodeSources(o.Sources); encoded != "" {
		sourcesJSON = &encoded
	}
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_turn_links
		 SET provider_status = ?, failure_category = ?, prompt_tokens = ?, completion_tokens = ?,
			finish_reason = ?, remote_chat_title = ?, sources_json = ?, completed_at = COALESCE(?, completed_at), updated_at = ?
		 WHERE id = ?`,
		string(o.Status), nullableString(o.FailureCategory), nullableInt(o.PromptTokens), nullableInt(o.CompletionTokens),
		nullableString(o.FinishReason), nullableString(o.Title), nullableString(sourcesJSON), completedAt, formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *openWebUITurnLinkRepository) SetAssistantEntry(ctx context.Context, id, assistantEntryID string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_turn_links SET assistant_entry_id = ?, updated_at = ? WHERE id = ?`,
		assistantEntryID, formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

// Tombstone marks a turn superseded by a later revision. The WHERE
// clause requires it not already be tombstoned, so a second attempt is
// domain.ErrConflict rather than a silent rewrite of when the first one
// happened. The row itself stays: it is the record of what was sent, and
// the entries it names are untouched (hiding those is ADR-0004's
// separate concern).
func (r *openWebUITurnLinkRepository) Tombstone(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_turn_links SET tombstoned_at = ?, updated_at = ?
		 WHERE id = ? AND tombstoned_at IS NULL`,
		formatTime(at), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func scanOpenWebUITurnLink(row rowScanner) (domain.OpenWebUITurnLink, error) {
	var t domain.OpenWebUITurnLink
	var providerStatus, createdAt, updatedAt string
	var localParentID, assistantEntryID, remoteChatID, remoteMessageID sql.NullString
	var remoteAssistantMessageID, remoteParentID, remoteCurrentID, tombstonedAt sql.NullString
	var failureCategory, finishReason, lastAttemptAt, completedAt sql.NullString
	var remoteChatTitle, sourcesJSON sql.NullString
	var promptTokens, completionTokens sql.NullInt64
	if err := row.Scan(&t.ID, &t.LinkID, &t.BranchID, &t.LocalMessageID, &localParentID,
		&assistantEntryID, &t.RequestID, &t.Revision, &t.Attempt, &providerStatus,
		&remoteChatID, &remoteMessageID, &remoteAssistantMessageID, &remoteParentID, &remoteCurrentID,
		&failureCategory, &promptTokens, &completionTokens, &finishReason, &remoteChatTitle, &sourcesJSON,
		&lastAttemptAt, &completedAt, &tombstonedAt, &createdAt, &updatedAt,
	); err != nil {
		return domain.OpenWebUITurnLink{}, mapReadError(err)
	}
	t.LocalParentID = stringPtr(localParentID)
	t.AssistantEntryID = stringPtr(assistantEntryID)
	t.Status = domain.TurnProviderStatus(providerStatus)
	t.RemoteChatID = stringPtr(remoteChatID)
	t.RemoteMessageID = stringPtr(remoteMessageID)
	t.RemoteAssistantMessageID = stringPtr(remoteAssistantMessageID)
	t.RemoteParentID = stringPtr(remoteParentID)
	t.RemoteCurrentID = stringPtr(remoteCurrentID)
	t.FailureCategory = stringPtr(failureCategory)
	t.PromptTokens = intPtr(promptTokens)
	t.CompletionTokens = intPtr(completionTokens)
	t.FinishReason = stringPtr(finishReason)
	t.RemoteChatTitle = stringPtr(remoteChatTitle)

	var err error
	if sourcesJSON.Valid {
		if t.Sources, err = domain.ParseOpenWebUISources(sourcesJSON.String); err != nil {
			return domain.OpenWebUITurnLink{}, fmt.Errorf("decode turn sources: %w", err)
		}
	}
	if t.LastAttemptAt, err = parseTimePtr(lastAttemptAt); err != nil {
		return domain.OpenWebUITurnLink{}, err
	}
	if t.CompletedAt, err = parseTimePtr(completedAt); err != nil {
		return domain.OpenWebUITurnLink{}, err
	}
	if t.TombstonedAt, err = parseTimePtr(tombstonedAt); err != nil {
		return domain.OpenWebUITurnLink{}, err
	}
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.OpenWebUITurnLink{}, err
	}
	if t.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.OpenWebUITurnLink{}, err
	}
	return t, nil
}
