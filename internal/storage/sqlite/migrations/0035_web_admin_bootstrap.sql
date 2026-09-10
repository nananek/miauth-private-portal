-- Issue #136 Phase 1 (ADR-0010): the admin Web UI's first two of four
-- planned tables (bootstrap tokens now; sessions and action-audit follow
-- in later phases, per ADR-0010's "fifth, structurally distinct
-- credential type" decision — never reusing api_tokens/
-- miauth_local_sessions schema or Go types).

-- web_admin_bootstrap_tokens: a single-use, SSH-issued token
-- (miauthctl web-login issue) whose only capability is completing one
-- WebAuthn registration ceremony for the Owner. Mirrors
-- miauth_local_sessions' created->authorized->consumed shape reduced to
-- the two states this credential actually has: issued->consumed.
-- webauthn_session_data holds the go-webauthn library's own
-- *webauthn.SessionData, JSON-serialized, set by BeginRegistration and
-- read (then implicitly discarded by consumption) by FinishRegistration
-- — kept on this row rather than a separate table since its lifecycle
-- is identical to the bootstrap token's own.
CREATE TABLE web_admin_bootstrap_tokens (
    id                    TEXT PRIMARY KEY,
    token_hash            TEXT NOT NULL UNIQUE,
    owner_actor_id        TEXT NOT NULL REFERENCES actors (id),
    status                TEXT NOT NULL CHECK (status IN ('issued', 'consumed')),
    webauthn_session_data TEXT,
    created_at            TEXT NOT NULL,
    expires_at            TEXT NOT NULL,
    consumed_at           TEXT
);

-- web_admin_credentials: one row per registered WebAuthn credential,
-- bound to the singleton Owner actor (never a new login-capable actor
-- type — ADR-0010 Decision 6). credential_id is the WebAuthn credential
-- ID (base64url, the library's own Credential.ID []byte), unique so a
-- given authenticator credential can never be double-registered.
-- credential_json is the full go-webauthn *webauthn.Credential struct,
-- JSON-serialized as-is (it already carries json tags for every field:
-- PublicKey, AttestationType/Format, Transport, Flags, Authenticator,
-- Attestation, Extensions) rather than decomposed into individual
-- columns — this avoids this schema drifting out of sync with the
-- library's own struct shape across upgrades; nothing in this
-- repository queries into its interior, only round-trips it whole via
-- Go's json package.
CREATE TABLE web_admin_credentials (
    id             TEXT PRIMARY KEY,
    owner_actor_id TEXT NOT NULL REFERENCES actors (id),
    credential_id  TEXT NOT NULL UNIQUE,
    credential_json TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    last_used_at   TEXT
);

CREATE INDEX idx_web_admin_credentials_owner ON web_admin_credentials (owner_actor_id);
