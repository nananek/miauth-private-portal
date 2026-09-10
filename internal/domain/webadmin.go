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
}
