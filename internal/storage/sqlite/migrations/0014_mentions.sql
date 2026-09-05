-- Issue #23 PR5: POST /api/notes/mentions detects @username self-mentions
-- in user_post bodies at creation time (see plan-issue-23's PR5 section
-- and docs/compat/aria-v1.5.11.md's "POST /api/notes/mentions" section).
-- Detection is forward-only: pre-existing entries created before this
-- migration applies are never scanned or backfilled (owner-confirmed
-- scope, 2026-09-06).
CREATE TABLE mentions (
    id TEXT PRIMARY KEY,
    entry_id TEXT NOT NULL REFERENCES entries (id),
    mentioned_actor_id TEXT NOT NULL REFERENCES actors (id),
    created_at TEXT NOT NULL
);

-- Backs ListEntriesByMentionedActor's join+filter+order (per-actor,
-- newest-first).
CREATE INDEX idx_mentions_actor ON mentions (mentioned_actor_id, created_at, id);
