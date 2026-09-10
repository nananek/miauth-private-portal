-- Issue #136 Phase 2 (ADR-0010): the third of four planned web admin
-- tables. One table, two states (see plan-136-phase2 §1.3): a row
-- starts 'pending' the moment BeginLogin is called (holding the
-- go-webauthn SessionData for that ceremony, no cookie issued yet) and
-- becomes 'active' the instant FinishLogin succeeds, at which point
-- session_token_hash/csrf_token are set and expires_at is extended from
-- the short ceremony window to ADMIN_SESSION_TTL. credential_id records
-- which of the Owner's registered credentials authenticated this
-- session — NULL while pending, set atomically with the transition to
-- active. revoked_at (not a third status value) mirrors api_tokens'
-- own shape: logout, or a credential revocation cascading onto its
-- sessions (miauthctl web-login revoke-credential), sets it; nothing
-- in this schema ever deletes a session row.
CREATE TABLE web_admin_sessions (
    id                    TEXT PRIMARY KEY,
    owner_actor_id        TEXT NOT NULL REFERENCES actors (id),
    credential_id         TEXT,
    status                TEXT NOT NULL CHECK (status IN ('pending', 'active')),
    session_token_hash    TEXT UNIQUE,
    csrf_token            TEXT,
    webauthn_session_data TEXT,
    created_at            TEXT NOT NULL,
    expires_at            TEXT NOT NULL,
    revoked_at            TEXT
);

-- Backs RequireAdminSession's lookup (WHERE session_token_hash = ? AND
-- status = 'active' AND revoked_at IS NULL AND expires_at > ?) and the
-- credential-revocation cascade's own lookup (WHERE credential_id = ?
-- AND status = 'active' AND revoked_at IS NULL). SQLite's UNIQUE
-- constraint on session_token_hash already tolerates the many NULL
-- rows a 'pending' ceremony leaves (SQLite treats NULLs as distinct
-- for UNIQUE purposes), so no partial index is needed for that column.
CREATE INDEX idx_web_admin_sessions_credential ON web_admin_sessions (credential_id);
