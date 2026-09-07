package domain

import (
	"context"
	"time"
)

// AppConfigEntry is one row of the app_config table (migration 0022,
// ADR-0006): a single db-eligible internal/config key's operator-set
// override, layered on top of the file/env/default value config.Load
// already resolves. A key with no row here simply falls back to that
// bootstrap value — see ConfigRepository's own doc comment.
type AppConfigEntry struct {
	Key   string
	Value string
	// Version starts at 1 on a key's first Set and increments by one on
	// every subsequent Set, backing ConfigRepository.Set's
	// compare-and-set write — the same WHERE-clause CAS shape this
	// codebase's other repositories already use (for example
	// OpenWebUIConversationLinkRepository.MarkReady).
	Version   int
	UpdatedAt time.Time
	// UpdatedBy is the owner actor id (ActorOwner) that made this
	// change — the only local actor a CLI operator running as this
	// service's host user is ever attributed to (ADR-0002).
	UpdatedBy string
}

// AppConfigAuditEntry is one row of the app_config_audit table: an
// immutable record of a single Set/Unset, written in the same
// transaction as the app_config write it describes (UnitOfWork.WithinTx)
// so an audited change and its effect can never disagree.
type AppConfigAuditEntry struct {
	ID  string
	Key string
	// OldValue is nil for a key's first-ever Set (no prior row existed)
	// — including the startup auto-seed path's writes, which record the
	// bootstrap value becoming the DB's value for the first time, not a
	// change from some prior operator value.
	OldValue *string
	// NewValue is nil for an Unset: the app_config row is removed and
	// the key's effective value reverts to config.Load's own file/env/
	// default resolution.
	NewValue  *string
	Version   int
	ChangedAt time.Time
	ChangedBy string
}

// ConfigRepository persists ADR-0006's DB configuration overlay: the
// db-eligible internal/config keys (internal/config.IsDBEligibleKey) an
// operator has explicitly set through miauthctl config, layered on top
// of config.Load's own file/env/default resolution. A secret
// (internal/config.IsSecretKey) or bootstrap-only key never has a row
// here at all — ADR-0005 D10 and ADR-0006 both treat "cannot be stored"
// as a property callers rely on, not a check every caller must remember
// to add.
type ConfigRepository interface {
	// Get returns key's current DB override, or ErrNotFound if an
	// operator has never set it (or has Unset it back to the bootstrap
	// value).
	Get(ctx context.Context, key string) (AppConfigEntry, error)
	// List returns every key an operator currently has a DB override
	// for, ordered by key — never in write order, since app_config rows
	// can be written in any order (startup auto-seed, CLI set,
	// rollback).
	List(ctx context.Context) ([]AppConfigEntry, error)
	// Set writes key's DB override, compare-and-set on expectedVersion:
	// 0 requires no row currently exists (a first-time Set or a startup
	// auto-seed write, producing AppConfigEntry.Version 1); a positive
	// expectedVersion requires the row's current version to match
	// exactly. Either mismatch — a row already existing when
	// expectedVersion is 0, or the row having moved to a different
	// version (including having been removed by an intervening Unset)
	// when it is not — returns ErrConflict rather than silently
	// overwriting a change the caller never saw.
	Set(ctx context.Context, key, value string, expectedVersion int, updatedBy string, at time.Time) error
	// Unset deletes key's DB override, reverting its effective value to
	// config.Load's own resolution. ErrNotFound if no row exists.
	Unset(ctx context.Context, key string) error
}

// ConfigAuditRepository persists ConfigRepository's own change history
// (app_config_audit). Every ConfigRepository.Set/Unset call is paired
// with exactly one ConfigAuditRepository.Record call inside the same
// UnitOfWork.WithinTx transaction, so a change and its audit trail can
// never disagree (Issue #76 AC4).
type ConfigAuditRepository interface {
	Record(ctx context.Context, entry AppConfigAuditEntry) error
	// ListByKey returns key's full change history, oldest first — the
	// same ordered-list shape every other List method in this service
	// uses, here by version alone: it is already a strictly increasing
	// per-key sequence, so it needs no secondary tie-break column.
	ListByKey(ctx context.Context, key string) ([]AppConfigAuditEntry, error)
}
