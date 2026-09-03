CREATE TABLE actors (
    id TEXT PRIMARY KEY,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('owner', 'assistant', 'system')),
    created_at TEXT NOT NULL,
    UNIQUE (actor_type)
);
