-- Issue #115 PR1: user_lists and user_list_members back the
-- Misskey-compatible users/lists/* CRUD and notes/user-list-timeline
-- (internal/userlist.Service). This service is single-owner (AGENTS.md),
-- so a list's owner is always the owner actor — unlike real Misskey
-- there is no owner column here, since every row would carry the same
-- value (plan-115 §0/§1.2).
CREATE TABLE user_lists (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    -- Persisted so users/lists/update's isPublic field round-trips, but
    -- has no other effect: real Misskey's public list page is a
    -- federation-facing feature this non-federating deployment does not
    -- implement (Issue #115 Non-goals).
    is_public INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- actor_id REFERENCES actors(id) with no ON DELETE behavior, like
-- reactions/mentions before it: this service never hard-deletes an actor
-- row (owner/assistant/system are singletons; an Open WebUI model actor
-- is only ever deactivated, never removed — internal/openwebui), so a
-- plain foreign key needs no cascade or SET NULL to stay valid.
--
-- list_id also has no ON DELETE CASCADE, mirroring entry_files' own
-- "no implicit cascade" precedent (0031_entry_files.sql): unlike that
-- table's attached-file safety concern, deleting a list's memberships
-- alongside the list itself is always the wanted behavior, so
-- userListRepository.Delete removes membership rows explicitly before
-- removing the list row, rather than relying on SQLite to cascade it.
CREATE TABLE user_list_members (
    list_id TEXT NOT NULL REFERENCES user_lists (id),
    actor_id TEXT NOT NULL REFERENCES actors (id),
    added_at TEXT NOT NULL,
    PRIMARY KEY (list_id, actor_id)
);

-- Backs MemberActorIDs' added-order read.
CREATE INDEX idx_user_list_members_list_id ON user_list_members (list_id, added_at);
