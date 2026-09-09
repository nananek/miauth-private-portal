package domain

import (
	"context"
	"time"
)

// MiAuthStatus is the local MiAuth session state machine defined by
// ADR-0002: created -> authorized -> consumed, with expired/denied as
// terminal states.
type MiAuthStatus string

const (
	MiAuthCreated    MiAuthStatus = "created"
	MiAuthAuthorized MiAuthStatus = "authorized"
	MiAuthConsumed   MiAuthStatus = "consumed"
	MiAuthExpired    MiAuthStatus = "expired"
	MiAuthDenied     MiAuthStatus = "denied"
)

// LocalMiAuthSession is the Aria-facing MiAuth attempt. RouteSessionID is
// the opaque bearer correlation secret supplied by Aria. Possessing it
// permits polling this attempt but does not authorize it; authorization
// requires an explicit operator action through miauthctl.
type LocalMiAuthSession struct {
	RouteSessionID       string
	Status               MiAuthStatus
	RequestedPermissions string
	ClientCallback       *string
	LocalActorID         *string
	CreatedAt            time.Time
	ExpiresAt            time.Time
	AuthorizedAt         *time.Time
	ConsumedAt           *time.Time
}

// APIToken is a local API token issued to Aria after a successful local
// MiAuth check. Only its one-way TokenHash is persisted.
type APIToken struct {
	ID                   string
	TokenHash            string
	LocalActorID         string
	MiAuthLocalSessionID *string
	Scopes               string
	CreatedAt            time.Time
	RevokedAt            *time.Time
	LastUsedAt           *time.Time
}

// LocalMiAuthSessionRepository persists Aria-facing local MiAuth sessions.
type LocalMiAuthSessionRepository interface {
	Create(ctx context.Context, s LocalMiAuthSession) error
	Get(ctx context.Context, routeSessionID string) (LocalMiAuthSession, error)
	Authorize(ctx context.Context, routeSessionID, localActorID string, at time.Time) error
	Consume(ctx context.Context, routeSessionID string, at time.Time) (LocalMiAuthSession, error)
	Deny(ctx context.Context, routeSessionID string, at time.Time) error
	ListPending(ctx context.Context, now time.Time) ([]LocalMiAuthSession, error)
}

// APITokenRepository persists local API tokens.
type APITokenRepository interface {
	Create(ctx context.Context, t APIToken) error
	// Get returns id's token, or ErrNotFound. Every other single-entity
	// repository in this codebase (Actors.Get, Config.Get, ...) already
	// has a Get by ID; ReflectScopes needs one too rather than scanning
	// the whole table via List for a single row.
	Get(ctx context.Context, id string) (APIToken, error)
	GetByTokenHash(ctx context.Context, tokenHash string) (APIToken, error)
	Revoke(ctx context.Context, id string, at time.Time) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
	List(ctx context.Context) ([]APIToken, error)
	// UpdateScopes overwrites id's stored scopes (miauth.Service.ReflectScopes'
	// write path). The WHERE clause requires the token to still be
	// non-revoked; ErrNotFound is returned both when id does not exist at
	// all and when it exists but is revoked, so callers that already
	// confirmed the token exists (as ReflectScopes does, via Get, inside
	// the same transaction) can treat ErrNotFound from UpdateScopes as
	// "revoked concurrently" — see ReflectScopes' doc comment.
	UpdateScopes(ctx context.Context, id, scopes string) error
}

// APITokenScopeAuditEntry is one row of the api_token_scope_audit table:
// an immutable record of a single ReflectScopes write, in the same
// transaction as the api_tokens.scopes UPDATE it describes — the same
// "audit and effect can never disagree" guarantee AppConfigAuditEntry
// gives app_config, applied to token scopes (Issue #133).
type APITokenScopeAuditEntry struct {
	ID        string
	TokenID   string
	OldScopes string
	NewScopes string
	ChangedAt time.Time
	// ChangedBy is the owner actor id — the CLI operator, per ADR-0002's
	// "a host-access CLI operator is always attributed to the one local
	// owner actor" (mirrors AppConfigAuditEntry.ChangedBy / ownerActorID).
	ChangedBy string
}

// APITokenScopeAuditRepository persists ReflectScopes' change history.
type APITokenScopeAuditRepository interface {
	Record(ctx context.Context, entry APITokenScopeAuditEntry) error
	// ListByToken returns tokenID's full reflect-scopes history, oldest
	// first, ordered by (ChangedAt, ID) — mirrors
	// ConfigAuditRepository.ListByKey's ordering rationale.
	ListByToken(ctx context.Context, tokenID string) ([]APITokenScopeAuditEntry, error)
}
