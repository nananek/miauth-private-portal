package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// --- bootstrap gates ---

type bootstrapGateRepository struct{ q querier }

func (r *bootstrapGateRepository) Create(ctx context.Context, g domain.BootstrapGate) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO bootstrap_gates (id, status, created_at, expires_at, consumed_at) VALUES (?, ?, ?, ?, ?)`,
		g.ID, string(g.Status), formatTime(g.CreatedAt), formatTime(g.ExpiresAt), formatTimePtr(g.ConsumedAt),
	)
	return mapWriteError(err)
}

func (r *bootstrapGateRepository) Get(ctx context.Context, id string) (domain.BootstrapGate, error) {
	row := r.q.QueryRowContext(ctx,
		`SELECT id, status, created_at, expires_at, consumed_at FROM bootstrap_gates WHERE id = ?`, id)
	return scanBootstrapGate(row)
}

// Consume atomically transitions an issued, unexpired gate to consumed.
// The WHERE clause encodes the expected prior state, so a concurrent
// second consume attempt affects zero rows and reports ErrConflict.
func (r *bootstrapGateRepository) Consume(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE bootstrap_gates SET status = 'consumed', consumed_at = ?
		 WHERE id = ? AND status = 'issued' AND expires_at > ?`,
		formatTime(at), id, formatTime(at),
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// Fail atomically transitions an issued, unexpired gate to failed, for a
// bootstrap attempt whose upstream verification did not succeed.
func (r *bootstrapGateRepository) Fail(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE bootstrap_gates SET status = 'failed'
		 WHERE id = ? AND status = 'issued' AND expires_at > ?`,
		id, formatTime(at),
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func scanBootstrapGate(row rowScanner) (domain.BootstrapGate, error) {
	var g domain.BootstrapGate
	var status, createdAt, expiresAt string
	var consumedAt sql.NullString
	if err := row.Scan(&g.ID, &status, &createdAt, &expiresAt, &consumedAt); err != nil {
		return domain.BootstrapGate{}, mapReadError(err)
	}
	g.Status = domain.BootstrapGateStatus(status)
	var err error
	if g.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.BootstrapGate{}, err
	}
	if g.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return domain.BootstrapGate{}, err
	}
	if g.ConsumedAt, err = parseTimePtr(consumedAt); err != nil {
		return domain.BootstrapGate{}, err
	}
	return g, nil
}

// --- local (Aria-facing) MiAuth sessions ---

type localMiAuthSessionRepository struct{ q querier }

const localMiAuthSelectColumns = `SELECT route_session_id, state, status, requested_permissions,
	client_callback, local_actor_id, created_at, expires_at, authorized_at, consumed_at
	FROM miauth_local_sessions`

func (r *localMiAuthSessionRepository) Create(ctx context.Context, s domain.LocalMiAuthSession) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO miauth_local_sessions (route_session_id, state, status, requested_permissions,
			client_callback, local_actor_id, created_at, expires_at, authorized_at, consumed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.RouteSessionID, s.State, string(s.Status), s.RequestedPermissions, nullableString(s.ClientCallback),
		nullableString(s.LocalActorID), formatTime(s.CreatedAt), formatTime(s.ExpiresAt),
		formatTimePtr(s.AuthorizedAt), formatTimePtr(s.ConsumedAt),
	)
	return mapWriteError(err)
}

func (r *localMiAuthSessionRepository) Get(ctx context.Context, routeSessionID string) (domain.LocalMiAuthSession, error) {
	row := r.q.QueryRowContext(ctx, localMiAuthSelectColumns+` WHERE route_session_id = ?`, routeSessionID)
	return scanLocalMiAuthSession(row)
}

// Authorize atomically transitions a created, unexpired session to
// authorized. The WHERE clause's status/expiry check is what makes this
// safe against a session that already expired or was already authorized.
func (r *localMiAuthSessionRepository) Authorize(ctx context.Context, routeSessionID, localActorID string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE miauth_local_sessions SET status = 'authorized', local_actor_id = ?, authorized_at = ?
		 WHERE route_session_id = ? AND status = 'created' AND expires_at > ?`,
		localActorID, formatTime(at), routeSessionID, formatTime(at),
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// Consume atomically transitions an authorized session to consumed. If
// two check() calls race, exactly one UPDATE affects a row; the other
// affects zero and reports ErrConflict.
func (r *localMiAuthSessionRepository) Consume(ctx context.Context, routeSessionID string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE miauth_local_sessions SET status = 'consumed', consumed_at = ?
		 WHERE route_session_id = ? AND status = 'authorized' AND expires_at > ?`,
		formatTime(at), routeSessionID, formatTime(at),
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// Deny atomically transitions a created session to denied, for a
// callback whose verification did not succeed (wrong user, state
// mismatch, malformed or replayed callback). See the interface doc for
// why this is a distinct terminal write rather than leaving the session
// to expire.
func (r *localMiAuthSessionRepository) Deny(ctx context.Context, routeSessionID string) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE miauth_local_sessions SET status = 'denied' WHERE route_session_id = ? AND status = 'created'`,
		routeSessionID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func scanLocalMiAuthSession(row rowScanner) (domain.LocalMiAuthSession, error) {
	var s domain.LocalMiAuthSession
	var status string
	var clientCallback, localActorID sql.NullString
	var createdAt, expiresAt string
	var authorizedAt, consumedAt sql.NullString

	if err := row.Scan(&s.RouteSessionID, &s.State, &status, &s.RequestedPermissions, &clientCallback,
		&localActorID, &createdAt, &expiresAt, &authorizedAt, &consumedAt); err != nil {
		return domain.LocalMiAuthSession{}, mapReadError(err)
	}

	s.Status = domain.MiAuthStatus(status)
	s.ClientCallback = stringPtr(clientCallback)
	s.LocalActorID = stringPtr(localActorID)

	var err error
	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.LocalMiAuthSession{}, err
	}
	if s.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return domain.LocalMiAuthSession{}, err
	}
	if s.AuthorizedAt, err = parseTimePtr(authorizedAt); err != nil {
		return domain.LocalMiAuthSession{}, err
	}
	if s.ConsumedAt, err = parseTimePtr(consumedAt); err != nil {
		return domain.LocalMiAuthSession{}, err
	}
	return s, nil
}

// --- upstream (owner-verification) MiAuth sessions ---

type upstreamMiAuthSessionRepository struct{ q querier }

const upstreamMiAuthSelectColumns = `SELECT id, local_session_id, bootstrap_gate_id, identity_origin, state,
	status, upstream_user_id, created_at, expires_at, authorized_at, consumed_at
	FROM miauth_upstream_sessions`

func (r *upstreamMiAuthSessionRepository) Create(ctx context.Context, s domain.UpstreamMiAuthSession) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO miauth_upstream_sessions (id, local_session_id, bootstrap_gate_id, identity_origin,
			state, status, upstream_user_id, created_at, expires_at, authorized_at, consumed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, nullableString(s.LocalSessionID), nullableString(s.BootstrapGateID), s.IdentityOrigin,
		s.State, string(s.Status), nullableString(s.UpstreamUserID), formatTime(s.CreatedAt),
		formatTime(s.ExpiresAt), formatTimePtr(s.AuthorizedAt), formatTimePtr(s.ConsumedAt),
	)
	return mapWriteError(err)
}

func (r *upstreamMiAuthSessionRepository) Get(ctx context.Context, id string) (domain.UpstreamMiAuthSession, error) {
	row := r.q.QueryRowContext(ctx, upstreamMiAuthSelectColumns+` WHERE id = ?`, id)
	return scanUpstreamMiAuthSession(row)
}

// GetByLocalSessionID backs idempotent resume of GET /miauth/{session}:
// local_session_id is UNIQUE, so at most one row can match.
func (r *upstreamMiAuthSessionRepository) GetByLocalSessionID(ctx context.Context, localSessionID string) (domain.UpstreamMiAuthSession, error) {
	row := r.q.QueryRowContext(ctx, upstreamMiAuthSelectColumns+` WHERE local_session_id = ?`, localSessionID)
	return scanUpstreamMiAuthSession(row)
}

// GetByBootstrapGateID is GetByLocalSessionID's counterpart for the
// operator bootstrap flow: bootstrap_gate_id is UNIQUE.
func (r *upstreamMiAuthSessionRepository) GetByBootstrapGateID(ctx context.Context, bootstrapGateID string) (domain.UpstreamMiAuthSession, error) {
	row := r.q.QueryRowContext(ctx, upstreamMiAuthSelectColumns+` WHERE bootstrap_gate_id = ?`, bootstrapGateID)
	return scanUpstreamMiAuthSession(row)
}

func (r *upstreamMiAuthSessionRepository) Authorize(ctx context.Context, id, upstreamUserID string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE miauth_upstream_sessions SET status = 'authorized', upstream_user_id = ?, authorized_at = ?
		 WHERE id = ? AND status = 'created' AND expires_at > ?`,
		upstreamUserID, formatTime(at), id, formatTime(at),
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *upstreamMiAuthSessionRepository) Consume(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE miauth_upstream_sessions SET status = 'consumed', consumed_at = ?
		 WHERE id = ? AND status = 'authorized' AND expires_at > ?`,
		formatTime(at), id, formatTime(at),
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// Deny atomically transitions a created session to denied — see
// localMiAuthSessionRepository.Deny for why this is a distinct terminal
// write rather than leaving the session to expire.
func (r *upstreamMiAuthSessionRepository) Deny(ctx context.Context, id string) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE miauth_upstream_sessions SET status = 'denied' WHERE id = ? AND status = 'created'`,
		id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func scanUpstreamMiAuthSession(row rowScanner) (domain.UpstreamMiAuthSession, error) {
	var s domain.UpstreamMiAuthSession
	var localSessionID, bootstrapGateID, upstreamUserID sql.NullString
	var status string
	var createdAt, expiresAt string
	var authorizedAt, consumedAt sql.NullString

	if err := row.Scan(&s.ID, &localSessionID, &bootstrapGateID, &s.IdentityOrigin, &s.State, &status,
		&upstreamUserID, &createdAt, &expiresAt, &authorizedAt, &consumedAt); err != nil {
		return domain.UpstreamMiAuthSession{}, mapReadError(err)
	}

	s.LocalSessionID = stringPtr(localSessionID)
	s.BootstrapGateID = stringPtr(bootstrapGateID)
	s.UpstreamUserID = stringPtr(upstreamUserID)
	s.Status = domain.MiAuthStatus(status)

	var err error
	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.UpstreamMiAuthSession{}, err
	}
	if s.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return domain.UpstreamMiAuthSession{}, err
	}
	if s.AuthorizedAt, err = parseTimePtr(authorizedAt); err != nil {
		return domain.UpstreamMiAuthSession{}, err
	}
	if s.ConsumedAt, err = parseTimePtr(consumedAt); err != nil {
		return domain.UpstreamMiAuthSession{}, err
	}
	return s, nil
}

// --- local API tokens ---

type apiTokenRepository struct{ q querier }

const apiTokenSelectColumns = `SELECT id, token_hash, local_actor_id, miauth_local_session_id, scopes,
	created_at, revoked_at, last_used_at FROM api_tokens`

func (r *apiTokenRepository) Create(ctx context.Context, t domain.APIToken) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO api_tokens (id, token_hash, local_actor_id, miauth_local_session_id, scopes,
			created_at, revoked_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TokenHash, t.LocalActorID, nullableString(t.MiAuthLocalSessionID), t.Scopes,
		formatTime(t.CreatedAt), formatTimePtr(t.RevokedAt), formatTimePtr(t.LastUsedAt),
	)
	return mapWriteError(err)
}

func (r *apiTokenRepository) GetByTokenHash(ctx context.Context, tokenHash string) (domain.APIToken, error) {
	row := r.q.QueryRowContext(ctx, apiTokenSelectColumns+` WHERE token_hash = ?`, tokenHash)
	return scanAPIToken(row)
}

func (r *apiTokenRepository) Revoke(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *apiTokenRepository) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	res, err := r.q.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanAPIToken(row rowScanner) (domain.APIToken, error) {
	var t domain.APIToken
	var miauthLocalSessionID sql.NullString
	var createdAt string
	var revokedAt, lastUsedAt sql.NullString

	if err := row.Scan(&t.ID, &t.TokenHash, &t.LocalActorID, &miauthLocalSessionID, &t.Scopes,
		&createdAt, &revokedAt, &lastUsedAt); err != nil {
		return domain.APIToken{}, mapReadError(err)
	}

	t.MiAuthLocalSessionID = stringPtr(miauthLocalSessionID)

	var err error
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.APIToken{}, err
	}
	if t.RevokedAt, err = parseTimePtr(revokedAt); err != nil {
		return domain.APIToken{}, err
	}
	if t.LastUsedAt, err = parseTimePtr(lastUsedAt); err != nil {
		return domain.APIToken{}, err
	}
	return t, nil
}

// --- owner binding ---

type ownerBindingRepository struct{ q querier }

// Bind creates the singleton owner_bindings row (fixed id = 1). A second
// concurrent Bind fails on that primary key collision and is mapped to
// domain.ErrConflict by mapWriteError: SQL alone is the compare-and-set
// enforcement ADR-0001 requires.
func (r *ownerBindingRepository) Bind(ctx context.Context, b domain.OwnerBinding) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO owner_bindings (id, local_actor_id, identity_origin, upstream_user_id, bound_at)
		 VALUES (1, ?, ?, ?, ?)`,
		b.LocalActorID, b.IdentityOrigin, b.UpstreamUserID, formatTime(b.BoundAt),
	)
	return mapWriteError(err)
}

func (r *ownerBindingRepository) Get(ctx context.Context) (domain.OwnerBinding, error) {
	row := r.q.QueryRowContext(ctx,
		`SELECT local_actor_id, identity_origin, upstream_user_id, bound_at FROM owner_bindings WHERE id = 1`)
	var b domain.OwnerBinding
	var boundAt string
	if err := row.Scan(&b.LocalActorID, &b.IdentityOrigin, &b.UpstreamUserID, &boundAt); err != nil {
		return domain.OwnerBinding{}, mapReadError(err)
	}
	var err error
	if b.BoundAt, err = parseTime(boundAt); err != nil {
		return domain.OwnerBinding{}, err
	}
	return b, nil
}

// --- upstream token ---

type upstreamTokenRepository struct{ q querier }

// Put upserts the single upstream_tokens row keyed to the owner binding
// (always id 1, since owner_bindings has at most one row).
func (r *upstreamTokenRepository) Put(ctx context.Context, t domain.UpstreamToken) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO upstream_tokens (owner_binding_id, ciphertext, nonce, key_version, created_at, rotated_at)
		 VALUES (1, ?, ?, ?, ?, ?)
		 ON CONFLICT (owner_binding_id) DO UPDATE SET
			ciphertext = excluded.ciphertext,
			nonce = excluded.nonce,
			key_version = excluded.key_version,
			rotated_at = excluded.rotated_at`,
		t.Ciphertext, t.Nonce, t.KeyVersion, formatTime(t.CreatedAt), formatTimePtr(t.RotatedAt),
	)
	return mapWriteError(err)
}

func (r *upstreamTokenRepository) Get(ctx context.Context) (domain.UpstreamToken, error) {
	row := r.q.QueryRowContext(ctx,
		`SELECT ciphertext, nonce, key_version, created_at, rotated_at FROM upstream_tokens WHERE owner_binding_id = 1`)
	var t domain.UpstreamToken
	var createdAt string
	var rotatedAt sql.NullString
	if err := row.Scan(&t.Ciphertext, &t.Nonce, &t.KeyVersion, &createdAt, &rotatedAt); err != nil {
		return domain.UpstreamToken{}, mapReadError(err)
	}
	var err error
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.UpstreamToken{}, err
	}
	if t.RotatedAt, err = parseTimePtr(rotatedAt); err != nil {
		return domain.UpstreamToken{}, err
	}
	return t, nil
}

func (r *upstreamTokenRepository) Delete(ctx context.Context) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM upstream_tokens WHERE owner_binding_id = 1`)
	return mapWriteError(err)
}
