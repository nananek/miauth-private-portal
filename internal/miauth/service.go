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
	// ErrNotOwner is returned by UpdateOwnerDisplayName when the resolved
	// local actor ID is not the Owner actor. In normal operation this
	// never happens (only the Owner is ever issued a local API token —
	// see RequireScope's doc comment), so this is a defense-in-depth
	// check rather than a reachable user-facing case; handlers must
	// treat it the same as any other authentication failure rather than
	// revealing that a valid-but-wrong-actor token was presented.
	ErrNotOwner = errors.New("miauth: actor is not the owner")
	// ErrTokenRevoked is returned by ReflectScopes when tokenID names a
	// revoked token: reflect-scopes only ever applies to active tokens.
	ErrTokenRevoked = errors.New("miauth: token is revoked")
	// ErrOriginatingSessionGone is returned by ReflectScopes when the
	// token's originating MiAuth session cannot be replayed — either the
	// token has no MiAuthLocalSessionID at all, or that session's row no
	// longer exists. See ReflectScopes' doc comment for why this must be
	// a clear error rather than a silent no-op.
	ErrOriginatingSessionGone = errors.New("miauth: token's originating MiAuth session is missing or unlinked")
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

		ownerActorID, err := s.ensureOwnerActor(ctx, repos, now)
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

// ensureOwnerActor seeds the initial display name from cfg.OwnerDisplayName
// only at Owner-row creation time (Issue #23 PR1: OWNER_DISPLAY_NAME is
// now just that initial value, not an ongoing source of truth — the
// actors.display_name column is, once POST /api/i/update can change it).
func (s *Service) ensureOwnerActor(ctx context.Context, repos domain.Repos, now time.Time) (string, error) {
	owner, err := repos.Actors.GetByType(ctx, domain.ActorOwner)
	if err == nil {
		return owner.ID, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}
	initialDisplayName := s.cfg.OwnerDisplayName
	owner = domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: now, DisplayName: &initialDisplayName}
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

// ReflectScopesResult is one token's before/after outcome, returned so a
// caller can report it without a second DB round trip.
type ReflectScopesResult struct {
	TokenID   string
	OldScopes string
	NewScopes string
	Changed   bool
}

// reflectScopesCompute reads tokenID's token and its originating session
// through repos and returns the would-be ReflectScopesResult, without
// writing anything. ReflectScopes' transactional write path and
// PreviewReflectScopes' read-only dry-run path both call this, so their
// semantics can never drift apart.
func reflectScopesCompute(ctx context.Context, repos domain.Repos, tokenID string) (domain.APIToken, ReflectScopesResult, error) {
	tok, err := repos.APITokens.Get(ctx, tokenID)
	if err != nil {
		return domain.APIToken{}, ReflectScopesResult{}, err
	}
	if tok.RevokedAt != nil {
		return domain.APIToken{}, ReflectScopesResult{}, ErrTokenRevoked
	}
	if tok.MiAuthLocalSessionID == nil {
		return domain.APIToken{}, ReflectScopesResult{}, ErrOriginatingSessionGone
	}
	session, err := repos.LocalMiAuth.Get(ctx, *tok.MiAuthLocalSessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.APIToken{}, ReflectScopesResult{}, ErrOriginatingSessionGone
		}
		return domain.APIToken{}, ReflectScopesResult{}, err
	}
	recomputed := scopesString(effectiveScopes(session.RequestedPermissions))
	next := clampToGrowth(tok.Scopes, recomputed)
	result := ReflectScopesResult{TokenID: tok.ID, OldScopes: tok.Scopes, NewScopes: next, Changed: next != tok.Scopes}
	return tok, result, nil
}

// PreviewReflectScopes computes what ReflectScopes would do for tokenID
// without writing anything (`tokens reflect-scopes --dry-run`). Reading
// outside a transaction is safe here since nothing is written.
func (s *Service) PreviewReflectScopes(ctx context.Context, tokenID string) (ReflectScopesResult, error) {
	_, result, err := reflectScopesCompute(ctx, s.repos, tokenID)
	if err != nil {
		return ReflectScopesResult{}, err
	}
	return result, nil
}

// ReflectScopes (Issue #133) recomputes tokenID's effective scopes from
// its originating MiAuth session's stored RequestedPermissions against
// the *current* grantableScopes, and writes the result back to
// api_tokens.scopes if it grows. This is how a token issued before a
// scope was added to grantableScopes (see effectiveScopes' doc comment)
// picks that scope up without a fresh Aria login.
//
// It is deliberately additive-only: clampToGrowth never drops a scope
// the token already carries, even one the freshly recomputed set would
// no longer include (see clampToGrowth's own doc comment for why this
// matters). Fails with ErrTokenRevoked for a revoked token, and
// ErrOriginatingSessionGone if MiAuthLocalSessionID is nil or the
// referenced session row no longer exists — a caller must never receive
// a silent no-op or a crash for those, so it can tell the operator
// exactly what remediation is needed (re-issue a new token via a fresh
// Aria login).
//
// changedByActorID is recorded as the audit entry's operator
// (ADR-0002); callers resolve it the same way cmd/miauthctl/config.go's
// ownerActorID does.
func (s *Service) ReflectScopes(ctx context.Context, tokenID, changedByActorID string) (ReflectScopesResult, error) {
	now := s.clock.Now()
	var result ReflectScopesResult
	err := s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		tok, r, err := reflectScopesCompute(ctx, repos, tokenID)
		if err != nil {
			return err
		}
		result = r
		if !result.Changed {
			return nil
		}
		if err := repos.APITokens.UpdateScopes(ctx, tok.ID, result.NewScopes, now); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// tok.RevokedAt was nil moments ago in this same
				// transaction and nothing in this codebase ever deletes
				// an api_tokens row, so zero rows affected here can only
				// mean a concurrent revoke landed between the read above
				// and this write.
				return ErrTokenRevoked
			}
			return err
		}
		return repos.TokenScopeAudit.Record(ctx, domain.APITokenScopeAuditEntry{
			ID: domain.NewID(), TokenID: tok.ID, OldScopes: result.OldScopes, NewScopes: result.NewScopes,
			ChangedAt: now, ChangedBy: changedByActorID,
		})
	})
	if err != nil {
		return ReflectScopesResult{}, err
	}
	return result, nil
}

// ReflectScopesAllItem is one token's outcome from ReflectScopesAll.
type ReflectScopesAllItem struct {
	TokenID string
	Result  ReflectScopesResult
	Err     error
}

// ReflectScopesAll runs ReflectScopes for every currently non-revoked
// token, each in its own transaction (via ReflectScopes) so a failure on
// one token never rolls back or blocks another's already-committed
// update. Revoked tokens are skipped entirely — reflect-scopes only ever
// applies to active tokens — and produce no ReflectScopesAllItem at all.
func (s *Service) ReflectScopesAll(ctx context.Context, changedByActorID string) ([]ReflectScopesAllItem, error) {
	tokens, err := s.repos.APITokens.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]ReflectScopesAllItem, 0, len(tokens))
	for _, tok := range tokens {
		if tok.RevokedAt != nil {
			continue
		}
		result, err := s.ReflectScopes(ctx, tok.ID, changedByActorID)
		items = append(items, ReflectScopesAllItem{TokenID: tok.ID, Result: result, Err: err})
	}
	return items, nil
}

type CheckResult struct {
	Token            string
	OwnerActorID     string
	OwnerCreatedAt   time.Time
	OwnerUsername    string
	OwnerDisplayName string
	// OwnerAvatarFileID mirrors OwnerProfile's own field (Issue #77
	// PR5) — see that type's doc comment.
	OwnerAvatarFileID *string
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
			OwnerUsername: s.cfg.OwnerUsername, OwnerDisplayName: displayNameOrEmpty(owner.DisplayName),
			OwnerAvatarFileID: owner.AvatarFileID,
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
	// AvatarFileID names a files row (Issue #77 PR5), or nil if the
	// owner has never set one. internal/httpserver resolves it to an
	// absolute GET /files/{id} URL for the wire's avatarUrl field —
	// never a bare avatarId key (userDetailedNotMe's own guardrail).
	AvatarFileID *string
}

// DescribeOwner returns the owner's profile for actorID, the local actor
// ID VerifyToken resolves a verified API token to. This deployment only
// ever issues local API tokens to the single bound owner (AGENTS.md: no
// general user login), so any actorID reaching here from
// httpserver.LocalActorIDFromContext already names the owner actor; this
// just re-fetches its CreatedAt/display name and layers the configured
// username on top, the same projection CheckResult carries right after a
// successful Check. Since Issue #23 PR1, DisplayName comes from the
// actors row (mutable via UpdateOwnerDisplayName), not from config;
// Username remains config-sourced permanently (see UpdateOwnerDisplayName's
// doc comment for why).
func (s *Service) DescribeOwner(ctx context.Context, actorID string) (OwnerProfile, error) {
	actor, err := s.repos.Actors.Get(ctx, actorID)
	if err != nil {
		return OwnerProfile{}, err
	}
	return OwnerProfile{
		ActorID:      actor.ID,
		Username:     s.cfg.OwnerUsername,
		DisplayName:  displayNameOrEmpty(actor.DisplayName),
		CreatedAt:    actor.CreatedAt,
		AvatarFileID: actor.AvatarFileID,
	}, nil
}

// UpdateOwnerDisplayName is POST /api/i/update's use case (Issue #23
// PR1). displayName is applied verbatim, including "" to clear it.
// actorID must already be RequireScope-verified; it is re-checked
// against the Owner actor type here rather than trusted blindly, so a
// hypothetical non-owner-bound token (see ErrNotOwner's doc comment)
// can never reach this write path. There is no equivalent
// UpdateOwnerUsername: docs/compat/aria-v1.5.11.md's "POST /api/i/update"
// section records that neither Aria nor the pinned misskey_dart client
// has any way to send a username field to this endpoint, so
// OWNER_USERNAME config remains the permanent source of truth for it.
func (s *Service) UpdateOwnerDisplayName(ctx context.Context, actorID, displayName string) (OwnerProfile, error) {
	actor, err := s.repos.Actors.Get(ctx, actorID)
	if err != nil {
		return OwnerProfile{}, err
	}
	if actor.Type != domain.ActorOwner {
		return OwnerProfile{}, ErrNotOwner
	}
	if err := s.repos.Actors.SetDisplayName(ctx, actorID, displayName); err != nil {
		return OwnerProfile{}, err
	}
	return OwnerProfile{
		ActorID:      actor.ID,
		Username:     s.cfg.OwnerUsername,
		DisplayName:  displayName,
		CreatedAt:    actor.CreatedAt,
		AvatarFileID: actor.AvatarFileID,
	}, nil
}

// UpdateOwnerAvatar is POST /api/i/update's avatarId field (Issue #77
// PR5). fileID is applied verbatim, including nil to clear it — PR0's
// trace of Aria's INotifier.setAvatarId found it sends an explicit
// `avatarId: null` to remove the avatar (opting out of the client's
// usual null-omission convention specifically for this field), so nil
// here must clear rather than be rejected as invalid. Unlike
// UpdateOwnerDisplayName, this package does not validate that fileID
// names a files row the owner actually owns — internal/httpserver does,
// through internal/drive.Service.ShowFile, before ever calling this
// (this package has no internal/drive dependency, and none of Drive's
// ownership/existence semantics belong in a bare actor-column setter).
func (s *Service) UpdateOwnerAvatar(ctx context.Context, actorID string, fileID *string) (OwnerProfile, error) {
	actor, err := s.repos.Actors.Get(ctx, actorID)
	if err != nil {
		return OwnerProfile{}, err
	}
	if actor.Type != domain.ActorOwner {
		return OwnerProfile{}, ErrNotOwner
	}
	if err := s.repos.Actors.SetAvatarFileID(ctx, actorID, fileID); err != nil {
		return OwnerProfile{}, err
	}
	return OwnerProfile{
		ActorID:      actor.ID,
		Username:     s.cfg.OwnerUsername,
		DisplayName:  displayNameOrEmpty(actor.DisplayName),
		CreatedAt:    actor.CreatedAt,
		AvatarFileID: fileID,
	}, nil
}

// BackfillOwnerDisplayName seeds the Owner actor's display_name from
// cfg.OwnerDisplayName when the Owner already exists but has never had
// display_name explicitly set (sql NULL, not ""). Two real paths reach
// that state: an Owner row bound before Issue #23 PR1's migration 0012
// added the column, and an Owner row bound by a caller whose Config left
// OwnerDisplayName unset (cmd/miauthctl performs the actual owner-binding
// ApproveSession call in production, so its Config must be wired for
// this to have any effect there). It is a no-op, not an error, if the
// Owner does not exist yet: ensureOwnerActor seeds it correctly at bind
// time in that case, and if display_name is already non-nil (including
// "" from an explicit clear via UpdateOwnerDisplayName), it is left
// untouched rather than being overwritten back to the config value.
func (s *Service) BackfillOwnerDisplayName(ctx context.Context) error {
	owner, err := s.repos.Actors.GetByType(ctx, domain.ActorOwner)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	if owner.DisplayName != nil {
		return nil
	}
	return s.repos.Actors.SetDisplayName(ctx, owner.ID, s.cfg.OwnerDisplayName)
}

// displayNameOrEmpty adapts a domain.Actor.DisplayName (nil until
// explicitly set) to OwnerProfile/CheckResult's plain-string convention,
// where "" already means "no display name" to their wire projections
// (see internal/httpserver's newUserDetailedNotMe).
func displayNameOrEmpty(dn *string) string {
	if dn == nil {
		return ""
	}
	return *dn
}
