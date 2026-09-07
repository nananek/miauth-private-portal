// Package configstore is Issue #76's DB configuration overlay's runtime
// read path (ADR-0006): Store resolves a db-eligible internal/config
// key's current effective value, reading through
// domain.ConfigRepository first and falling back to the bootstrap
// (file/env/default) value a caller already has otherwise — the
// "DB (when a row exists) > env > file > default" precedence ADR-0006
// establishes.
//
// A continuously-running or continuously-polled component
// (internal/jobs, internal/ingest, internal/llmreply,
// internal/llmclassify, internal/openwebui) is meant to read through a
// Store at the point it consumes a tunable value, instead of capturing
// *config.Config once at construction, so a DB override actually
// reaches it without a restart. Wiring an actual call site over to read
// through Store is each component's own later PR (Issue #76 PR4a/4b/4c);
// this package only provides the read primitive.
package configstore

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// Store reads domain.ConfigRepository, falling back to a caller-supplied
// bootstrap value whenever no DB override exists or the stored value
// fails to parse as the requested type. It never returns an error: a
// config read is never allowed to fail or block the caller's own real
// work, so any problem here (no row, a transient read error, a value
// that no longer parses after a since-changed internal/config bound)
// degrades to "use the bootstrap value," logged at warn level so it is
// still visible operationally.
type Store struct {
	repo   domain.ConfigRepository
	logger *slog.Logger
}

// New builds a Store. logger defaults to slog.Default() when nil.
func New(repo domain.ConfigRepository, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{repo: repo, logger: logger}
}

// raw returns key's current DB override value and true, or ("", false)
// if no row exists. A read error other than domain.ErrNotFound is
// logged and also treated as "no override" — see Store's own doc
// comment on why a config read never fails the caller.
func (s *Store) raw(ctx context.Context, key string) (string, bool) {
	entry, err := s.repo.Get(ctx, key)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			s.logger.Warn("configstore: read failed, using bootstrap value", "key", key, "error", err)
		}
		return "", false
	}
	return entry.Value, true
}

// String returns key's DB override, or fallback if unset.
func (s *Store) String(ctx context.Context, key, fallback string) string {
	if v, ok := s.raw(ctx, key); ok {
		return v
	}
	return fallback
}

// StringList returns key's DB override, split the same comma-separated
// way internal/config.KeyRSSFeedURLs itself is split, or fallback if
// unset. An empty override value yields an empty (non-nil) list, the
// explicit "no entries" state — a caller wanting "unchanged from
// bootstrap" instead should not have written an empty override in the
// first place (ValidateKeyValue rejects an empty Set value for exactly
// this reason: use Unset instead).
func (s *Store) StringList(ctx context.Context, key string, fallback []string) []string {
	v, ok := s.raw(ctx, key)
	if !ok {
		return fallback
	}
	if v == "" {
		return []string{}
	}
	parts := strings.Split(v, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// BoolPtr returns key's DB override parsed as a boolean, or fallback if
// unset or unparseable. Unlike Bool, fallback (and the return value) is
// a *bool: it exists for a tri-state key like
// OPENWEBUI_WEB_SEARCH_ENABLED (ADR-0005 D21), where "no DB row and no
// bootstrap value either" (nil) is a third state distinct from both
// true and false, not just Bool's ordinary default. A DB override, once
// set, is always a concrete true or false — ValidateKeyValue rejects an
// empty Set value — so only the no-override case can ever produce nil,
// and only when fallback itself is nil.
func (s *Store) BoolPtr(ctx context.Context, key string, fallback *bool) *bool {
	v, ok := s.raw(ctx, key)
	if !ok {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		s.logger.Warn("configstore: stored value no longer parses as a boolean, using bootstrap value", "key", key)
		return fallback
	}
	return &b
}

// Bool returns key's DB override parsed as a boolean, or fallback if
// unset or unparseable.
func (s *Store) Bool(ctx context.Context, key string, fallback bool) bool {
	v, ok := s.raw(ctx, key)
	if !ok {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		s.logger.Warn("configstore: stored value no longer parses as a boolean, using bootstrap value", "key", key)
		return fallback
	}
	return b
}

// Int returns key's DB override parsed as an integer, or fallback if
// unset or unparseable.
func (s *Store) Int(ctx context.Context, key string, fallback int) int {
	v, ok := s.raw(ctx, key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		s.logger.Warn("configstore: stored value no longer parses as an integer, using bootstrap value", "key", key)
		return fallback
	}
	return n
}

// Int64 returns key's DB override parsed as a 64-bit integer, or
// fallback if unset or unparseable.
func (s *Store) Int64(ctx context.Context, key string, fallback int64) int64 {
	v, ok := s.raw(ctx, key)
	if !ok {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		s.logger.Warn("configstore: stored value no longer parses as an integer, using bootstrap value", "key", key)
		return fallback
	}
	return n
}

// Duration returns key's DB override parsed as a time.Duration, or
// fallback if unset or unparseable.
func (s *Store) Duration(ctx context.Context, key string, fallback time.Duration) time.Duration {
	v, ok := s.raw(ctx, key)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		s.logger.Warn("configstore: stored value no longer parses as a duration, using bootstrap value", "key", key)
		return fallback
	}
	return d
}
