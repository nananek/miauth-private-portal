// Package miauth implements Issue #5's bridged MiAuth use cases: the
// Aria-facing local session, the upstream owner-verification session,
// the operator bootstrap gate, and local API token issuance/scope
// checking. It depends only on internal/domain and this package's own
// UpstreamProvider boundary — never on net/http, database/sql, or any
// specific LLM/storage driver type — per AGENTS.md's rule that
// domain/use-case code must not depend on transport or storage details.
package miauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// TTLs are ADR-0001's accepted, fixed design (10-minute local/upstream
// sessions, a 15-minute bootstrap gate) rather than operator-configurable
// settings — the same treatment internal/storage/sqlite gives
// foreign_keys/journal_mode.
const (
	localSessionTTL    = 10 * time.Minute
	upstreamSessionTTL = 10 * time.Minute
	bootstrapGateTTL   = 15 * time.Minute
)

var (
	// ErrClientCallbackNotAllowed is returned by StartLocalSession when
	// Aria supplies a callback query parameter not present in the
	// configured exact-match allowlist.
	ErrClientCallbackNotAllowed = errors.New("miauth: client callback not allowed")
	// ErrSessionUnavailable is returned by StartLocalSession when the
	// route session already exists but cannot be resumed: it is in a
	// terminal state, has expired, or the request no longer matches the
	// pending attempt exactly.
	ErrSessionUnavailable = errors.New("miauth: session cannot be started or resumed")
	// ErrCallbackInvalid is returned by HandleUpstreamCallback for an
	// unknown id, a state that does not match (constant-time compared),
	// or an expired/non-pending session. It deliberately does not
	// mutate any session row: see HandleUpstreamCallback's doc comment
	// for why a bad state guess must not be able to burn the legitimate
	// holder's attempt.
	ErrCallbackInvalid = errors.New("miauth: invalid or expired callback")
	// ErrUpstreamVerification is returned by HandleUpstreamCallback when
	// the upstream MiAuth check either failed as a transport/decode
	// error, or came back ok=false (upstream did not approve).
	ErrUpstreamVerification = errors.New("miauth: upstream verification failed")
	// ErrOwnerBindingDenied is returned by HandleUpstreamCallback when
	// upstream verification succeeded but the verified identity does
	// not satisfy this deployment's owner-binding rules (wrong user, or
	// does not match an existing binding).
	ErrOwnerBindingDenied = errors.New("miauth: owner binding denied")
	// ErrCheckNotReady is returned by Check for every non-success
	// outcome — not found, not yet authorized, already consumed
	// (replay), expired, or denied. Aria's wire contract collapses all
	// of these to a uniform {"ok":false} response
	// (docs/compat/aria-v1.5.11.md), so callers need only distinguish
	// success from not-success, never this error's specific cause.
	ErrCheckNotReady = errors.New("miauth: check not ready")
	// ErrTokenInvalid is returned by VerifyToken for an unknown, revoked,
	// or insufficiently-scoped token.
	ErrTokenInvalid = errors.New("miauth: invalid token")
	// ErrAlreadyBound is returned by IssueBootstrapGate and
	// StartBootstrapSession once an OwnerBinding already exists: the
	// bootstrap path only ever creates the first binding.
	ErrAlreadyBound = errors.New("miauth: owner already bound")
	// ErrBootstrapUnavailable is returned by StartBootstrapSession for
	// an unknown, expired, or non-issued gate, or once a binding already
	// exists. It intentionally does not distinguish these cases in the
	// error value: the handler renders one generic response either way,
	// so a probing request cannot learn which case applies.
	ErrBootstrapUnavailable = errors.New("miauth: bootstrap gate unavailable")
)

// Config carries the primitive/typed values Service needs from
// configuration. It never embeds internal/config.Config, matching
// internal/httpserver's existing boundary of translating config at the
// call site rather than depending on the config package directly.
type Config struct {
	// IdentityOrigin is the fixed upstream Misskey origin (ADR-0001
	// IDENTITY_ORIGIN) used both for the upstream redirect and as the
	// origin recorded on every UpstreamMiAuthSession and OwnerBinding.
	IdentityOrigin string
	// AllowedMisskeyUserID is the single-owner allowlist. Empty means
	// this deployment is bootstrap-only until an operator completes the
	// bootstrap-gate flow.
	AllowedMisskeyUserID string
	// ClientCallbacks is the exact-match allowlist of Aria client return
	// callbacks. An empty list rejects any client-supplied callback.
	ClientCallbacks []string
	// OwnerUsername and OwnerDisplayName back the UserDetailedNotMe
	// projection httpserver builds from a successful Check.
	OwnerUsername    string
	OwnerDisplayName string
}

// Service implements Issue #5's MiAuth use cases.
type Service struct {
	uow      domain.UnitOfWork
	repos    domain.Repos
	upstream UpstreamProvider
	clock    Clock
	cfg      Config
}

// NewService builds a Service. uow and repos are usually the same
// *sqlite.DB (which implements both domain.UnitOfWork and embeds
// domain.Repos), but Service depends only on these two domain
// interfaces, never on the concrete storage type.
func NewService(uow domain.UnitOfWork, repos domain.Repos, upstream UpstreamProvider, cfg Config) *Service {
	return &Service{uow: uow, repos: repos, upstream: upstream, clock: realClock{}, cfg: cfg}
}

// expired reports whether at has passed as of now, using the same
// "expires_at > now means still valid" convention every CAS repository
// method's WHERE clause already encodes.
func expired(now, at time.Time) bool {
	return !now.Before(at)
}

func (s *Service) callbackAllowed(callback string) bool {
	for _, allowed := range s.cfg.ClientCallbacks {
		if allowed == callback {
			return true
		}
	}
	return false
}

func sameCallback(existing, requested *string) bool {
	if existing == nil || requested == nil {
		return existing == requested
	}
	return *existing == *requested
}

// StartedSession is what the caller needs to build the redirect to
// IDENTITY_ORIGIN: the upstream session's ID and its crypto/rand state.
// Both are embedded by the caller (internal/httpserver) as query
// parameters on the internal callback URL it hands to Misskey, since
// Misskey has no notion of this service's own state value — see
// HandleUpstreamCallback's doc comment.
type StartedSession struct {
	UpstreamSessionID string
	UpstreamState     string
}

// StartLocalSession handles GET /miauth/{session}: it creates (or
// idempotently resumes) the Aria-facing local session and its linked
// upstream verification session, and returns the upstream session the
// caller redirects the browser to.
//
// A repeated request with the same routeSessionID, requestedPermission,
// and clientCallback resumes the existing pending attempt rather than
// erroring on the linked upstream session's UNIQUE(local_session_id)
// constraint. A repeated request that differs in either field, or whose
// prior attempt is no longer pending, returns ErrSessionUnavailable
// without mutating the existing row.
func (s *Service) StartLocalSession(ctx context.Context, routeSessionID, requestedPermission string, clientCallback *string) (StartedSession, error) {
	if clientCallback != nil && !s.callbackAllowed(*clientCallback) {
		return StartedSession{}, ErrClientCallbackNotAllowed
	}

	now := s.clock.Now()

	var started StartedSession
	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		existing, getErr := repos.LocalMiAuth.Get(ctx, routeSessionID)
		if errors.Is(getErr, domain.ErrNotFound) {
			return s.createLocalAndUpstreamSession(ctx, repos, routeSessionID, requestedPermission, clientCallback, now, &started)
		}
		if getErr != nil {
			return getErr
		}

		if existing.Status != domain.MiAuthCreated || expired(now, existing.ExpiresAt) ||
			existing.RequestedPermissions != requestedPermission || !sameCallback(existing.ClientCallback, clientCallback) {
			return ErrSessionUnavailable
		}

		upstream, uErr := repos.UpstreamMiAuth.GetByLocalSessionID(ctx, routeSessionID)
		if uErr != nil {
			return uErr
		}
		started = StartedSession{UpstreamSessionID: upstream.ID, UpstreamState: upstream.State}
		return nil
	})
	if err != nil {
		return StartedSession{}, err
	}
	return started, nil
}

func (s *Service) createLocalAndUpstreamSession(
	ctx context.Context, repos domain.Repos, routeSessionID, requestedPermission string,
	clientCallback *string, now time.Time, out *StartedSession,
) error {
	local := domain.LocalMiAuthSession{
		RouteSessionID:       routeSessionID,
		State:                domain.NewID(),
		Status:               domain.MiAuthCreated,
		RequestedPermissions: requestedPermission,
		ClientCallback:       clientCallback,
		CreatedAt:            now,
		ExpiresAt:            now.Add(localSessionTTL),
	}
	if err := repos.LocalMiAuth.Create(ctx, local); err != nil {
		return err
	}

	upstreamState := domain.NewID()
	upstream := domain.UpstreamMiAuthSession{
		ID:             domain.NewID(),
		LocalSessionID: &routeSessionID,
		IdentityOrigin: s.cfg.IdentityOrigin,
		State:          upstreamState,
		Status:         domain.MiAuthCreated,
		CreatedAt:      now,
		ExpiresAt:      now.Add(upstreamSessionTTL),
	}
	if err := repos.UpstreamMiAuth.Create(ctx, upstream); err != nil {
		return err
	}
	*out = StartedSession{UpstreamSessionID: upstream.ID, UpstreamState: upstreamState}
	return nil
}

// StartBootstrapSession handles GET /miauth/bootstrap/{gate}: it
// validates the operator-issued gate and creates (or idempotently
// resumes) its linked upstream verification session, mirroring
// StartLocalSession. It refuses (ErrBootstrapUnavailable) once an
// OwnerBinding already exists, or if the gate is unknown, not in the
// issued state, or expired — all collapsed into the same error so a
// probing request cannot distinguish which case applies.
func (s *Service) StartBootstrapSession(ctx context.Context, gateID string) (StartedSession, error) {
	now := s.clock.Now()

	var started StartedSession
	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if _, bErr := repos.OwnerBindings.Get(ctx); bErr == nil {
			return ErrBootstrapUnavailable
		} else if !errors.Is(bErr, domain.ErrNotFound) {
			return bErr
		}

		gate, gErr := repos.BootstrapGates.Get(ctx, gateID)
		if gErr != nil {
			if errors.Is(gErr, domain.ErrNotFound) {
				return ErrBootstrapUnavailable
			}
			return gErr
		}
		if gate.Status != domain.BootstrapGateIssued || expired(now, gate.ExpiresAt) {
			return ErrBootstrapUnavailable
		}

		existing, uErr := repos.UpstreamMiAuth.GetByBootstrapGateID(ctx, gateID)
		if uErr == nil {
			if existing.Status != domain.MiAuthCreated || expired(now, existing.ExpiresAt) {
				return ErrBootstrapUnavailable
			}
			started = StartedSession{UpstreamSessionID: existing.ID, UpstreamState: existing.State}
			return nil
		}
		if !errors.Is(uErr, domain.ErrNotFound) {
			return uErr
		}

		upstreamState := domain.NewID()
		upstream := domain.UpstreamMiAuthSession{
			ID:              domain.NewID(),
			BootstrapGateID: &gateID,
			IdentityOrigin:  s.cfg.IdentityOrigin,
			State:           upstreamState,
			Status:          domain.MiAuthCreated,
			CreatedAt:       now,
			ExpiresAt:       now.Add(upstreamSessionTTL),
		}
		if cErr := repos.UpstreamMiAuth.Create(ctx, upstream); cErr != nil {
			return cErr
		}
		started = StartedSession{UpstreamSessionID: upstream.ID, UpstreamState: upstreamState}
		return nil
	})
	if err != nil {
		return StartedSession{}, err
	}
	return started, nil
}

// CallbackResult tells the GET /miauth/callback handler how to
// conclude a successful attempt: redirect to ClientCallback (if Aria
// supplied one) with RouteSessionID, or show its own "return to Aria"
// page if ClientCallback is nil. Both fields are zero for the bootstrap
// flow, which has no Aria route session or client callback to report.
type CallbackResult struct {
	ClientCallback *string
	RouteSessionID string
}

// HandleUpstreamCallback handles the fixed internal GET /miauth/callback
// endpoint shared by both the ordinary Aria-triggered flow and the
// operator bootstrap flow.
//
// Only an authoritative, verified outcome — upstream explicitly did not
// approve, or the verified identity fails this deployment's
// owner-binding rules — terminally denies the session (LocalMiAuth.Deny
// / UpstreamMiAuth.Deny / BootstrapGates.Fail), per ADR-0001's rule that
// a rejected callback is a terminal failure for that attempt.
//
// An invalid callback itself (unknown id, a state that does not match,
// an expired or non-pending session) does NOT deny anything and leaves
// the row untouched. The id embedded in the internal callback URL is
// observable (it passes through the upstream Misskey redirect and the
// owner's browser), but the state is not: it is a crypto/rand secret
// nothing outside this attempt should be able to guess. If a wrong
// state guess denied the session outright, anyone who merely observes
// the id (an attacker, a proxy, a duplicate request) could burn the
// legitimate holder's one real attempt before they ever complete it.
// Leaving the row untouched means only someone who already holds the
// correct state can affect its outcome, and the session simply expires
// on its own TTL if never completed correctly.
//
// A transport/timeout/decode failure calling upstream also does not
// deny the session: it is evidence about that HTTP call, not about the
// callback's validity or the user's identity, so the attempt may still
// succeed on retry once upstream recovers.
func (s *Service) HandleUpstreamCallback(ctx context.Context, upstreamSessionID, state string) (CallbackResult, error) {
	now := s.clock.Now()

	upstream, err := s.repos.UpstreamMiAuth.Get(ctx, upstreamSessionID)
	if err != nil {
		return CallbackResult{}, ErrCallbackInvalid
	}
	if upstream.Status != domain.MiAuthCreated || expired(now, upstream.ExpiresAt) || !constantTimeEqual(upstream.State, state) {
		return CallbackResult{}, ErrCallbackInvalid
	}

	upstreamUserID, ok, err := s.upstream.Check(ctx, upstreamSessionID)
	if err != nil {
		return CallbackResult{}, fmt.Errorf("%w: %v", ErrUpstreamVerification, err)
	}
	if !ok {
		if denyErr := s.denyAttempt(ctx, upstream, now); denyErr != nil {
			return CallbackResult{}, denyErr
		}
		return CallbackResult{}, ErrUpstreamVerification
	}

	verified := verifiedIdentity{IdentityOrigin: s.cfg.IdentityOrigin, UpstreamUserID: upstreamUserID}

	var result CallbackResult
	var denied bool
	err = s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		existing, bErr := existingBinding(ctx, repos)
		if bErr != nil {
			return bErr
		}

		decision := decideBinding(existing, s.cfg.AllowedMisskeyUserID, upstream.BootstrapGateID != nil, verified)
		if !decision.Allow {
			if dErr := s.denyWithinTx(ctx, repos, upstream, now); dErr != nil {
				return dErr
			}
			denied = true
			return nil
		}

		if aErr := repos.UpstreamMiAuth.Authorize(ctx, upstream.ID, upstreamUserID, now); aErr != nil {
			return aErr
		}
		if cErr := repos.UpstreamMiAuth.Consume(ctx, upstream.ID, now); cErr != nil {
			return cErr
		}

		ownerActorID, oErr := ensureOwnerActor(ctx, repos, existing, verified, now)
		if oErr != nil {
			return oErr
		}

		if upstream.BootstrapGateID != nil {
			return repos.BootstrapGates.Consume(ctx, *upstream.BootstrapGateID, now)
		}

		local, lErr := repos.LocalMiAuth.Get(ctx, *upstream.LocalSessionID)
		if lErr != nil {
			return lErr
		}
		if aErr := repos.LocalMiAuth.Authorize(ctx, local.RouteSessionID, ownerActorID, now); aErr != nil {
			return aErr
		}
		result = CallbackResult{ClientCallback: local.ClientCallback, RouteSessionID: local.RouteSessionID}
		return nil
	})
	if err != nil {
		return CallbackResult{}, err
	}
	if denied {
		return CallbackResult{}, ErrOwnerBindingDenied
	}
	return result, nil
}

// denyAttempt runs denyWithinTx in its own transaction, for the
// upstream-check-returned-not-ok path that happens before
// HandleUpstreamCallback's main transaction begins.
func (s *Service) denyAttempt(ctx context.Context, upstream domain.UpstreamMiAuthSession, now time.Time) error {
	return s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		return s.denyWithinTx(ctx, repos, upstream, now)
	})
}

// denyWithinTx marks upstream denied, and denies its linked local
// session or fails its linked bootstrap gate. It returns nil on success
// (the caller tracks the "denied" outcome itself, since returning a
// non-nil error from a WithinTx callback rolls back everything the
// callback just wrote — these writes must commit, not roll back).
func (s *Service) denyWithinTx(ctx context.Context, repos domain.Repos, upstream domain.UpstreamMiAuthSession, now time.Time) error {
	if err := repos.UpstreamMiAuth.Deny(ctx, upstream.ID); err != nil {
		return err
	}
	if upstream.BootstrapGateID != nil {
		return repos.BootstrapGates.Fail(ctx, *upstream.BootstrapGateID, now)
	}
	if upstream.LocalSessionID != nil {
		return repos.LocalMiAuth.Deny(ctx, *upstream.LocalSessionID)
	}
	return nil
}

func existingBinding(ctx context.Context, repos domain.Repos) (*verifiedIdentity, error) {
	b, err := repos.OwnerBindings.Get(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &verifiedIdentity{IdentityOrigin: b.IdentityOrigin, UpstreamUserID: b.UpstreamUserID}, nil
}

// ensureOwnerActor returns the existing Owner actor's ID, or — the
// first time a binding is created — creates the Owner actor and its
// OwnerBinding together and returns the new actor's ID.
func ensureOwnerActor(ctx context.Context, repos domain.Repos, existing *verifiedIdentity, verified verifiedIdentity, now time.Time) (string, error) {
	if existing != nil {
		owner, err := repos.Actors.GetByType(ctx, domain.ActorOwner)
		if err != nil {
			return "", err
		}
		return owner.ID, nil
	}

	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: now}
	if err := repos.Actors.Create(ctx, owner); err != nil {
		return "", err
	}
	if err := repos.OwnerBindings.Bind(ctx, domain.OwnerBinding{
		LocalActorID:   owner.ID,
		IdentityOrigin: verified.IdentityOrigin,
		UpstreamUserID: verified.UpstreamUserID,
		BoundAt:        now,
	}); err != nil {
		return "", err
	}
	return owner.ID, nil
}

// CheckResult carries what the POST /api/miauth/{session}/check success
// response needs. httpserver's miauth_wire.go builds the actual
// Misskey-compatible UserDetailedNotMe JSON from these fields; Service
// deliberately returns only the underlying facts, not a wire type, so
// this package stays independent of the transport/wire boundary.
type CheckResult struct {
	Token            string
	OwnerActorID     string
	OwnerCreatedAt   time.Time
	OwnerUsername    string
	OwnerDisplayName string
}

// Check handles POST /api/miauth/{session}/check: it atomically
// consumes an authorized local session, mints and hashes a new API
// token scoped to the session's effective permissions, and returns the
// raw token (returned to Aria exactly once; only its hash is
// persisted). Every non-success outcome collapses to ErrCheckNotReady,
// matching Aria's uniform {"ok":false} wire contract.
func (s *Service) Check(ctx context.Context, routeSessionID string) (CheckResult, error) {
	now := s.clock.Now()

	var result CheckResult
	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if err := repos.LocalMiAuth.Consume(ctx, routeSessionID, now); err != nil {
			return ErrCheckNotReady
		}

		local, err := repos.LocalMiAuth.Get(ctx, routeSessionID)
		if err != nil {
			return err
		}
		if local.LocalActorID == nil {
			// Unreachable in practice: Authorize always sets
			// LocalActorID together with the authorized status Consume
			// requires, atomically, within this same database. Treated
			// as a hard error rather than silently issuing a token with
			// no owner.
			return fmt.Errorf("miauth: consumed session %s has no local actor id", routeSessionID)
		}

		owner, err := repos.Actors.Get(ctx, *local.LocalActorID)
		if err != nil {
			return err
		}

		raw := newRawAPIToken()
		scopes := effectiveScopes(local.RequestedPermissions)
		token := domain.APIToken{
			ID:                   domain.NewID(),
			TokenHash:            hashAPIToken(raw),
			LocalActorID:         owner.ID,
			MiAuthLocalSessionID: &routeSessionID,
			Scopes:               scopesString(scopes),
			CreatedAt:            now,
		}
		if err := repos.APITokens.Create(ctx, token); err != nil {
			return err
		}

		result = CheckResult{
			Token:            raw,
			OwnerActorID:     owner.ID,
			OwnerCreatedAt:   owner.CreatedAt,
			OwnerUsername:    s.cfg.OwnerUsername,
			OwnerDisplayName: s.cfg.OwnerDisplayName,
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrCheckNotReady) {
			return CheckResult{}, ErrCheckNotReady
		}
		return CheckResult{}, err
	}
	return result, nil
}

// VerifyToken looks up rawToken by its hash, checks it is unrevoked and
// carries requiredScope by exact match, records its use, and returns
// the owning local actor's ID. It is the scope-enforcement primitive
// internal/httpserver's middleware (and, from Issue #7 on, protected
// endpoint handlers) call.
func (s *Service) VerifyToken(ctx context.Context, rawToken, requiredScope string) (string, error) {
	tok, err := s.repos.APITokens.GetByTokenHash(ctx, hashAPIToken(rawToken))
	if err != nil {
		return "", ErrTokenInvalid
	}
	if tok.RevokedAt != nil {
		return "", ErrTokenInvalid
	}
	if !hasScope(tok.Scopes, requiredScope) {
		return "", ErrTokenInvalid
	}
	if err := s.repos.APITokens.TouchLastUsed(ctx, tok.ID, s.clock.Now()); err != nil {
		return "", err
	}
	return tok.LocalActorID, nil
}

// IssueBootstrapGate creates a new operator bootstrap gate, refusing
// once an OwnerBinding already exists (ErrAlreadyBound). It is the
// helper cmd/bootstrapctl calls, so the TTL and randomness logic live
// in one place shared with the rest of this package rather than being
// duplicated in the CLI.
func (s *Service) IssueBootstrapGate(ctx context.Context) (string, error) {
	now := s.clock.Now()

	if _, err := s.repos.OwnerBindings.Get(ctx); err == nil {
		return "", ErrAlreadyBound
	} else if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}

	gate := domain.BootstrapGate{
		ID:        domain.NewID(),
		Status:    domain.BootstrapGateIssued,
		CreatedAt: now,
		ExpiresAt: now.Add(bootstrapGateTTL),
	}
	if err := s.repos.BootstrapGates.Create(ctx, gate); err != nil {
		return "", err
	}
	return gate.ID, nil
}
