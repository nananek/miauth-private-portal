CREATE TABLE owner_bindings (
    -- id is fixed at 1: the singleton row IS the compare-and-set lock. A
    -- second concurrent bind attempt fails on this primary key collision.
    id INTEGER PRIMARY KEY CHECK (id = 1),
    local_actor_id TEXT NOT NULL REFERENCES actors (id),
    identity_origin TEXT NOT NULL,
    upstream_user_id TEXT NOT NULL,
    bound_at TEXT NOT NULL
);

CREATE TABLE upstream_tokens (
    owner_binding_id INTEGER NOT NULL UNIQUE REFERENCES owner_bindings (id) ON DELETE CASCADE,
    ciphertext BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version TEXT NOT NULL,
    created_at TEXT NOT NULL,
    rotated_at TEXT
);
