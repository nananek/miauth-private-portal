-- migrate:rebuild
--
-- Issue #52 (OWUI-P): make room in actors for Open WebUI VirtualActor
-- rows. 0001_actors.sql pinned actor_type to a three-value CHECK and a
-- table-level UNIQUE(actor_type); SQLite can change neither in place, so
-- this is the twelve-step table rebuild the -- migrate:rebuild directive
-- above exists for (see rebuildDirective in migrate.go).
--
-- Two things change:
--
--   * 'openwebui_model' joins the CHECK list. A row of that type is a
--     presentation actor for one Open WebUI model, projected to Aria as
--     @<slug>@<presentation_host> (docs/roadmap/openwebui.md, ADR-0005).
--     It is never login-capable: domain.Actor's IsLoginable/CanMiAuth
--     predicates are derived from actor_type, and internal/miauth only
--     ever binds sessions and tokens to the owner.
--   * UNIQUE(actor_type) becomes a partial unique index covering only
--     owner/assistant/system. Those three stay singletons (a second
--     owner is still domain.ErrConflict), while openwebui_model may have
--     one row per model.
--
-- No row is created here. Until Issue #52's later PRs wire the registry
-- up behind OPENWEBUI_ENABLED, the actors table still holds exactly the
-- same three rows it did before and every projection is unchanged.
CREATE TABLE actors_new (
    id TEXT PRIMARY KEY,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('owner', 'assistant', 'system', 'openwebui_model')),
    created_at TEXT NOT NULL,
    display_name TEXT
);

INSERT INTO actors_new (id, actor_type, created_at, display_name)
    SELECT id, actor_type, created_at, display_name FROM actors;

DROP TABLE actors;

ALTER TABLE actors_new RENAME TO actors;

CREATE UNIQUE INDEX idx_actors_singleton_type ON actors (actor_type)
    WHERE actor_type IN ('owner', 'assistant', 'system');
