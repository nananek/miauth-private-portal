-- Issue #136 Phase 3 (ADR-0010 Decision 8): the fourth and final planned
-- web admin table. One row per admin action performed through the Web
-- UI (approve/reject a MiAuth session, revoke an API token, reflect a
-- token's scopes) — not written in the same DB transaction as the
-- action itself (see plan-136-phase3 §0/§5 for why: internal/miauth.
-- Service's own methods are each their own transaction boundary and
-- this phase does not modify them), but always written immediately
-- after that call succeeds, best-effort. credential_id identifies
-- which of the Owner's registered WebAuthn credentials (which device)
-- performed the action — the entire reason this table exists at all:
-- once more than one credential can act as "the operator" (Issue
-- #136's whole premise), recording which one did what is worth
-- tracking, unlike ADR-0002's single-SSH-operator CLI model, which
-- never needed this. before_value/after_value are nullable free-form
-- strings, populated only when meaningful for that action (see
-- plan-136-phase3 §1 Decision 6) — never decomposed further, since
-- their shape differs per action type and nothing in this repository
-- ever queries into them.
CREATE TABLE web_admin_action_audit (
    id             TEXT PRIMARY KEY,
    owner_actor_id TEXT NOT NULL REFERENCES actors (id),
    credential_id  TEXT,
    action         TEXT NOT NULL,
    target         TEXT NOT NULL,
    before_value   TEXT,
    after_value    TEXT,
    changed_at     TEXT NOT NULL
);

-- No viewer/list screen exists yet in this phase (plan-136-phase3 §12
-- — a deliberate non-goal, not an oversight), but an index on the
-- action-target pair is cheap now and exactly what a future "audit
-- history for this session/token" screen would need, so it is added
-- alongside the table rather than as a later migration.
CREATE INDEX idx_web_admin_action_audit_target ON web_admin_action_audit (target, changed_at);
