CREATE TABLE threads (
    -- A thread's id always equals its root entry's id (enforced by the
    -- entries.CHECK below), so no circular foreign key is needed between
    -- the two tables.
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE entries (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads (id),
    parent_entry_id TEXT REFERENCES entries (id),
    kind TEXT NOT NULL CHECK (
        kind IN ('user_post', 'llm_reply', 'llm_follow_up', 'news', 'mail', 'system')
    ),
    author_actor_id TEXT NOT NULL REFERENCES actors (id),
    -- User-authored or ingestion-provenance source text. LLM
    -- classification/summary/tag data lives in llm_classifications and
    -- must never overwrite this column.
    body TEXT NOT NULL,
    processing_status TEXT NOT NULL DEFAULT 'none' CHECK (
        processing_status IN ('none', 'pending', 'processing', 'complete', 'failed')
    ),
    archived_at TEXT,
    hidden_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- An entry is a thread root (no parent) if and only if its id equals
    -- its thread_id.
    CHECK ((parent_entry_id IS NULL) = (id = thread_id))
);

CREATE INDEX idx_entries_thread_created ON entries (thread_id, created_at, id);
CREATE INDEX idx_entries_parent ON entries (parent_entry_id);
CREATE INDEX idx_entries_timeline_all ON entries (created_at, id);

-- Covers the default (non-archived, non-hidden) timeline listing without
-- scanning archived/hidden rows.
CREATE INDEX idx_entries_timeline_default ON entries (created_at, id)
    WHERE archived_at IS NULL AND hidden_at IS NULL;
