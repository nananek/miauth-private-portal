package sqlite

import (
	"context"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// This file implements Issue #133's api_token_scope_audit table: the
// immutable change log for miauth.Service.ReflectScopes' writes to
// api_tokens.scopes, mirroring config_repository.go's app_config_audit
// (ADR-0006).

type apiTokenScopeAuditRepository struct{ q querier }

const apiTokenScopeAuditSelectColumns = `SELECT id, token_id, old_scopes, new_scopes, changed_at, changed_by FROM api_token_scope_audit`

func (r *apiTokenScopeAuditRepository) Record(ctx context.Context, entry domain.APITokenScopeAuditEntry) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO api_token_scope_audit (id, token_id, old_scopes, new_scopes, changed_at, changed_by) VALUES (?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.TokenID, entry.OldScopes, entry.NewScopes, formatTime(entry.ChangedAt), entry.ChangedBy,
	)
	return mapWriteError(err)
}

// ListByToken orders by (changed_at, id), the same "real time, not a
// recyclable sequence number" ordering configAuditRepository.ListByKey
// uses.
func (r *apiTokenScopeAuditRepository) ListByToken(ctx context.Context, tokenID string) ([]domain.APITokenScopeAuditEntry, error) {
	rows, err := r.q.QueryContext(ctx, apiTokenScopeAuditSelectColumns+` WHERE token_id = ? ORDER BY changed_at, id`, tokenID)
	if err != nil {
		return nil, mapReadError(err)
	}
	defer rows.Close()

	var entries []domain.APITokenScopeAuditEntry
	for rows.Next() {
		e, err := scanAPITokenScopeAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func scanAPITokenScopeAuditEntry(row rowScanner) (domain.APITokenScopeAuditEntry, error) {
	var e domain.APITokenScopeAuditEntry
	var changedAt string
	if err := row.Scan(&e.ID, &e.TokenID, &e.OldScopes, &e.NewScopes, &changedAt, &e.ChangedBy); err != nil {
		return domain.APITokenScopeAuditEntry{}, mapReadError(err)
	}
	var err error
	if e.ChangedAt, err = parseTime(changedAt); err != nil {
		return domain.APITokenScopeAuditEntry{}, err
	}
	return e, nil
}
