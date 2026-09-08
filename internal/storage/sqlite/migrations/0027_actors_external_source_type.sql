-- migrate:rebuild
--
-- Issue #77 PR4: make room in actors for one row per RSS-kind
-- domain.ExternalSource (the "1 external identity = 1 actor row"
-- pattern Issue #52 set for Open WebUI models, ADR-0008). SQLite cannot
-- change a CHECK list in place, so this is the same table-rebuild
-- migration 0016 already used to add 'openwebui_model'.
--
-- 'external_source' joins the CHECK list. A row of that type is a
-- presentation actor for one RSS feed, projected to Aria as
-- @<username>@<host> where host is that feed's own real origin
-- (ADR-0008 records why, and the "Revisit if" condition that would
-- invalidate the decision). It is never login-capable: domain.Actor's
-- IsLoginable/CanMiAuth predicates are derived from actor_type, and
-- internal/miauth only ever binds sessions and tokens to the owner.
--
-- The partial unique index over the three singleton types is
-- unaffected: 'external_source', like 'openwebui_model' before it, may
-- have many rows.
CREATE TABLE actors_new (
    id TEXT PRIMARY KEY,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('owner', 'assistant', 'system', 'openwebui_model', 'external_source')),
    created_at TEXT NOT NULL,
    display_name TEXT
);

INSERT INTO actors_new (id, actor_type, created_at, display_name)
    SELECT id, actor_type, created_at, display_name FROM actors;

DROP TABLE actors;

ALTER TABLE actors_new RENAME TO actors;

CREATE UNIQUE INDEX idx_actors_singleton_type ON actors (actor_type)
    WHERE actor_type IN ('owner', 'assistant', 'system');
