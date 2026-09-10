package domain

import (
	"context"
	"time"
)

// WebAdminBootstrapStatus is web_admin_bootstrap_tokens' two-state
// machine (Issue #136 Phase 1, ADR-0010): issued -> consumed. Unlike
// LocalMiAuthSession there is no "authorized" middle state — a bootstrap
// token's only action (complete one WebAuthn registration) is one
// atomic step, not a separate approve-then-consume pair, because
// there's no separate approval step to model: presenting host-issued
// possession of the raw token (mirroring the local MiAuth route session
// ID's bearer-capability role) already required SSH access.
type WebAdminBootstrapStatus string

const (
	WebAdminBootstrapIssued   WebAdminBootstrapStatus = "issued"
	WebAdminBootstrapConsumed WebAdminBootstrapStatus = "consumed"
)

// WebAdminBootstrapToken is a single-use, SSH-issued token whose only
// capability is completing one WebAuthn registration ceremony for the
// Owner (ADR-0010 Decisions 2-3). WebAuthnSessionData holds the
// go-webauthn library's own session state (JSON) between
// BeginRegistration and FinishRegistration; nil until BeginRegistration
// sets it.
type WebAdminBootstrapToken struct {
	ID                  string
	TokenHash           string
	OwnerActorID        string
	Status              WebAdminBootstrapStatus
	WebAuthnSessionData *string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
}

// WebAdminBootstrapTokenRepository persists Issue #136's bootstrap
// tokens.
type WebAdminBootstrapTokenRepository interface {
	Create(ctx context.Context, t WebAdminBootstrapToken) error
	// GetByTokenHash looks a token up for validation (BeginRegistration
	// re-validates on every ceremony step: exists, status=issued,
	// unexpired — see internal/webadmin.Service's doc comments).
	GetByTokenHash(ctx context.Context, tokenHash string) (WebAdminBootstrapToken, error)
	// SetSessionData stores sessionData (JSON) against id, guarded by
	// status='issued' AND expires_at > at — the same "only touch a row
	// still in its expected state" pattern
	// LocalMiAuthSessionRepository.Authorize uses. Returns ErrConflict
	// (not ErrNotFound) on a zero-row match, mirroring
	// LocalMiAuthSessionRepository.Authorize's own
	// requireRowAffectedConflict choice: a token that raced to expiry or
	// consumption between GetByTokenHash and this call is a conflict,
	// not a lookup failure.
	SetSessionData(ctx context.Context, id, sessionData string, at time.Time) error
	// Consume atomically transitions id from issued to consumed
	// (guarded by status='issued' AND expires_at > at, RETURNING the
	// updated row) — the same UPDATE...RETURNING/ErrConflict-on-zero-rows
	// shape LocalMiAuthSessionRepository.Consume already establishes.
	// FinishRegistration calls this only after the WebAuthn library's own
	// FinishRegistration call has already succeeded, inside the same
	// transaction as the new WebAdminCredential's Create (see
	// internal/webadmin.Service.FinishRegistration).
	Consume(ctx context.Context, id string, at time.Time) (WebAdminBootstrapToken, error)
}

// WebAdminCredential is one registered WebAuthn credential, bound to the
// singleton Owner actor (ADR-0010 Decision 6 — never a new login-capable
// actor type). CredentialID is the WebAuthn credential ID, base64url-
// encoded. CredentialJSON is the go-webauthn library's own
// *webauthn.Credential struct, JSON round-tripped whole (see migration
// 0035's own comment for why this isn't decomposed into columns).
type WebAdminCredential struct {
	ID             string
	OwnerActorID   string
	CredentialID   string
	CredentialJSON string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

// WebAdminCredentialRepository persists Issue #136's registered WebAuthn
// credentials.
type WebAdminCredentialRepository interface {
	Create(ctx context.Context, c WebAdminCredential) error
	// ListByOwner returns every credential registered to ownerActorID —
	// used both to populate webauthn.User.WebAuthnCredentials() (so a
	// second registration ceremony's exclude-list keeps the same
	// authenticator from double-registering) and, in Phase 2, for login
	// credential lookup. Phase 1 has exactly one caller
	// (internal/webadmin.Service.BeginRegistration); Phase 2 adds the
	// login lookup on top without needing a new repository method.
	ListByOwner(ctx context.Context, ownerActorID string) ([]WebAdminCredential, error)
	// UpdateAfterLogin persists FinishLogin's returned *webauthn.Credential
	// (re-encoded as CredentialJSON by the caller) and LastUsedAt together,
	// keyed by credentialID (the base64url WebAuthn credential ID — the
	// library's own documented primary lookup key at login). This is not
	// optional bookkeeping: go-webauthn's own package documentation
	// requires SignCount/CloneWarning/UserVerified/BackupState to be
	// written back after every successful login so the next ceremony
	// observes current values — see internal/webadmin.Service.FinishLogin's
	// doc comment for the full citation.
	UpdateAfterLogin(ctx context.Context, credentialID, credentialJSON string, at time.Time) error
	// Delete removes id's credential row permanently (miauthctl web-login
	// revoke-credential). Unlike WebAdminBootstrapToken/WebAdminSession,
	// a credential registration has no "revoked but kept for history"
	// state to preserve — Phase 3's web_admin_action_audit table (not
	// yet built) is the only place CLI-vs-Web-UI action history will
	// ever live, and CLI actions don't get one either way (ADR-0010
	// Decision 8's own scoping note). Returns domain.ErrNotFound if id
	// does not exist.
	Delete(ctx context.Context, id string) error
	// Get looks up one credential by its own row ID (miauthctl web-login
	// revoke-credential resolves the operator-supplied id argument to a
	// CredentialID before calling Delete + the session-revocation
	// cascade — see cmd/miauthctl's webLoginRevokeCredential).
	Get(ctx context.Context, id string) (WebAdminCredential, error)
}

// WebAdminSessionStatus is web_admin_sessions' two-state machine (Issue
// #136 Phase 2, ADR-0010): pending -> active. A row never has a third
// state; expiry and revocation are read off ExpiresAt/RevokedAt, not a
// stored status value — see this type's own package-level doc comment
// for why (mirrors WebAdminBootstrapStatus's identical reasoning).
type WebAdminSessionStatus string

const (
	WebAdminSessionPending WebAdminSessionStatus = "pending"
	WebAdminSessionActive  WebAdminSessionStatus = "active"
)

// WebAdminSession is a login ceremony (while Status == WebAdminSessionPending)
// or an authenticated browser session (once WebAdminSessionActive) for the
// Owner (ADR-0010 Decisions 4-5). Its own row ID is the bearer correlation
// value threaded through BeginLogin/FinishLogin's two HTTP calls — the
// same "no separate ceremony cookie" pattern Phase 1 established for
// bootstrap-token registration (see internal/webadmin.Service.BeginLogin's
// doc comment).
type WebAdminSession struct {
	ID                  string
	OwnerActorID        string
	CredentialID        *string // nil while pending; set atomically with the pending->active transition
	Status              WebAdminSessionStatus
	SessionTokenHash    *string // nil while pending
	CSRFToken           *string // nil while pending
	WebAuthnSessionData *string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	RevokedAt           *time.Time
}

// WebAdminSessionRepository persists Issue #136 Phase 2's login
// ceremonies and the sessions they produce.
type WebAdminSessionRepository interface {
	// Create starts a new pending session (BeginLogin).
	Create(ctx context.Context, s WebAdminSession) error
	// Get looks a session up by its own row ID — the correlation value
	// threaded through the login ceremony's two HTTP calls (mirrors
	// WebAdminBootstrapTokenRepository.GetByTokenHash's role, keyed
	// differently since there is no separate raw/hash pair here: the
	// row ID itself is never sent to the browser as a bearer secret,
	// only used server-side to correlate BeginLogin -> FinishLogin
	// within the same short ceremony window).
	Get(ctx context.Context, id string) (WebAdminSession, error)
	// Activate atomically transitions id from pending to active
	// (guarded by status='pending' AND expires_at > at, RETURNING the
	// updated row), setting credentialID/sessionTokenHash/csrfToken and
	// extending expiresAt to the real ADMIN_SESSION_TTL window — the
	// same UPDATE...RETURNING/ErrConflict-on-zero-rows shape
	// WebAdminBootstrapTokenRepository.Consume already establishes.
	// FinishLogin calls this only after the WebAuthn library's own
	// FinishLogin call has already succeeded.
	Activate(ctx context.Context, id, credentialID, sessionTokenHash, csrfToken string, expiresAt, at time.Time) (WebAdminSession, error)
	// GetActiveBySessionTokenHash is RequireAdminSession's lookup:
	// status='active' AND revoked_at IS NULL AND expires_at > at,
	// returns ErrNotFound (mapped to a generic auth failure by the
	// caller, never surfaced as a distinct case) otherwise.
	GetActiveBySessionTokenHash(ctx context.Context, sessionTokenHash string, at time.Time) (WebAdminSession, error)
	// Revoke sets revoked_at on id (logout). A no-op (success) if
	// already revoked or expired — logout must never error just
	// because the session was already gone by the time it ran.
	Revoke(ctx context.Context, id string, at time.Time) error
	// RevokeAllByCredential sets revoked_at on every currently-active,
	// unrevoked session whose CredentialID is credentialID — the
	// cascade §1 Decision 6 requires when a credential is revoked.
	// Returns the number of sessions revoked (for the CLI's own output,
	// e.g. "revoked credential X and its 2 active session(s)").
	RevokeAllByCredential(ctx context.Context, credentialID string, at time.Time) (int, error)
}

// WebAdminAction names one of the four action kinds Issue #136 Phase
// 3's handlers record. A plain string, not a CHECK-constrained column
// (unlike WebAdminBootstrapStatus/WebAdminSessionStatus's two-value
// state machines): this set may grow in a later phase (e.g. RSS feed
// add/remove) without a schema change, and nothing queries by exact
// action value today beyond storing/displaying it.
type WebAdminAction string

const (
	WebAdminActionApproveSession WebAdminAction = "approve_session"
	WebAdminActionRejectSession  WebAdminAction = "reject_session"
	WebAdminActionRevokeToken    WebAdminAction = "revoke_token"
	WebAdminActionReflectScopes  WebAdminAction = "reflect_scopes"
	// WebAdminActionAddRSSFeed/WebAdminActionRemoveRSSFeed are Issue
	// #136 Phase 4's two action kinds, added to the set
	// WebAdminAction's own doc comment already anticipated growing
	// (Phase 3) — no schema change (action is a plain TEXT column).
	WebAdminActionAddRSSFeed    WebAdminAction = "add_rss_feed"
	WebAdminActionRemoveRSSFeed WebAdminAction = "remove_rss_feed"
)

// WebAdminActionAuditEntry is one row of the api_token_scope_audit-style
// change log Issue #136 Phase 3 adds for Web-UI-originated admin actions
// (ADR-0010 Decision 8). CredentialID is nil only defensively (an active
// WebAdminSession always has one set — see WebAdminSession.CredentialID's
// own doc comment — this field's nilability exists so a future action
// performed by something other than a browser session, if one is ever
// added, is representable without a schema change, not because today's
// callers ever leave it unset).
type WebAdminActionAuditEntry struct {
	ID           string
	OwnerActorID string
	CredentialID *string
	Action       WebAdminAction
	Target       string
	BeforeValue  *string
	AfterValue   *string
	ChangedAt    time.Time
}

// WebAdminActionAuditRepository persists Issue #136 Phase 3's admin
// action audit trail. Record-only for this phase — no List method yet;
// a future audit-viewer screen would add one then rather than
// speculatively now.
type WebAdminActionAuditRepository interface {
	Record(ctx context.Context, entry WebAdminActionAuditEntry) error
}
