-- Issue #76 (ADR-0006): DB-backed runtime configuration overlay for a
-- fixed subset of internal/config's known keys (internal/config's own
-- db-eligible classification, internal/config.IsDBEligibleKey). A row
-- here overrides config.Load's file/env/default value for that key; an
-- absent row means the bootstrap value applies unchanged, exactly as it
-- did before this migration. Secret (ADR-0005 D10) and bootstrap-only
-- keys — including network-destination settings like
-- OPENWEBUI_BASE_URL/IMAP_HOST (ADR-0006) — never get a row here at
-- all: there is no column to hold one, so "cannot be stored" is a
-- schema fact rather than a runtime check a caller must remember.
--
-- key is the exact internal/config.Key* constant string (for example
-- "JOBS_POLL_INTERVAL"). value is the same raw text representation an
-- environment variable would carry for that key, parsed by the same
-- internal/config.ValidateKeyValue every write path shares — this table
-- deliberately has no separate type/description/secret-flag column,
-- since internal/config is already the single source of truth for all
-- three.
--
-- version starts at 1 on the row's first insert and increments by one on
-- every subsequent write, backing domain.ConfigRepository.Set's
-- compare-and-set semantics (ADR-0006's optimistic concurrency control):
-- a caller supplies the version it last read, and a write whose row has
-- since moved to a different version — including one since deleted by
-- an intervening Unset — fails with domain.ErrConflict rather than
-- silently overwriting a change it never saw.
CREATE TABLE app_config (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL
);

-- app_config_audit is app_config's immutable, append-only change log:
-- one row per Set/Unset (including the startup auto-seed path's
-- first-time writes), recording the value transition and which version
-- it produced. Every write here happens in the same transaction as the
-- app_config write it describes (domain.UnitOfWork.WithinTx), so a
-- change and its audit trail can never disagree.
--
-- old_value/new_value are both nullable: old_value is NULL for a key's
-- first-ever row (nothing to have changed from), new_value is NULL for
-- an Unset (the row is removed, reverting to the bootstrap value).
--
-- version is 0 for an Unset event (app_config has no row afterward, so
-- there is no new row version to record) and otherwise mirrors the
-- app_config row's own version immediately after this write. It is
-- deliberately NOT this table's ordering column: a key deleted and
-- later re-Set restarts app_config's own version at 1, so two audit
-- rows for the same key can legitimately share a version number across
-- such a delete/recreate cycle. changed_at is what miauthctl config
-- rollback/history order by; version is carried only so a specific
-- historical value can still be named ("rollback --to-version N" finds
-- the most recent audit row at that version, per ConfigAuditRepository's
-- own doc comment).
CREATE TABLE app_config_audit (
    id         TEXT PRIMARY KEY,
    key        TEXT NOT NULL,
    old_value  TEXT,
    new_value  TEXT,
    version    INTEGER NOT NULL,
    changed_at TEXT NOT NULL,
    changed_by TEXT NOT NULL
);

-- Backs domain.ConfigAuditRepository.ListByKey (miauthctl config
-- history), ordered by (changed_at, id) — the same "real time, not a
-- recyclable sequence number" ordering this service's other list
-- methods already use (for example openwebui_models' (created_at, id)).
CREATE INDEX idx_app_config_audit_key ON app_config_audit (key, changed_at);
