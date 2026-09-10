-- Issue #133: api_token_scope_audit is api_tokens.scopes' immutable,
-- append-only change log for miauthctl tokens reflect-scopes — the
-- api_tokens-column equivalent of app_config_audit (migration 0022,
-- ADR-0006). One row per ReflectScopes write, in the same transaction as
-- the api_tokens UPDATE it describes, so a change and its audit trail can
-- never disagree. Only a genuine scope change is ever recorded here (a
-- reflect-scopes run that finds nothing to add writes nothing) — see
-- internal/miauth.Service.ReflectScopes.
CREATE TABLE api_token_scope_audit (
    id         TEXT PRIMARY KEY,
    token_id   TEXT NOT NULL REFERENCES api_tokens (id),
    old_scopes TEXT NOT NULL,
    new_scopes TEXT NOT NULL,
    changed_at TEXT NOT NULL,
    changed_by TEXT NOT NULL
);

-- Backs domain.APITokenScopeAuditRepository.ListByToken, ordered by
-- (changed_at, id) — same convention as idx_app_config_audit_key.
CREATE INDEX idx_api_token_scope_audit_token ON api_token_scope_audit (token_id, changed_at);
