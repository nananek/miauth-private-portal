-- Issue #23 PR4: POST /api/notes/reactions/create, /delete, and the
-- "who reacted" list POST /api/notes/reactions (see
-- docs/compat/aria-v1.5.11.md's correction that the list endpoint's wire
-- path is notes/reactions, not notes/reactions/list as plan-issue-23
-- originally assumed).
CREATE TABLE reactions (
    id TEXT PRIMARY KEY,
    entry_id TEXT NOT NULL REFERENCES entries (id),
    reactor_actor_id TEXT NOT NULL REFERENCES actors (id),
    emoji TEXT NOT NULL,
    created_at TEXT NOT NULL,
    -- A Misskey account has at most one reaction per note; changing it is
    -- expressed as a delete followed by a create (see
    -- domain.ReactionRepository's doc comment), never a second row for
    -- the same pair.
    UNIQUE (entry_id, reactor_actor_id)
);

-- Backs both CountsByEmoji/ListByEntry (per-entry, newest-first) and the
-- UNIQUE constraint above.
CREATE INDEX idx_reactions_entry_id ON reactions (entry_id, created_at, id);
