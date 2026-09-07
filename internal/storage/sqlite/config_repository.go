package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// This file implements ADR-0006's two-table runtime configuration
// overlay (migration 0022): app_config, the current DB override per
// db-eligible internal/config key, and app_config_audit, its immutable
// change log. Nothing here validates a key or its value — that is
// internal/config.IsDBEligibleKey / internal/config.ValidateKeyValue's
// job, checked before a write ever reaches this layer, the same split
// openwebui_registry_repository.go's own leading comment describes for
// that table.

type configRepository struct{ q querier }

const appConfigSelectColumns = `SELECT key, value, version, updated_at, updated_by FROM app_config`

func (r *configRepository) Get(ctx context.Context, key string) (domain.AppConfigEntry, error) {
	return scanAppConfigEntry(r.q.QueryRowContext(ctx, appConfigSelectColumns+` WHERE key = ?`, key))
}

// List orders by key, not by any write-order column: app_config rows
// can be written in any order (startup auto-seed, CLI set, rollback), so
// key is the only stable, deterministic order available.
func (r *configRepository) List(ctx context.Context) ([]domain.AppConfigEntry, error) {
	rows, err := r.q.QueryContext(ctx, appConfigSelectColumns+` ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []domain.AppConfigEntry
	for rows.Next() {
		e, err := scanAppConfigEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Set implements domain.ConfigRepository.Set's compare-and-set write.
// expectedVersion 0 requires no row to currently exist: the INSERT's own
// primary key turns a concurrent first-time Set into domain.ErrConflict
// via mapWriteError, the same translation every other repository's
// uniqueness conflict already goes through. A positive expectedVersion
// requires the row's current version to match exactly; the UPDATE's
// WHERE clause encodes that expectation, so zero rows affected — the row
// having moved to a different version, or having been removed entirely
// by an intervening Unset — becomes domain.ErrConflict via
// requireRowAffectedConflict, mirroring this package's other CAS writes.
func (r *configRepository) Set(ctx context.Context, key, value string, expectedVersion int, updatedBy string, at time.Time) error {
	if expectedVersion == 0 {
		_, err := r.q.ExecContext(ctx,
			`INSERT INTO app_config (key, value, version, updated_at, updated_by) VALUES (?, ?, 1, ?, ?)`,
			key, value, formatTime(at), updatedBy,
		)
		return mapWriteError(err)
	}
	res, err := r.q.ExecContext(ctx,
		`UPDATE app_config SET value = ?, version = ?, updated_at = ?, updated_by = ? WHERE key = ? AND version = ?`,
		value, expectedVersion+1, formatTime(at), updatedBy, key, expectedVersion,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *configRepository) Unset(ctx context.Context, key string) error {
	res, err := r.q.ExecContext(ctx, `DELETE FROM app_config WHERE key = ?`, key)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanAppConfigEntry(row rowScanner) (domain.AppConfigEntry, error) {
	var e domain.AppConfigEntry
	var updatedAt string
	if err := row.Scan(&e.Key, &e.Value, &e.Version, &updatedAt, &e.UpdatedBy); err != nil {
		return domain.AppConfigEntry{}, mapReadError(err)
	}
	var err error
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.AppConfigEntry{}, err
	}
	return e, nil
}

type configAuditRepository struct{ q querier }

const appConfigAuditSelectColumns = `SELECT id, key, old_value, new_value, version, changed_at, changed_by FROM app_config_audit`

func (r *configAuditRepository) Record(ctx context.Context, entry domain.AppConfigAuditEntry) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO app_config_audit (id, key, old_value, new_value, version, changed_at, changed_by) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.Key, nullableString(entry.OldValue), nullableString(entry.NewValue), entry.Version,
		formatTime(entry.ChangedAt), entry.ChangedBy,
	)
	return mapWriteError(err)
}

// ListByKey orders by version alone: it is already a strictly increasing
// per-key sequence, so it needs no secondary timestamp tie-break.
func (r *configAuditRepository) ListByKey(ctx context.Context, key string) ([]domain.AppConfigAuditEntry, error) {
	rows, err := r.q.QueryContext(ctx, appConfigAuditSelectColumns+` WHERE key = ? ORDER BY version`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []domain.AppConfigAuditEntry
	for rows.Next() {
		e, err := scanAppConfigAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func scanAppConfigAuditEntry(row rowScanner) (domain.AppConfigAuditEntry, error) {
	var e domain.AppConfigAuditEntry
	var oldValue, newValue sql.NullString
	var changedAt string
	if err := row.Scan(&e.ID, &e.Key, &oldValue, &newValue, &e.Version, &changedAt, &e.ChangedBy); err != nil {
		return domain.AppConfigAuditEntry{}, mapReadError(err)
	}
	e.OldValue = stringPtr(oldValue)
	e.NewValue = stringPtr(newValue)
	var err error
	if e.ChangedAt, err = parseTime(changedAt); err != nil {
		return domain.AppConfigAuditEntry{}, err
	}
	return e, nil
}
