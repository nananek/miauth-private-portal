CREATE TABLE bootstrap_gates (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('issued', 'consumed', 'expired', 'revoked', 'failed')),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);

CREATE TABLE miauth_local_sessions (
    -- route_session_id is Aria's opaque bearer correlation secret; it is
    -- this record's own primary key, not a separate internal ID.
    route_session_id TEXT PRIMARY KEY,
    state TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('created', 'authorized', 'consumed', 'expired', 'denied')),
    requested_permissions TEXT NOT NULL,
    client_callback TEXT,
    local_actor_id TEXT REFERENCES actors (id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    authorized_at TEXT,
    consumed_at TEXT
);

CREATE TABLE miauth_upstream_sessions (
    id TEXT PRIMARY KEY,
    local_session_id TEXT UNIQUE REFERENCES miauth_local_sessions (route_session_id),
    bootstrap_gate_id TEXT UNIQUE REFERENCES bootstrap_gates (id),
    identity_origin TEXT NOT NULL,
    state TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('created', 'authorized', 'consumed', 'expired', 'denied')),
    upstream_user_id TEXT,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    authorized_at TEXT,
    consumed_at TEXT,
    -- Bound to exactly one of the normal Aria-triggered flow (a local
    -- session) or the operator bootstrap flow (a bootstrap gate), never
    -- both and never neither.
    CHECK ((local_session_id IS NULL) != (bootstrap_gate_id IS NULL))
);

CREATE TABLE api_tokens (
    id TEXT PRIMARY KEY,
    -- Only the one-way hash is stored; the raw token is never persisted.
    token_hash TEXT NOT NULL UNIQUE,
    local_actor_id TEXT NOT NULL REFERENCES actors (id),
    miauth_local_session_id TEXT REFERENCES miauth_local_sessions (route_session_id),
    scopes TEXT NOT NULL,
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    last_used_at TEXT
);

CREATE INDEX idx_api_tokens_local_actor ON api_tokens (local_actor_id);
