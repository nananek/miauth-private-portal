CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    -- Open string, not a CHECK enum: the job-type registry lives in Go
    -- (Issue #8), so adding a job type never requires a migration.
    job_type TEXT NOT NULL,
    payload TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1,
    state TEXT NOT NULL CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'dead')),
    attempt INTEGER NOT NULL DEFAULT 0,
    idempotency_key TEXT UNIQUE,
    next_run_at TEXT NOT NULL,
    lease_owner TEXT,
    lease_expires_at TEXT,
    last_error TEXT,
    source_entry_id TEXT REFERENCES entries (id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_jobs_claimable ON jobs (next_run_at) WHERE state = 'pending';
CREATE INDEX idx_jobs_lease ON jobs (lease_expires_at) WHERE state = 'running';
CREATE INDEX idx_jobs_type_state ON jobs (job_type, state);
