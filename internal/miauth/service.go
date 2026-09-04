// Package miauth implements the Aria-facing local MiAuth flow, explicit
// host-operator approval, and local API-token issuance/scope checks.
package miauth

import (
	"context"
	"errors"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

const localSessionTTL = 10 * time.Minute

var (
	ErrClientCallbackNotAllowed = errors.New("miauth: client callback not allowed")
	ErrSessionUnavailable       = errors.New("miauth: session unavailable")
	ErrCheckNotReady            = errors.New("miauth: check not ready")
	ErrTokenInvalid             = errors.New("miauth: invalid token")
)

type Config struct {
	ClientCallbacks  []string
	OwnerUsername    string
	OwnerDisplayName string
	Clock            Clock
}

type Service struct {
	uow   domain.UnitOfWork
	repos domain.Repos
	clock Clock
	cfg   Config
}

func NewService(uow domain.UnitOfWork, repos domain.Repos, cfg Config) *Service {
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Service{uow: uow, repos: repos, clock: clock, cfg: cfg}
}

func expired(now, at time.Time) bool { return !now.Before(at) }

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

// StartLocalSession creates, or idempotently resumes, an unexpired
// Aria-facing session. Creating the session never authorizes it.
func (s *Service) StartLocalSession(ctx context.Context, routeSessionID, requestedPermission string, clientCallback *string) error {
	if clientCallback != nil && !s.callbackAllowed(*clientCallback) {
		return ErrClientCallbackNotAllowed
	}
	now := s.clock.Now()
	return s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		existing, err := repos.LocalMiAuth.Get(ctx, routeSessionID)
		if errors.Is(err, domain.ErrNotFound) {
			return repos.LocalMiAuth.Create(ctx, domain.LocalMiAuthSession{
				RouteSessionID:       routeSessionID,
				Status:               domain.MiAuthCreated,
				RequestedPermissions: requestedPermission,
				ClientCallback:       clientCallback,
				CreatedAt:            now,
				ExpiresAt:            now.Add(localSessionTTL),
			})
		}
		if err != nil {
			return err
		}
		if existing.Status != domain.MiAuthCreated || expired(now, existing.ExpiresAt) ||
			existing.RequestedPermissions != requestedPermission || !sameCallback(existing.ClientCallback, clientCallback) {
			return ErrSessionUnavailable
		}
		return nil
	})
}

// ApproveSession is the trusted-host transition performed by miauthctl.
// The local session's status/expiry guard is enforced again by the
// repository update, making approval single-use under concurrent calls.
func (s *Service) ApproveSession(ctx context.Context, routeSessionID string) error {
	now := s.clock.Now()
	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		session, err := repos.LocalMiAuth.Get(ctx, routeSessionID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return ErrSessionUnavailable
			}
			return err
		}
		if session.Status != domain.MiAuthCreated || expired(now, session.ExpiresAt) {
			return ErrSessionUnavailable
		}

		ownerActorID, err := ensureOwnerActor(ctx, repos, now)
		if err != nil {
			return err
		}
		if err := repos.LocalMiAuth.Authorize(ctx, routeSessionID, ownerActorID, now); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return ErrSessionUnavailable
			}
			return err
		}
		return nil
	})
	return err
}

func ensureOwnerActor(ctx context.Context, repos domain.Repos, now time.Time) (string, error) {
	owner, err := repos.Actors.GetByType(ctx, domain.ActorOwner)
	if err == nil {
		return owner.ID, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}
	owner = domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: now}
	if err := repos.Actors.Create(ctx, owner); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			existing, getErr := repos.Actors.GetByType(ctx, domain.ActorOwner)
			if getErr != nil {
				return "", getErr
			}
			return existing.ID, nil
		}
		return "", err
	}
	return owner.ID, nil
}

func (s *Service) RejectSession(ctx context.Context, routeSessionID string) error {
	err := s.repos.LocalMiAuth.Deny(ctx, routeSessionID, s.clock.Now())
	if errors.Is(err, domain.ErrConflict) {
		return ErrSessionUnavailable
	}
	return err
}

func (s *Service) ListPendingSessions(ctx context.Context) ([]domain.LocalMiAuthSession, error) {
	return s.repos.LocalMiAuth.ListPending(ctx, s.clock.Now())
}

func (s *Service) ListAPITokens(ctx context.Context) ([]domain.APIToken, error) {
	return s.repos.APITokens.List(ctx)
}

func (s *Service) RevokeAPIToken(ctx context.Context, tokenID string) error {
	return s.repos.APITokens.Revoke(ctx, tokenID, s.clock.Now())
}

type CheckResult struct {
	Token            string
	OwnerActorID     string
	OwnerCreatedAt   time.Time
	OwnerUsername    string
	OwnerDisplayName string
}

func (s *Service) Check(ctx context.Context, routeSessionID string) (CheckResult, error) {
	now := s.clock.Now()
	var result CheckResult
	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		local, err := repos.LocalMiAuth.Consume(ctx, routeSessionID, now)
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return ErrCheckNotReady
			}
			return err
		}
		if local.LocalActorID == nil {
			return errors.New("miauth: consumed session has no local actor id")
		}
		owner, err := repos.Actors.Get(ctx, *local.LocalActorID)
		if err != nil {
			return err
		}
		raw := newRawAPIToken()
		token := domain.APIToken{
			ID:                   domain.NewID(),
			TokenHash:            hashAPIToken(raw),
			LocalActorID:         owner.ID,
			MiAuthLocalSessionID: &routeSessionID,
			Scopes:               scopesString(effectiveScopes(local.RequestedPermissions)),
			CreatedAt:            now,
		}
		if err := repos.APITokens.Create(ctx, token); err != nil {
			return err
		}
		result = CheckResult{
			Token: raw, OwnerActorID: owner.ID, OwnerCreatedAt: owner.CreatedAt,
			OwnerUsername: s.cfg.OwnerUsername, OwnerDisplayName: s.cfg.OwnerDisplayName,
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

func (s *Service) VerifyToken(ctx context.Context, rawToken, requiredScope string) (string, error) {
	tok, err := s.repos.APITokens.GetByTokenHash(ctx, hashAPIToken(rawToken))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", ErrTokenInvalid
		}
		return "", err
	}
	if tok.RevokedAt != nil || !hasScope(tok.Scopes, requiredScope) {
		return "", ErrTokenInvalid
	}
	_ = s.repos.APITokens.TouchLastUsed(ctx, tok.ID, s.clock.Now())
	return tok.LocalActorID, nil
}

// OwnerProfile is the owner projection internal/httpserver builds
// Misskey-compatible wire responses from outside of a fresh Check call
// (Issue #7's POST /api/i, in particular). Its fields mirror CheckResult's
// owner-related fields so both call sites feed the same wire constructor.
type OwnerProfile struct {
	ActorID     string
	Username    string
	DisplayName string
	CreatedAt   time.Time
}

// DescribeOwner returns the owner's profile for actorID, the local actor
// ID VerifyToken resolves a verified API token to. This deployment only
// ever issues local API tokens to the single bound owner (AGENTS.md: no
// general user login), so any actorID reaching here from
// httpserver.LocalActorIDFromContext already names the owner actor; this
// just re-fetches its CreatedAt and layers the configured
// username/display name on top, the same projection CheckResult carries
// right after a successful Check.
func (s *Service) DescribeOwner(ctx context.Context, actorID string) (OwnerProfile, error) {
	actor, err := s.repos.Actors.Get(ctx, actorID)
	if err != nil {
		return OwnerProfile{}, err
	}
	return OwnerProfile{
		ActorID:     actor.ID,
		Username:    s.cfg.OwnerUsername,
		DisplayName: s.cfg.OwnerDisplayName,
		CreatedAt:   actor.CreatedAt,
	}, nil
}
