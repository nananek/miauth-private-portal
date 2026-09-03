CREATE TABLE user_tags (
    -- Internal surrogate key only; never wire-exposed.
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id TEXT NOT NULL REFERENCES entries (id),
    tag TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (entry_id, tag)
);

CREATE INDEX idx_user_tags_tag ON user_tags (tag);

CREATE TABLE llm_classifications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id TEXT NOT NULL REFERENCES entries (id),
    version INTEGER NOT NULL,
    is_active INTEGER NOT NULL DEFAULT 0 CHECK (is_active IN (0, 1)),
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'complete', 'failed')),
    error_category TEXT,
    summary TEXT,
    -- Structured subject/field/keyword/priority/... output; validated in
    -- Go, not by SQLite. See internal/domain.LLMClassification.
    structured_output TEXT,
    prompt_tokens INTEGER,
    completion_tokens INTEGER,
    generated_at TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (entry_id, version)
);

-- At most one active (current) classification version per entry.
CREATE UNIQUE INDEX idx_llm_classifications_active ON llm_classifications (entry_id)
    WHERE is_active = 1;

CREATE TABLE llm_classification_tags (
    classification_id INTEGER NOT NULL REFERENCES llm_classifications (id),
    tag TEXT NOT NULL,
    PRIMARY KEY (classification_id, tag)
);

CREATE TABLE llm_classification_related_entries (
    classification_id INTEGER NOT NULL REFERENCES llm_classifications (id),
    related_entry_id TEXT NOT NULL REFERENCES entries (id),
    PRIMARY KEY (classification_id, related_entry_id)
);
