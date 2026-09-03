CREATE TABLE llm_generations (
    id TEXT PRIMARY KEY,
    target_entry_id TEXT NOT NULL REFERENCES entries (id),
    -- Set only once a generation completes; pending/failed attempts never
    -- produce a timeline entry.
    result_entry_id TEXT UNIQUE REFERENCES entries (id),
    kind TEXT NOT NULL CHECK (kind IN ('reply', 'follow_up_question')),
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'complete', 'failed')),
    error_category TEXT,
    body TEXT,
    prompt_tokens INTEGER,
    completion_tokens INTEGER,
    job_id TEXT REFERENCES jobs (id),
    requested_at TEXT NOT NULL,
    generated_at TEXT
);

CREATE INDEX idx_llm_generations_target ON llm_generations (target_entry_id, generated_at);

-- At most one concurrently in-flight generation per (target, kind), so a
-- retried job cannot produce a duplicate reply or follow-up question.
CREATE UNIQUE INDEX idx_llm_generations_pending ON llm_generations (target_entry_id, kind)
    WHERE status = 'pending';
