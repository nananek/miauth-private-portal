// Package webadmin implements Issue #136's admin Web UI auth (ADR-0010).
// Phase 1 issues a single-use, SSH-anchored bootstrap token via
// miauthctl web-login and uses it exactly once to register one WebAuthn
// credential for the Owner. Phase 2 adds the WebAuthn login ceremony
// against that registered credential, the session cookie/CSRF token it
// produces, logout, and miauthctl web-login list-credentials/
// revoke-credential.
package webadmin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// bootstrapTokenTTL matches internal/miauth's localSessionTTL exactly —
// no reason for this credential to live longer, and this codebase
// already has that precedent for "a short-lived, SSH-issued, single-use
// credential" (ADR-0010 Decision 2).
const bootstrapTokenTTL = 10 * time.Minute

var (
	// ErrBootstrapTokenInvalid is returned for any reason a bootstrap
	// token can't be used — unknown, expired, or already consumed — as
	// one generic error. Callers (the HTTP handlers) must not
	// distinguish these to the caller, mirroring RequireScope's own
	// "unknown token, revoked, or wrong scope are all indistinguishable"
	// precedent: telling an attacker *why* a token failed narrows their
	// search space for free.
	ErrBootstrapTokenInvalid = errors.New("webadmin: bootstrap token is invalid, expired, or already used")
	// ErrCeremonyFailed wraps any go-webauthn library failure during
	// Begin/FinishRegistration or BeginLogin/FinishLogin (malformed
	// response, challenge mismatch, origin mismatch, signature
	// verification failure, ...) as one category for the same reason.
	// Named for both ceremony types (registration and login) it covers,
	// not just registration — see plan-136-phase2 §5's rename note.
	ErrCeremonyFailed = errors.New("webadmin: WebAuthn ceremony failed")
	// ErrSessionInvalid is RequireAdminSession's one generic failure
	// category — unknown session, expired, revoked, or a login
	// ceremony never completed — mirroring ErrBootstrapTokenInvalid's
	// own "don't tell the caller which case" reasoning.
	ErrSessionInvalid = errors.New("webadmin: session is invalid, expired, or revoked")
	// ErrNoCredentialsRegistered is returned by BeginLogin when the
	// Owner has no registered WebAuthn credential yet — a real,
	// reachable case (e.g. Phase 1's registration was never completed,
	// or every credential was later revoked via
	// miauthctl web-login revoke-credential), not hypothetical; see
	// go-webauthn's own BeginLogin/BeginMediatedLogin, which returns
	// protocol.ErrBadRequest for exactly this and is remapped to this
	// sentinel here so callers don't need to know that library detail.
	ErrNoCredentialsRegistered = errors.New("webadmin: owner has no registered WebAuthn credentials")
)

// loginCeremonyTTL bounds a login ceremony's own pending window —
// shorter than bootstrapTokenTTL (a login is expected to complete in
// seconds; there is no "operator manually opens a URL on another
// device" step the way bootstrap-token registration has). See
// plan-136-phase2 §1 Decision 2.
const loginCeremonyTTL = 5 * time.Minute

type Config struct {
	// RPID is the WebAuthn Relying Party ID: LOCAL_ORIGIN's hostname
	// (no scheme, no port) — e.g. "portal.example.com" for
	// "https://portal.example.com". Computed by the caller (cmd/server,
	// cmd/miauthctl) via net/url.Parse(cfg.Auth.LocalOrigin).Hostname(),
	// not here, so this package stays free of internal/config's own
	// parsing.
	RPID string
	// RPDisplayName is the *service's* name, shown by some authenticator
	// UIs during the ceremony (e.g. "miauth-private-portal") — no
	// security role, and NOT the Owner's own display name (that's
	// OwnerDisplayName below; WebAuthn's RPDisplayName and a
	// credential's user-entity displayName are two different concepts
	// in the spec — don't conflate them). A fixed string constant is
	// fine; this codebase has no existing "service branding name"
	// config key to reuse.
	RPDisplayName string
	// RPOrigins is the exact-match allowed origin list — []string{LOCAL_ORIGIN}
	// (full origin, scheme+host+port), matching this codebase's existing
	// exact-origin-allowlist convention (ADR-0001's client-callback
	// allowlist, OPENWEBUI_ALLOWED_ORIGINS) rather than a wildcard.
	RPOrigins []string
	// OwnerUsername/OwnerDisplayName back the WebAuthn user entity's own
	// name/displayName (cosmetic — shown by some authenticator UIs, no
	// security role; WebAuthnID, the Owner's actor ID, is what
	// authentication/authorization actually keys on per the library's
	// own doc comment). Sourced the same way internal/miauth.Config's
	// identically-named fields are — cfg.Auth.OwnerUsername/
	// OwnerDisplayName — deliberately NOT read live from
	// domain.Actor.DisplayName (which is *string, owner-editable via
	// POST /api/i/update, and would need its own nil-handling): the
	// live value is a nice-to-have this field skips for Phase 1's
	// simplicity, since nothing about it is security-relevant.
	OwnerUsername    string
	OwnerDisplayName string
	Clock            Clock
	// SessionCookie is what BeginLogin/FinishLogin/RequireAdminSession/
	// Logout need to know about the session cookie itself — kept
	// separate from the WebAuthn-ceremony fields above since it has
	// nothing to do with the library, only with how this service's
	// caller (internal/httpserver) wants the cookie shaped.
	SessionCookie SessionCookieConfig
}

// SessionCookieConfig is Config.SessionCookie's own type (Issue #136
// Phase 2). SessionTTL is ADMIN_SESSION_TTL, resolved live per
// plan-136-phase2 §1 Decision 1 (db-eligible) via ReloadSessionTTL,
// falling back to SessionTTL when nil or non-positive — the exact
// rss.Config.ReloadSummaryMaxChars pattern.
type SessionCookieConfig struct {
	SessionTTL       time.Duration
	ReloadSessionTTL func(ctx context.Context) time.Duration
}

type Service struct {
	repos            domain.Repos
	uow              domain.UnitOfWork
	webauthn         *webauthn.WebAuthn
	ownerUsername    string
	ownerDisplayName string
	clock            Clock
	sessionCookie    SessionCookieConfig
}

func NewService(uow domain.UnitOfWork, repos domain.Repos, cfg Config) (*Service, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPID: cfg.RPID, RPDisplayName: cfg.RPDisplayName, RPOrigins: cfg.RPOrigins,
	})
	if err != nil {
		return nil, fmt.Errorf("webadmin: init webauthn: %w", err)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Service{
		repos: repos, uow: uow, webauthn: wa, clock: clock,
		ownerUsername: cfg.OwnerUsername, ownerDisplayName: cfg.OwnerDisplayName,
		sessionCookie: cfg.SessionCookie,
	}, nil
}

func (s *Service) sessionTTL(ctx context.Context) time.Duration {
	if s.sessionCookie.ReloadSessionTTL != nil {
		if d := s.sessionCookie.ReloadSessionTTL(ctx); d > 0 {
			return d
		}
	}
	return s.sessionCookie.SessionTTL
}

// IssueBootstrapToken creates a new single-use bootstrap token bound to
// the current Owner actor (resolved the same way ownerActorID does in
// cmd/miauthctl — the caller passes it in, already resolved, rather
// than this package depending on cmd/miauthctl's helper) and returns
// its raw value. The raw value is returned to the CLI caller exactly
// once, to print to stdout and never log — only its hash is persisted.
func (s *Service) IssueBootstrapToken(ctx context.Context, ownerActorID string) (raw string, tok domain.WebAdminBootstrapToken, err error) {
	now := s.clock.Now()
	raw = newRawBootstrapToken()
	tok = domain.WebAdminBootstrapToken{
		ID: domain.NewID(), TokenHash: hashBootstrapToken(raw), OwnerActorID: ownerActorID,
		Status: domain.WebAdminBootstrapIssued, CreatedAt: now, ExpiresAt: now.Add(bootstrapTokenTTL),
	}
	if err := s.repos.WebAdminBootstrapTokens.Create(ctx, tok); err != nil {
		return "", domain.WebAdminBootstrapToken{}, fmt.Errorf("webadmin: create bootstrap token: %w", err)
	}
	return raw, tok, nil
}

// validateBootstrapToken looks rawToken up and confirms it is still
// issued and unexpired, mapping every failure (unknown hash, wrong
// status, expired) to the one generic ErrBootstrapTokenInvalid. Called
// at the start of every ceremony step (GET /admin/setup's own read-only
// check, BeginRegistration, FinishRegistration) so a token that expires
// mid-ceremony is caught at whichever step notices, not just the first.
func (s *Service) validateBootstrapToken(ctx context.Context, rawToken string) (domain.WebAdminBootstrapToken, error) {
	tok, err := s.repos.WebAdminBootstrapTokens.GetByTokenHash(ctx, hashBootstrapToken(rawToken))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.WebAdminBootstrapToken{}, ErrBootstrapTokenInvalid
		}
		return domain.WebAdminBootstrapToken{}, err
	}
	if tok.Status != domain.WebAdminBootstrapIssued || !s.clock.Now().Before(tok.ExpiresAt) {
		return domain.WebAdminBootstrapToken{}, ErrBootstrapTokenInvalid
	}
	return tok, nil
}

// CheckBootstrapToken is the read-only validity check GET /admin/setup
// uses to decide whether to render the registration page at all (no
// state change — repeated GETs of the same still-valid URL must not
// mutate anything, matching ADR-0001's "a repeat GET must not... mutate
// the pending attempt" MiAuth precedent applied to this credential
// type).
func (s *Service) CheckBootstrapToken(ctx context.Context, rawToken string) error {
	_, err := s.validateBootstrapToken(ctx, rawToken)
	return err
}

// BeginRegistration re-validates rawToken, loads the Owner's existing
// credentials (so the ceremony can exclude them via WithExclusions,
// preventing the same authenticator from double-registering), starts a
// WebAuthn registration ceremony via the library, persists the
// resulting SessionData against the bootstrap token row, and returns
// the CredentialCreation options JSON for the browser's
// navigator.credentials.create() call.
func (s *Service) BeginRegistration(ctx context.Context, rawToken string) (*protocol.CredentialCreation, error) {
	tok, err := s.validateBootstrapToken(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	owner, err := s.repos.Actors.GetByType(ctx, domain.ActorOwner)
	if err != nil {
		return nil, fmt.Errorf("webadmin: resolve owner actor: %w", err)
	}
	existing, err := s.repos.WebAdminCredentials.ListByOwner(ctx, owner.ID)
	if err != nil {
		return nil, fmt.Errorf("webadmin: list existing credentials: %w", err)
	}
	credentials := decodeCredentials(existing)
	user := ownerWebAuthnUser{
		ownerActorID: owner.ID, ownerUsername: s.ownerUsername, ownerDisplayName: s.ownerDisplayName,
		credentials: credentials,
	}
	creation, session, err := s.webauthn.BeginRegistration(user,
		webauthn.WithExclusions(webauthn.Credentials(credentials).CredentialDescriptors()))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCeremonyFailed, err)
	}
	sessionJSON, err := encodeSessionData(session)
	if err != nil {
		return nil, fmt.Errorf("webadmin: encode session data: %w", err)
	}
	if err := s.repos.WebAdminBootstrapTokens.SetSessionData(ctx, tok.ID, sessionJSON, s.clock.Now()); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil, ErrBootstrapTokenInvalid // expired/consumed since validateBootstrapToken, same instant ago
		}
		return nil, fmt.Errorf("webadmin: persist session data: %w", err)
	}
	return creation, nil
}

// FinishRegistration re-validates rawToken, loads its stored
// SessionData (set by BeginRegistration — a token presented here
// without having gone through BeginRegistration first, or whose session
// data was already consumed, is ErrBootstrapTokenInvalid, not a
// ceremony failure), verifies r's body against it via the library, and
// — only on a successful verification — atomically consumes the
// bootstrap token and persists the new WebAdminCredential together in
// one transaction, so a crash between them can never leave a consumed
// token with no credential or a credential with a still-usable token.
func (s *Service) FinishRegistration(ctx context.Context, rawToken string, r *http.Request) (domain.WebAdminCredential, error) {
	tok, err := s.validateBootstrapToken(ctx, rawToken)
	if err != nil {
		return domain.WebAdminCredential{}, err
	}
	if tok.WebAuthnSessionData == nil {
		return domain.WebAdminCredential{}, ErrBootstrapTokenInvalid
	}
	session, err := decodeSessionData(*tok.WebAuthnSessionData)
	if err != nil {
		return domain.WebAdminCredential{}, fmt.Errorf("webadmin: decode session data: %w", err)
	}
	owner, err := s.repos.Actors.GetByType(ctx, domain.ActorOwner)
	if err != nil {
		return domain.WebAdminCredential{}, fmt.Errorf("webadmin: resolve owner actor: %w", err)
	}
	user := ownerWebAuthnUser{ownerActorID: owner.ID, ownerUsername: s.ownerUsername, ownerDisplayName: s.ownerDisplayName}
	cred, err := s.webauthn.FinishRegistration(user, *session, r)
	if err != nil {
		return domain.WebAdminCredential{}, fmt.Errorf("%w: %w", ErrCeremonyFailed, err)
	}
	credJSON, err := encodeCredential(cred)
	if err != nil {
		return domain.WebAdminCredential{}, fmt.Errorf("webadmin: encode credential: %w", err)
	}
	now := s.clock.Now()
	record := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: owner.ID,
		CredentialID: base64.RawURLEncoding.EncodeToString(cred.ID), CredentialJSON: credJSON, CreatedAt: now,
	}
	err = s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if _, err := repos.WebAdminBootstrapTokens.Consume(ctx, tok.ID, now); err != nil {
			return err
		}
		return repos.WebAdminCredentials.Create(ctx, record)
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return domain.WebAdminCredential{}, ErrBootstrapTokenInvalid // raced to consumption/expiry
		}
		return domain.WebAdminCredential{}, fmt.Errorf("webadmin: commit registration: %w", err)
	}
	return record, nil
}

// BeginLogin starts a WebAuthn login ceremony for the Owner: loads
// their registered credentials (ErrNoCredentialsRegistered if there are
// none), creates a new pending WebAdminSession row (this row's own ID
// is the correlation value the caller must thread through to
// FinishLogin — no separate ceremony cookie, the same pattern Phase 1's
// BeginRegistration/FinishRegistration established for bootstrap
// tokens), and returns the CredentialAssertion options JSON for the
// browser's navigator.credentials.get() call alongside that session ID.
func (s *Service) BeginLogin(ctx context.Context) (assertion *protocol.CredentialAssertion, sessionID string, err error) {
	owner, err := s.repos.Actors.GetByType(ctx, domain.ActorOwner)
	if err != nil {
		return nil, "", fmt.Errorf("webadmin: resolve owner actor: %w", err)
	}
	existing, err := s.repos.WebAdminCredentials.ListByOwner(ctx, owner.ID)
	if err != nil {
		return nil, "", fmt.Errorf("webadmin: list credentials: %w", err)
	}
	credentials := decodeCredentials(existing)
	if len(credentials) == 0 {
		return nil, "", ErrNoCredentialsRegistered
	}
	user := ownerWebAuthnUser{
		ownerActorID: owner.ID, ownerUsername: s.ownerUsername, ownerDisplayName: s.ownerDisplayName,
		credentials: credentials,
	}
	assertion, session, err := s.webauthn.BeginLogin(user)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrCeremonyFailed, err)
	}
	sessionJSON, err := encodeSessionData(session)
	if err != nil {
		return nil, "", fmt.Errorf("webadmin: encode session data: %w", err)
	}
	now := s.clock.Now()
	record := domain.WebAdminSession{
		ID: domain.NewID(), OwnerActorID: owner.ID, Status: domain.WebAdminSessionPending,
		WebAuthnSessionData: &sessionJSON, CreatedAt: now, ExpiresAt: now.Add(loginCeremonyTTL),
	}
	if err := s.repos.WebAdminSessions.Create(ctx, record); err != nil {
		return nil, "", fmt.Errorf("webadmin: create login session: %w", err)
	}
	return assertion, record.ID, nil
}

// FinishLogin loads sessionID's pending row (ErrSessionInvalid if it
// doesn't exist, isn't pending, or has expired — the ceremony window,
// not the eventual session lifetime), verifies r's body against its
// stored SessionData via the library, persists the library's updated
// Credential back to storage (REQUIRED per go-webauthn's own Storage
// documentation: "Fields that change across assertions —
// Authenticator.SignCount, Authenticator.CloneWarning,
// CredentialFlags.UserVerified, and CredentialFlags.BackupState (when
// BackupEligible) — MUST be written back to storage on every successful
// FinishLogin/ValidateLogin so the next ceremony observes the current
// values." Skipping this silently breaks WebAuthn's clone-detection
// forever — see plan-136-phase2 §0 and §5's dedicated test), and — only
// on success — atomically activates the session: mints a fresh session
// token + CSRF token, extends expiry to sessionTTL(ctx), and clears the
// now-unneeded WebAuthnSessionData. Returns the raw session token (for
// the cookie) and the session record (for the CSRF token / expiry the
// caller sets the cookie's attributes from).
func (s *Service) FinishLogin(ctx context.Context, sessionID string, r *http.Request) (rawSessionToken string, session domain.WebAdminSession, err error) {
	pending, err := s.repos.WebAdminSessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", domain.WebAdminSession{}, ErrSessionInvalid
		}
		return "", domain.WebAdminSession{}, err
	}
	if pending.Status != domain.WebAdminSessionPending || pending.WebAuthnSessionData == nil || !s.clock.Now().Before(pending.ExpiresAt) {
		return "", domain.WebAdminSession{}, ErrSessionInvalid
	}
	webauthnSession, err := decodeSessionData(*pending.WebAuthnSessionData)
	if err != nil {
		return "", domain.WebAdminSession{}, fmt.Errorf("webadmin: decode session data: %w", err)
	}
	owner, err := s.repos.Actors.GetByType(ctx, domain.ActorOwner)
	if err != nil {
		return "", domain.WebAdminSession{}, fmt.Errorf("webadmin: resolve owner actor: %w", err)
	}
	existing, err := s.repos.WebAdminCredentials.ListByOwner(ctx, owner.ID)
	if err != nil {
		return "", domain.WebAdminSession{}, fmt.Errorf("webadmin: list credentials: %w", err)
	}
	user := ownerWebAuthnUser{
		ownerActorID: owner.ID, ownerUsername: s.ownerUsername, ownerDisplayName: s.ownerDisplayName,
		credentials: decodeCredentials(existing),
	}
	cred, err := s.webauthn.FinishLogin(user, *webauthnSession, r)
	if err != nil {
		return "", domain.WebAdminSession{}, fmt.Errorf("%w: %w", ErrCeremonyFailed, err)
	}
	credentialID := base64.RawURLEncoding.EncodeToString(cred.ID)
	credJSON, err := encodeCredential(cred)
	if err != nil {
		return "", domain.WebAdminSession{}, fmt.Errorf("webadmin: encode credential: %w", err)
	}
	now := s.clock.Now()
	rawSessionToken = newRawSessionToken()
	csrfToken := newCSRFToken()
	err = s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		// MUST happen even though this write's own success has no
		// further bearing on whether login succeeds: see this
		// function's doc comment. It is inside the same transaction as
		// Activate below only for atomicity convenience, not because a
		// failure here should abort a successful FinishLogin — a
		// storage error updating sign-count bookkeeping on an
		// already-cryptographically-verified login is still a login
		// the Owner just correctly proved.
		if err := repos.WebAdminCredentials.UpdateAfterLogin(ctx, credentialID, credJSON, now); err != nil {
			return err
		}
		session, err = repos.WebAdminSessions.Activate(ctx, pending.ID, credentialID,
			hashSessionToken(rawSessionToken), csrfToken, now.Add(s.sessionTTL(ctx)), now)
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return "", domain.WebAdminSession{}, ErrSessionInvalid // raced to expiry since the Get above
		}
		return "", domain.WebAdminSession{}, fmt.Errorf("webadmin: commit login: %w", err)
	}
	return rawSessionToken, session, nil
}

// VerifySession is RequireAdminSession's lookup: hash rawSessionToken
// and require an active, unrevoked, unexpired row. Returns
// ErrSessionInvalid for every failure case, never a more specific one
// (plan-136-phase2 §1 Decision 4).
func (s *Service) VerifySession(ctx context.Context, rawSessionToken string) (domain.WebAdminSession, error) {
	session, err := s.repos.WebAdminSessions.GetActiveBySessionTokenHash(ctx, hashSessionToken(rawSessionToken), s.clock.Now())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.WebAdminSession{}, ErrSessionInvalid
		}
		return domain.WebAdminSession{}, err
	}
	return session, nil
}

// Logout revokes sessionID. Idempotent — revoking an already-revoked or
// expired session is a success (see
// domain.WebAdminSessionRepository.Revoke's own doc comment): a logout
// button must never surface an error just because the session already
// expired between page load and the click.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	return s.repos.WebAdminSessions.Revoke(ctx, sessionID, s.clock.Now())
}

// ListCredentials and RevokeCredential back miauthctl web-login
// list-credentials/revoke-credential.
func (s *Service) ListCredentials(ctx context.Context, ownerActorID string) ([]domain.WebAdminCredential, error) {
	return s.repos.WebAdminCredentials.ListByOwner(ctx, ownerActorID)
}

// RevokeCredentialResult reports what RevokeCredential actually did, so
// the CLI can print an accurate summary.
type RevokeCredentialResult struct {
	RevokedSessions int
}

// RevokeCredential deletes credentialRowID's WebAdminCredential and, in
// the same transaction, revokes every currently-active session that
// credential established (plan-136-phase2 §1 Decision 6 — revoking a
// lost/stolen device's credential must also cut off any session it
// already has open, not just block future logins from it).
func (s *Service) RevokeCredential(ctx context.Context, credentialRowID string) (RevokeCredentialResult, error) {
	cred, err := s.repos.WebAdminCredentials.Get(ctx, credentialRowID)
	if err != nil {
		return RevokeCredentialResult{}, err // domain.ErrNotFound passes through
	}
	var result RevokeCredentialResult
	now := s.clock.Now()
	err = s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if err := repos.WebAdminCredentials.Delete(ctx, credentialRowID); err != nil {
			return err
		}
		n, err := repos.WebAdminSessions.RevokeAllByCredential(ctx, cred.CredentialID, now)
		result.RevokedSessions = n
		return err
	})
	if err != nil {
		return RevokeCredentialResult{}, err
	}
	return result, nil
}
