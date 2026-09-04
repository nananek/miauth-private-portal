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
	GetByTokenHash(ctx context.Context, tokenHash string) (APIToken, error)
	Revoke(ctx context.Context, id string, at time.Time) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
	List(ctx context.Context) ([]APIToken, error)
}
