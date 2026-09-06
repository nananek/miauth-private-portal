-- Issue #52 (OWUI-P): conversation and turn links — the local record of
-- which branch maps to which remote chat, and what was sent for each
-- turn.
--
-- Every remote_* column here is opaque, nullable provider correlation
-- metadata (ADR-0005 D2). None of them is part of a primary key, a
-- unique identity, an ORDER BY, or an authorization check: the local
-- (thread_id, branch_id) pair is the branch's identity and entries'
-- parent_entry_id is the only source of truth for parentage. The one
-- uniqueness constraint on a remote value exists to stop two branches
-- adopting the same remote chat, not to identify anything by it.
--
-- Like 0017, this migration creates empty tables and changes no
-- behaviour.
CREATE TABLE openwebui_conversation_links (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads (id),
    -- A locally minted opaque branch id, not anything the provider
    -- supplied. One local branch maps to one remote chat (ADR-0005 D5).
    branch_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES openwebui_workspaces (id),
    -- The workspace's default model at claim time, recorded so a later
    -- default change cannot rewrite which model a branch was started
    -- with.
    model_id TEXT NOT NULL REFERENCES openwebui_models (id),
    -- The roadmap's state machine. 'unlinked' is deliberately absent: it
    -- means no row exists, and claiming a branch is the INSERT below.
    state TEXT NOT NULL CHECK (state IN ('creation_pending', 'ready', 'ambiguous', 'failed', 'dead')),
    -- The single durable job holding this link's one initial StartChat
    -- authorization. The claim is spent once; no other job may issue it.
    claim_job_id TEXT REFERENCES jobs (id),
    remote_chat_id TEXT,
    -- Recorded only after the provider reports the turn done (ADR-0005
    -- D3): the server advances its currentId onto failed messages too.
    remote_current_id TEXT,
    -- A local category (auth_failed, contract_failed, ...), never
    -- provider error text, which ADR-0005 D6 requires be discarded.
    failure_category TEXT,
    claimed_at TEXT NOT NULL,
    ready_at TEXT,
    last_transition_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- The atomic claim: a second attempt on the same branch collides
    -- here rather than producing a second link that could create a
    -- second remote chat.
    UNIQUE (thread_id, branch_id)
);

-- Partial so that many links may have no remote chat yet (every pending
-- one does not), while no two links in a workspace may claim the same
-- one.
CREATE UNIQUE INDEX idx_openwebui_links_remote_chat
    ON openwebui_conversation_links (workspace_id, remote_chat_id)
    WHERE remote_chat_id IS NOT NULL;

CREATE INDEX idx_openwebui_links_thread
    ON openwebui_conversation_links (thread_id, created_at, id);

CREATE TABLE openwebui_turn_links (
    id TEXT PRIMARY KEY,
    link_id TEXT NOT NULL REFERENCES openwebui_conversation_links (id),
    branch_id TEXT NOT NULL,
    -- The owner's entry and its reply_to_id (NULL at a branch root).
    -- These mirror entries; they never derive parentage from a remote
    -- parent id.
    local_message_id TEXT NOT NULL REFERENCES entries (id),
    local_parent_id TEXT REFERENCES entries (id),
    -- The VirtualActor-authored reply, once the turn produced one.
    assistant_entry_id TEXT REFERENCES entries (id),
    -- A locally generated correlation key. Open WebUI has no
    -- idempotency mechanism (ADR-0005 D7), so single-flight is entirely
    -- this value's job.
    request_id TEXT NOT NULL UNIQUE,
    revision INTEGER NOT NULL DEFAULT 1,
    attempt INTEGER NOT NULL DEFAULT 0,
    provider_status TEXT NOT NULL CHECK (provider_status IN (
        'pending', 'succeeded', 'failed', 'ambiguous', 'contract_failed', 'auth_failed', 'cancelled')),
    remote_chat_id TEXT,
    -- Client-generated UUIDs the server stores verbatim (ADR-0005 D3).
    -- The completion response's own chatcmpl id belongs to the upstream
    -- model provider and is deliberately not recorded anywhere.
    remote_message_id TEXT,
    remote_assistant_message_id TEXT,
    remote_parent_id TEXT,
    remote_current_id TEXT,
    -- Marks a turn superseded by a later revision. Hiding the entries
    -- themselves is ADR-0004's separate concern and is untouched here.
    tombstoned_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- The logical turn key: one row per revision of one local message.
    UNIQUE (link_id, local_message_id, revision)
);

CREATE INDEX idx_openwebui_turn_links_link ON openwebui_turn_links (link_id, created_at, id);
