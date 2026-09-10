package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type webAdminBootstrapTokenRepository struct{ q querier }

const webAdminBootstrapTokenColumns = `id, token_hash, owner_actor_id, status, webauthn_session_data, created_at, expires_at, consumed_at`

func (r *webAdminBootstrapTokenRepository) Create(ctx context.Context, t domain.WebAdminBootstrapToken) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO web_admin_bootstrap_tokens (id, token_hash, owner_actor_id, status, webauthn_session_data, created_at, expires_at, consumed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TokenHash, t.OwnerActorID, string(t.Status), nullableString(t.WebAuthnSessionData),
		formatTime(t.CreatedAt), formatTime(t.ExpiresAt), formatTimePtr(t.ConsumedAt),
	)
	return mapWriteError(err)
}

func (r *webAdminBootstrapTokenRepository) GetByTokenHash(ctx context.Context, tokenHash string) (domain.WebAdminBootstrapToken, error) {
	return scanWebAdminBootstrapToken(r.q.QueryRowContext(ctx,
		`SELECT `+webAdminBootstrapTokenColumns+` FROM web_admin_bootstrap_tokens WHERE token_hash = ?`, tokenHash))
}

func (r *webAdminBootstrapTokenRepository) SetSessionData(ctx context.Context, id, sessionData string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE web_admin_bootstrap_tokens SET webauthn_session_data = ?
		 WHERE id = ? AND status = 'issued' AND expires_at > ?`,
		sessionData, id, formatTime(at),
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *webAdminBootstrapTokenRepository) Consume(ctx context.Context, id string, at time.Time) (domain.WebAdminBootstrapToken, error) {
	row := r.q.QueryRowContext(ctx,
		`UPDATE web_admin_bootstrap_tokens SET status = 'consumed', consumed_at = ?
		 WHERE id = ? AND status = 'issued' AND expires_at > ?
		 RETURNING `+webAdminBootstrapTokenColumns,
		formatTime(at), id, formatTime(at),
	)
	tok, err := scanWebAdminBootstrapToken(row)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.WebAdminBootstrapToken{}, domain.ErrConflict
	}
	return tok, err
}

func scanWebAdminBootstrapToken(row rowScanner) (domain.WebAdminBootstrapToken, error) {
	var t domain.WebAdminBootstrapToken
	var status, createdAt, expiresAt string
	var sessionData, consumedAt sql.NullString
	if err := row.Scan(&t.ID, &t.TokenHash, &t.OwnerActorID, &status, &sessionData, &createdAt, &expiresAt, &consumedAt); err != nil {
		return domain.WebAdminBootstrapToken{}, mapReadError(err)
	}
	t.Status = domain.WebAdminBootstrapStatus(status)
	t.WebAuthnSessionData = stringPtr(sessionData)
	var err error
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.WebAdminBootstrapToken{}, err
	}
	if t.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return domain.WebAdminBootstrapToken{}, err
	}
	if t.ConsumedAt, err = parseTimePtr(consumedAt); err != nil {
		return domain.WebAdminBootstrapToken{}, err
	}
	return t, nil
}

type webAdminCredentialRepository struct{ q querier }

const webAdminCredentialColumns = `id, owner_actor_id, credential_id, credential_json, created_at, last_used_at`

func (r *webAdminCredentialRepository) Create(ctx context.Context, c domain.WebAdminCredential) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO web_admin_credentials (id, owner_actor_id, credential_id, credential_json, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.OwnerActorID, c.CredentialID, c.CredentialJSON, formatTime(c.CreatedAt), formatTimePtr(c.LastUsedAt),
	)
	return mapWriteError(err)
}

func (r *webAdminCredentialRepository) ListByOwner(ctx context.Context, ownerActorID string) ([]domain.WebAdminCredential, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+webAdminCredentialColumns+` FROM web_admin_credentials WHERE owner_actor_id = ? ORDER BY created_at, id`, ownerActorID)
	if err != nil {
		return nil, mapReadError(err)
	}
	defer rows.Close()
	out := make([]domain.WebAdminCredential, 0)
	for rows.Next() {
		c, err := scanWebAdminCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, mapReadError(err)
	}
	return out, nil
}

func scanWebAdminCredential(row rowScanner) (domain.WebAdminCredential, error) {
	var c domain.WebAdminCredential
	var createdAt string
	var lastUsedAt sql.NullString
	if err := row.Scan(&c.ID, &c.OwnerActorID, &c.CredentialID, &c.CredentialJSON, &createdAt, &lastUsedAt); err != nil {
		return domain.WebAdminCredential{}, mapReadError(err)
	}
	var err error
	if c.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.WebAdminCredential{}, err
	}
	if c.LastUsedAt, err = parseTimePtr(lastUsedAt); err != nil {
		return domain.WebAdminCredential{}, err
	}
	return c, nil
}

func (r *webAdminCredentialRepository) UpdateAfterLogin(ctx context.Context, credentialID, credentialJSON string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE web_admin_credentials SET credential_json = ?, last_used_at = ? WHERE credential_id = ?`,
		credentialJSON, formatTime(at), credentialID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *webAdminCredentialRepository) Delete(ctx context.Context, id string) error {
	res, err := r.q.ExecContext(ctx, `DELETE FROM web_admin_credentials WHERE id = ?`, id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *webAdminCredentialRepository) Get(ctx context.Context, id string) (domain.WebAdminCredential, error) {
	return scanWebAdminCredential(r.q.QueryRowContext(ctx, `SELECT `+webAdminCredentialColumns+` FROM web_admin_credentials WHERE id = ?`, id))
}

type webAdminSessionRepository struct{ q querier }

const webAdminSessionColumns = `id, owner_actor_id, credential_id, status, session_token_hash, csrf_token, webauthn_session_data, created_at, expires_at, revoked_at`

func (r *webAdminSessionRepository) Create(ctx context.Context, s domain.WebAdminSession) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO web_admin_sessions (id, owner_actor_id, credential_id, status, session_token_hash, csrf_token, webauthn_session_data, created_at, expires_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OwnerActorID, nullableString(s.CredentialID), string(s.Status),
		nullableString(s.SessionTokenHash), nullableString(s.CSRFToken), nullableString(s.WebAuthnSessionData),
		formatTime(s.CreatedAt), formatTime(s.ExpiresAt), formatTimePtr(s.RevokedAt),
	)
	return mapWriteError(err)
}

func (r *webAdminSessionRepository) Get(ctx context.Context, id string) (domain.WebAdminSession, error) {
	return scanWebAdminSession(r.q.QueryRowContext(ctx, `SELECT `+webAdminSessionColumns+` FROM web_admin_sessions WHERE id = ?`, id))
}

func (r *webAdminSessionRepository) Activate(ctx context.Context, id, credentialID, sessionTokenHash, csrfToken string, expiresAt, at time.Time) (domain.WebAdminSession, error) {
	row := r.q.QueryRowContext(ctx,
		`UPDATE web_admin_sessions
		 SET status = 'active', credential_id = ?, session_token_hash = ?, csrf_token = ?,
		     webauthn_session_data = NULL, expires_at = ?
		 WHERE id = ? AND status = 'pending' AND expires_at > ?
		 RETURNING `+webAdminSessionColumns,
		credentialID, sessionTokenHash, csrfToken, formatTime(expiresAt), id, formatTime(at),
	)
	s, err := scanWebAdminSession(row)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.WebAdminSession{}, domain.ErrConflict
	}
	return s, err
}

func (r *webAdminSessionRepository) GetActiveBySessionTokenHash(ctx context.Context, sessionTokenHash string, at time.Time) (domain.WebAdminSession, error) {
	return scanWebAdminSession(r.q.QueryRowContext(ctx,
		`SELECT `+webAdminSessionColumns+` FROM web_admin_sessions
		 WHERE session_token_hash = ? AND status = 'active' AND revoked_at IS NULL AND expires_at > ?`,
		sessionTokenHash, formatTime(at)))
}

func (r *webAdminSessionRepository) Revoke(ctx context.Context, id string, at time.Time) error {
	_, err := r.q.ExecContext(ctx,
		`UPDATE web_admin_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, formatTime(at), id)
	// No requireRowAffected: revoking an already-revoked or nonexistent
	// session is a success, not an error — see the interface doc comment.
	return mapWriteError(err)
}

func (r *webAdminSessionRepository) RevokeAllByCredential(ctx context.Context, credentialID string, at time.Time) (int, error) {
	res, err := r.q.ExecContext(ctx,
		`UPDATE web_admin_sessions SET revoked_at = ?
		 WHERE credential_id = ? AND status = 'active' AND revoked_at IS NULL`,
		formatTime(at), credentialID,
	)
	if err != nil {
		return 0, mapWriteError(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func scanWebAdminSession(row rowScanner) (domain.WebAdminSession, error) {
	var s domain.WebAdminSession
	var status, createdAt, expiresAt string
	var credentialID, sessionTokenHash, csrfToken, webauthnSessionData, revokedAt sql.NullString
	if err := row.Scan(&s.ID, &s.OwnerActorID, &credentialID, &status, &sessionTokenHash, &csrfToken,
		&webauthnSessionData, &createdAt, &expiresAt, &revokedAt); err != nil {
		return domain.WebAdminSession{}, mapReadError(err)
	}
	s.Status = domain.WebAdminSessionStatus(status)
	s.CredentialID = stringPtr(credentialID)
	s.SessionTokenHash = stringPtr(sessionTokenHash)
	s.CSRFToken = stringPtr(csrfToken)
	s.WebAuthnSessionData = stringPtr(webauthnSessionData)
	var err error
	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.WebAdminSession{}, err
	}
	if s.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return domain.WebAdminSession{}, err
	}
	if s.RevokedAt, err = parseTimePtr(revokedAt); err != nil {
		return domain.WebAdminSession{}, err
	}
	return s, nil
}

type webAdminActionAuditRepository struct{ q querier }

func (r *webAdminActionAuditRepository) Record(ctx context.Context, entry domain.WebAdminActionAuditEntry) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO web_admin_action_audit (id, owner_actor_id, credential_id, action, target, before_value, after_value, changed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.OwnerActorID, nullableString(entry.CredentialID), string(entry.Action), entry.Target,
		nullableString(entry.BeforeValue), nullableString(entry.AfterValue), formatTime(entry.ChangedAt),
	)
	return mapWriteError(err)
}
