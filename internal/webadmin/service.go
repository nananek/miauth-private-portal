// Package webadmin implements Issue #136 Phase 1 (ADR-0010): issuing a
// single-use, SSH-anchored bootstrap token via miauthctl web-login, and
// using it exactly once to register one WebAuthn credential for the
// Owner. It never issues a session cookie or authenticates a browser —
// that is Phase 2's scope.
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
	// ErrRegistrationCeremonyFailed wraps any go-webauthn library
	// failure during Begin/FinishRegistration (malformed response,
	// challenge mismatch, origin mismatch, signature verification
	// failure, ...) as one category for the same reason.
	ErrRegistrationCeremonyFailed = errors.New("webadmin: WebAuthn registration ceremony failed")
)

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
}

type Service struct {
	repos            domain.Repos
	uow              domain.UnitOfWork
	webauthn         *webauthn.WebAuthn
	ownerUsername    string
	ownerDisplayName string
	clock            Clock
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
	}, nil
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
		return nil, fmt.Errorf("%w: %w", ErrRegistrationCeremonyFailed, err)
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
		return domain.WebAdminCredential{}, fmt.Errorf("%w: %w", ErrRegistrationCeremonyFailed, err)
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
