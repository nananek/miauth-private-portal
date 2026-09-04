package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type localMiAuthSessionRepository struct{ q querier }

const localMiAuthColumns = `route_session_id, status, requested_permissions,
	client_callback, local_actor_id, created_at, expires_at, authorized_at, consumed_at`

const localMiAuthSelectColumns = `SELECT ` + localMiAuthColumns + ` FROM miauth_local_sessions`

func (r *localMiAuthSessionRepository) Create(ctx context.Context, s domain.LocalMiAuthSession) error {
	// Migration 0010 deliberately leaves the legacy state column in place:
	// SQLite cannot DROP a column covered by a UNIQUE constraint. It is no
	// longer a domain credential, but old and new databases still require a
	// unique non-empty compatibility value. Application code never reads it.
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO miauth_local_sessions (route_session_id, state, status, requested_permissions,
			client_callback, local_actor_id, created_at, expires_at, authorized_at, consumed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.RouteSessionID, domain.NewID(), string(s.Status), s.RequestedPermissions, nullableString(s.ClientCallback),
		nullableString(s.LocalActorID), formatTime(s.CreatedAt), formatTime(s.ExpiresAt),
		formatTimePtr(s.AuthorizedAt), formatTimePtr(s.ConsumedAt),
	)
	return mapWriteError(err)
}

func (r *localMiAuthSessionRepository) Get(ctx context.Context, routeSessionID string) (domain.LocalMiAuthSession, error) {
	return scanLocalMiAuthSession(r.q.QueryRowContext(ctx, localMiAuthSelectColumns+` WHERE route_session_id = ?`, routeSessionID))
}

func (r *localMiAuthSessionRepository) ListPending(ctx context.Context, now time.Time) ([]domain.LocalMiAuthSession, error) {
	rows, err := r.q.QueryContext(ctx, localMiAuthSelectColumns+
		` WHERE status = 'created' AND expires_at > ? ORDER BY created_at DESC, route_session_id`, formatTime(now))
	if err != nil {
		return nil, mapReadError(err)
	}
	defer rows.Close()

	sessions := make([]domain.LocalMiAuthSession, 0)
	for rows.Next() {
		session, err := scanLocalMiAuthSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, mapReadError(err)
	}
	return sessions, nil
}

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

func (r *localMiAuthSessionRepository) Consume(ctx context.Context, routeSessionID string, at time.Time) (domain.LocalMiAuthSession, error) {
	row := r.q.QueryRowContext(ctx,
		`UPDATE miauth_local_sessions SET status = 'consumed', consumed_at = ?
		 WHERE route_session_id = ? AND status = 'authorized' AND expires_at > ?
		 RETURNING `+localMiAuthColumns,
		formatTime(at), routeSessionID, formatTime(at),
	)
	session, err := scanLocalMiAuthSession(row)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.LocalMiAuthSession{}, domain.ErrConflict
	}
	return session, err
}

func (r *localMiAuthSessionRepository) Deny(ctx context.Context, routeSessionID string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE miauth_local_sessions SET status = 'denied'
		 WHERE route_session_id = ? AND status = 'created' AND expires_at > ?`,
		routeSessionID, formatTime(at),
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

	if err := row.Scan(&s.RouteSessionID, &status, &s.RequestedPermissions, &clientCallback,
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

type apiTokenRepository struct{ q querier }

const apiTokenSelectColumns = `SELECT id, token_hash, local_actor_id, miauth_local_session_id, scopes,
	created_at, revoked_at, last_used_at FROM api_tokens`

func (r *apiTokenRepository) Create(ctx context.Context, t domain.APIToken) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO api_tokens (id, token_hash, local_actor_id, miauth_local_session_id, scopes,
			created_at, revoked_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TokenHash, t.LocalActorID, nullableString(t.MiAuthLocalSessionID), t.Scopes,
		formatTime(t.CreatedAt), formatTimePtr(t.RevokedAt), formatTimePtr(t.LastUsedAt),
	)
	return mapWriteError(err)
}

func (r *apiTokenRepository) GetByTokenHash(ctx context.Context, tokenHash string) (domain.APIToken, error) {
	return scanAPIToken(r.q.QueryRowContext(ctx, apiTokenSelectColumns+` WHERE token_hash = ?`, tokenHash))
}

func (r *apiTokenRepository) List(ctx context.Context) ([]domain.APIToken, error) {
	rows, err := r.q.QueryContext(ctx, apiTokenSelectColumns+` ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, mapReadError(err)
	}
	defer rows.Close()
	tokens := make([]domain.APIToken, 0)
	for rows.Next() {
		token, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, mapReadError(err)
	}
	return tokens, nil
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
	var sessionID sql.NullString
	var createdAt string
	var revokedAt, lastUsedAt sql.NullString
	if err := row.Scan(&t.ID, &t.TokenHash, &t.LocalActorID, &sessionID, &t.Scopes,
		&createdAt, &revokedAt, &lastUsedAt); err != nil {
		return domain.APIToken{}, mapReadError(err)
	}
	t.MiAuthLocalSessionID = stringPtr(sessionID)
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
