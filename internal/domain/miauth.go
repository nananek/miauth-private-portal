package domain

import (
	"context"
	"time"
)

// MiAuthStatus is the state machine ADR-0001 defines for both local and
// upstream MiAuth sessions: created -> authorized -> consumed, with
// expired/denied as the other terminal states.
type MiAuthStatus string

const (
	MiAuthCreated    MiAuthStatus = "created"
	MiAuthAuthorized MiAuthStatus = "authorized"
	MiAuthConsumed   MiAuthStatus = "consumed"
	MiAuthExpired    MiAuthStatus = "expired"
	MiAuthDenied     MiAuthStatus = "denied"
)

// BootstrapGateStatus is the state machine ADR-0001 defines for the
// single-use operator bootstrap gate: issued -> consumed, with
// expired/revoked/failed as the other terminal states.
type BootstrapGateStatus string

const (
	BootstrapGateIssued   BootstrapGateStatus = "issued"
	BootstrapGateConsumed BootstrapGateStatus = "consumed"
	BootstrapGateExpired  BootstrapGateStatus = "expired"
	BootstrapGateRevoked  BootstrapGateStatus = "revoked"
	BootstrapGateFailed   BootstrapGateStatus = "failed"
)

// BootstrapGate is the single-use, time-boxed gate an operator presents to
// bind the initial owner when ALLOWED_MISSKEY_USER_ID is unset (ADR-0001
// §2).
type BootstrapGate struct {
	// ID is the crypto/rand secret shown only through the operator
	// channel; it is also this record's primary key.
	ID         string
	Status     BootstrapGateStatus
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

// LocalMiAuthSession is the Aria-facing MiAuth attempt. RouteSessionID is
// the opaque bearer correlation secret Aria supplies in its
// /miauth/{session} URL; State is a separate server-generated crypto/rand
// value bound to it (ADR-0001 §3). Possessing RouteSessionID permits
// polling/checking this attempt only; it does not authenticate the owner.
type LocalMiAuthSession struct {
	RouteSessionID       string
	State                string
	Status               MiAuthStatus
	RequestedPermissions string
	// ClientCallback is Aria's exact-match-allowlisted return callback,
	// if it supplied one.
	ClientCallback *string
	// LocalActorID is set once this session is authorized.
	LocalActorID *string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	AuthorizedAt *time.Time
	ConsumedAt   *time.Time
}

// UpstreamMiAuthSession is the owner-verification MiAuth attempt against
// the configured IDENTITY_ORIGIN. It is bound either to a
// LocalMiAuthSession (the normal Aria-triggered flow) or to a
// BootstrapGate (the operator-controlled bootstrap flow), never both and
// never neither.
type UpstreamMiAuthSession struct {
	ID              string
	LocalSessionID  *string
	BootstrapGateID *string
	IdentityOrigin  string
	State           string
	Status          MiAuthStatus
	// UpstreamUserID is set once this session is authorized.
	UpstreamUserID *string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	AuthorizedAt   *time.Time
	ConsumedAt     *time.Time
}

// APIToken is a local API token issued to Aria after a successful local
// MiAuth check. Only its one-way TokenHash is ever persisted; the raw
// token value exists only long enough to be returned once and is never
// logged or stored.
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

// OwnerBinding is the single-row record of which upstream identity owns
// this deployment. OwnerBindingRepository.Bind enforces that at most one
// binding can ever be created, so a later verification can never silently
// replace it.
type OwnerBinding struct {
	LocalActorID   string
	IdentityOrigin string
	UpstreamUserID string
	BoundAt        time.Time
}

// UpstreamToken is the owner's upstream Misskey token, persisted only if
// an adapter needs it to survive past a single request. It is encrypted
// at rest and is never returned to Aria.
type UpstreamToken struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion string
	CreatedAt  time.Time
	RotatedAt  *time.Time
}

// BootstrapGateRepository persists operator bootstrap gates.
type BootstrapGateRepository interface {
	Create(ctx context.Context, g BootstrapGate) error
	Get(ctx context.Context, id string) (BootstrapGate, error)
	// Consume atomically transitions an issued, unexpired gate to
	// consumed. It returns ErrConflict if the gate is missing, expired,
	// or already in a terminal state.
	Consume(ctx context.Context, id string, at time.Time) error
	// Fail atomically transitions an issued, unexpired gate to failed,
	// for a bootstrap attempt whose upstream verification did not
	// succeed. ADR-0001 requires the gate be invalid after a failed
	// binding attempt, not just after expiry or explicit revocation. It
	// returns ErrConflict if the gate is missing, expired, or already in
	// a terminal state.
	Fail(ctx context.Context, id string, at time.Time) error
}

// LocalMiAuthSessionRepository persists Aria-facing local MiAuth sessions.
type LocalMiAuthSessionRepository interface {
	Create(ctx context.Context, s LocalMiAuthSession) error
	Get(ctx context.Context, routeSessionID string) (LocalMiAuthSession, error)
	Authorize(ctx context.Context, routeSessionID, localActorID string, at time.Time) error
	// Consume atomically transitions an authorized session to consumed.
	// It returns ErrConflict if the session is not currently authorized
	// (already consumed, expired, or never authorized) — the "only one
	// winner" guarantee a racing check() call needs.
	Consume(ctx context.Context, routeSessionID string, at time.Time) error
	// Deny atomically transitions a created session to denied. ADR-0001
	// treats an unknown, malformed, replayed, mismatched, or otherwise
	// rejected upstream callback as a terminal failure for that attempt,
	// not something left to expire naturally, so a wrong-user or
	// state-mismatch callback must call this instead of leaving the
	// session retryable. It returns ErrConflict if the session is not
	// currently in the created state (already authorized, consumed, or
	// previously denied).
	Deny(ctx context.Context, routeSessionID string) error
}

// UpstreamMiAuthSessionRepository persists upstream owner-verification
// MiAuth sessions.
type UpstreamMiAuthSessionRepository interface {
	Create(ctx context.Context, s UpstreamMiAuthSession) error
	Get(ctx context.Context, id string) (UpstreamMiAuthSession, error)
	Authorize(ctx context.Context, id, upstreamUserID string, at time.Time) error
	// Consume atomically transitions an authorized session to consumed.
	// It returns ErrConflict if the session is not currently authorized.
	Consume(ctx context.Context, id string, at time.Time) error
	// Deny atomically transitions a created session to denied — see
	// LocalMiAuthSessionRepository.Deny for why this is a distinct
	// terminal write rather than leaving the session to expire. It
	// returns ErrConflict if the session is not currently in the created
	// state.
	Deny(ctx context.Context, id string) error
}

// APITokenRepository persists local API tokens.
type APITokenRepository interface {
	Create(ctx context.Context, t APIToken) error
	GetByTokenHash(ctx context.Context, tokenHash string) (APIToken, error)
	Revoke(ctx context.Context, id string, at time.Time) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
}

// OwnerBindingRepository persists the single owner-binding record.
type OwnerBindingRepository interface {
	// Bind creates the singleton owner binding. It returns ErrConflict if
	// a binding already exists — the atomic compare-and-set under a
	// uniqueness constraint ADR-0001 requires so a concurrent bootstrap
	// race has exactly one winner.
	Bind(ctx context.Context, b OwnerBinding) error
	Get(ctx context.Context) (OwnerBinding, error)
}

// UpstreamTokenRepository persists the single encrypted upstream token,
// when persistence is unavoidable.
type UpstreamTokenRepository interface {
	Put(ctx context.Context, t UpstreamToken) error
	Get(ctx context.Context) (UpstreamToken, error)
	Delete(ctx context.Context) error
}
