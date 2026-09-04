CREATE TABLE external_sources (
    id TEXT PRIMARY KEY,
    -- Open string, not a CHECK enum: see jobs.job_type above for the same
    -- rationale (adapter registry lives in Go, e.g. Issue #11/#12).
    kind TEXT NOT NULL,
    uri TEXT NOT NULL,
    display_name TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (kind, uri)
);

CREATE TABLE external_items (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES external_sources (id),
    external_id TEXT NOT NULL,
    provenance_url TEXT,
    published_at TEXT,
    fetched_at TEXT NOT NULL,
    -- Content-hash fallback for sources whose external_id is unstable or
    -- absent.
    dedupe_key TEXT NOT NULL UNIQUE,
    entry_id TEXT UNIQUE REFERENCES entries (id),
    created_at TEXT NOT NULL,
    UNIQUE (source_id, external_id)
);

CREATE INDEX idx_external_items_source_fetched ON external_items (source_id, fetched_at);
