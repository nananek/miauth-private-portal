-- migrate:rebuild
--
-- Issue #93 PR2 (ADR-0005 D24/D25): make room in
-- openwebui_conversation_links.state for the new terminal outcome a
-- branch reaches when its turn was served through Client.StreamTurn
-- (the native multi-round tool-execution path) instead of StartChat.
-- SQLite cannot change a CHECK list in place, so this is the same
-- table-rebuild migration 0016/0027 already used for actors.actor_type.
--
-- 'stateless' joins the state list. Unlike every other terminal state
-- here (failed, dead), reaching it is success, not failure: no remote
-- chat was ever created, by design, so there is nothing left uncertain
-- for an owner to recover and nothing for a later reply to continue —
-- domain.LinkStateless's own doc comment has the full reasoning.
-- TurnJob.complete is this state's only writer, and only from
-- creation_pending, in the same transaction that records the turn's own
-- succeeded outcome and creates its generated reply.
--
-- Every existing index is recreated unchanged: this migration touches
-- only the CHECK list, none of (thread_id, branch_id), remote_chat_id,
-- or state's own indexed columns.
CREATE TABLE openwebui_conversation_links_new (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads (id),
    branch_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES openwebui_workspaces (id),
    model_id TEXT NOT NULL REFERENCES openwebui_models (id),
    state TEXT NOT NULL CHECK (state IN ('creation_pending', 'ready', 'ambiguous', 'failed', 'dead', 'stateless')),
    claim_job_id TEXT REFERENCES jobs (id),
    remote_chat_id TEXT,
    remote_current_id TEXT,
    failure_category TEXT,
    claimed_at TEXT NOT NULL,
    ready_at TEXT,
    last_transition_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (thread_id, branch_id)
);

INSERT INTO openwebui_conversation_links_new (
    id, thread_id, branch_id, workspace_id, model_id, state, claim_job_id,
    remote_chat_id, remote_current_id, failure_category,
    claimed_at, ready_at, last_transition_at, created_at, updated_at
)
    SELECT
        id, thread_id, branch_id, workspace_id, model_id, state, claim_job_id,
        remote_chat_id, remote_current_id, failure_category,
        claimed_at, ready_at, last_transition_at, created_at, updated_at
    FROM openwebui_conversation_links;

DROP TABLE openwebui_conversation_links;

ALTER TABLE openwebui_conversation_links_new RENAME TO openwebui_conversation_links;

CREATE UNIQUE INDEX idx_openwebui_links_remote_chat
    ON openwebui_conversation_links (workspace_id, remote_chat_id)
    WHERE remote_chat_id IS NOT NULL;

CREATE INDEX idx_openwebui_links_thread
    ON openwebui_conversation_links (thread_id, created_at, id);

CREATE INDEX idx_openwebui_links_state ON openwebui_conversation_links (state, created_at, id);
