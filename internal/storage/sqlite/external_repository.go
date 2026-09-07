package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type externalSourceRepository struct{ q querier }

const externalSourceSelectColumns = `SELECT id, kind, uri, display_name, actor_id, username, host, cursor, last_fetched_at, last_error,
	consecutive_failures, active, created_at FROM external_sources`

// Create relies on the active column's own DEFAULT 1 (migration 0023):
// a newly created source is always active, so callers (Create's own,
// and ReconcileFromConfig's create-if-missing path below) never need to
// pass it explicitly.
func (r *externalSourceRepository) Create(ctx context.Context, s domain.ExternalSource) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO external_sources (id, kind, uri, display_name, actor_id, username, host, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Kind, s.URI, nullableString(s.DisplayName), nullableString(s.ActorID), nullableString(s.Username), nullableString(s.Host),
		formatTime(s.CreatedAt),
	)
	return mapWriteError(err)
}

func (r *externalSourceRepository) Get(ctx context.Context, id string) (domain.ExternalSource, error) {
	row := r.q.QueryRowContext(ctx, externalSourceSelectColumns+` WHERE id = ?`, id)
	return scanExternalSource(row)
}

func (r *externalSourceRepository) GetByURI(ctx context.Context, kind, uri string) (domain.ExternalSource, error) {
	row := r.q.QueryRowContext(ctx, externalSourceSelectColumns+` WHERE kind = ? AND uri = ?`, kind, uri)
	return scanExternalSource(row)
}

func (r *externalSourceRepository) GetByActorID(ctx context.Context, actorID string) (domain.ExternalSource, error) {
	row := r.q.QueryRowContext(ctx, externalSourceSelectColumns+` WHERE actor_id = ?`, actorID)
	return scanExternalSource(row)
}

func (r *externalSourceRepository) List(ctx context.Context, kind string) ([]domain.ExternalSource, error) {
	rows, err := r.q.QueryContext(ctx, externalSourceSelectColumns+` WHERE kind = ? AND active = 1 ORDER BY created_at, id`, kind)
	if err != nil {
		return nil, fmt.Errorf("list external sources: %w", err)
	}
	defer rows.Close()

	var sources []domain.ExternalSource
	for rows.Next() {
		s, err := scanExternalSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, s)
	}
	return sources, rows.Err()
}

// RecordFetchSuccess uses COALESCE so a nil cursor (an unmodified-since-
// last-fetch outcome, which has no new ETag/Last-Modified to persist)
// leaves the previously stored cursor untouched instead of clearing it.
func (r *externalSourceRepository) RecordFetchSuccess(ctx context.Context, id string, cursor *string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE external_sources SET cursor = COALESCE(?, cursor), last_fetched_at = ?, last_error = NULL,
			consecutive_failures = 0 WHERE id = ?`,
		nullableString(cursor), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *externalSourceRepository) RecordFetchFailure(ctx context.Context, id string, errMsg string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE external_sources SET last_fetched_at = ?, last_error = ?, consecutive_failures = consecutive_failures + 1
			WHERE id = ?`,
		formatTime(at), errMsg, id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

// ReconcileFromConfig is not run inside one transaction across every
// row: each Create/setActive is independent, so a conflict or failure
// on one URI never blocks the others from being reconciled. It never
// touches an existing source's display_name, cursor, last_fetched_at,
// last_error, or consecutive_failures — the only column this ever
// writes on an existing row is active.
func (r *externalSourceRepository) ReconcileFromConfig(ctx context.Context, kind string, uris []string, at time.Time) error {
	rows, err := r.q.QueryContext(ctx, `SELECT id, uri, active FROM external_sources WHERE kind = ?`, kind)
	if err != nil {
		return fmt.Errorf("reconcile external sources: list existing: %w", err)
	}
	type existingSource struct {
		id     string
		active bool
	}
	existing := make(map[string]existingSource)
	for rows.Next() {
		var id, uri string
		var activeInt int
		if scanErr := rows.Scan(&id, &uri, &activeInt); scanErr != nil {
			rows.Close()
			return fmt.Errorf("reconcile external sources: scan existing: %w", scanErr)
		}
		existing[uri] = existingSource{id: id, active: activeInt != 0}
	}
	closeErr := rows.Err()
	rows.Close()
	if closeErr != nil {
		return fmt.Errorf("reconcile external sources: list existing: %w", closeErr)
	}

	desired := make(map[string]bool, len(uris))
	for _, uri := range uris {
		desired[uri] = true
		cur, ok := existing[uri]
		if !ok {
			if err := r.Create(ctx, domain.ExternalSource{ID: domain.NewID(), Kind: kind, URI: uri, CreatedAt: at}); err != nil {
				if errors.Is(err, domain.ErrConflict) {
					// Created concurrently since the SELECT above (e.g.
					// another reconcile round racing this one); it is
					// active by default (the column's own DEFAULT 1),
					// nothing more to do for this URI.
					continue
				}
				return fmt.Errorf("reconcile external sources: create %s: %w", uri, err)
			}
			continue
		}
		if !cur.active {
			if err := r.setActive(ctx, cur.id, true); err != nil {
				return fmt.Errorf("reconcile external sources: reactivate %s: %w", uri, err)
			}
		}
	}

	for uri, cur := range existing {
		if desired[uri] || !cur.active {
			continue
		}
		if err := r.setActive(ctx, cur.id, false); err != nil {
			return fmt.Errorf("reconcile external sources: deactivate %s: %w", uri, err)
		}
	}
	return nil
}

func (r *externalSourceRepository) setActive(ctx context.Context, id string, active bool) error {
	res, err := r.q.ExecContext(ctx, `UPDATE external_sources SET active = ? WHERE id = ?`, boolToInt(active), id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanExternalSource(row rowScanner) (domain.ExternalSource, error) {
	var s domain.ExternalSource
	var displayName, actorID, username, host, cursor, lastFetchedAt, lastError sql.NullString
	var active int
	var createdAt string
	if err := row.Scan(&s.ID, &s.Kind, &s.URI, &displayName, &actorID, &username, &host, &cursor, &lastFetchedAt, &lastError,
		&s.ConsecutiveFailures, &active, &createdAt); err != nil {
		return domain.ExternalSource{}, mapReadError(err)
	}
	s.DisplayName = stringPtr(displayName)
	s.ActorID = stringPtr(actorID)
	s.Username = stringPtr(username)
	s.Host = stringPtr(host)
	s.Cursor = stringPtr(cursor)
	s.LastError = stringPtr(lastError)
	s.Active = active != 0

	var err error
	if s.LastFetchedAt, err = parseTimePtr(lastFetchedAt); err != nil {
		return domain.ExternalSource{}, err
	}
	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.ExternalSource{}, err
	}
	return s, nil
}

type externalItemRepository struct{ q querier }

const externalItemSelectColumns = `SELECT id, source_id, external_id, provenance_url, published_at,
	fetched_at, dedupe_key, entry_id, created_at FROM external_items`

func (r *externalItemRepository) Create(ctx context.Context, i domain.ExternalItem) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO external_items (id, source_id, external_id, provenance_url, published_at, fetched_at,
			dedupe_key, entry_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.SourceID, i.ExternalID, nullableString(i.ProvenanceURL), formatTimePtr(i.PublishedAt),
		formatTime(i.FetchedAt), i.DedupeKey, nullableString(i.EntryID), formatTime(i.CreatedAt),
	)
	return mapWriteError(err)
}

func (r *externalItemRepository) GetByDedupeKey(ctx context.Context, dedupeKey string) (domain.ExternalItem, error) {
	row := r.q.QueryRowContext(ctx, externalItemSelectColumns+` WHERE dedupe_key = ?`, dedupeKey)
	return scanExternalItem(row)
}

// Promote sets id's entry_id, but only if it is not already promoted: the
// WHERE clause's entry_id IS NULL check makes a second Promote of the
// same item report ErrConflict instead of silently overwriting the
// existing entry_id, symmetric with Create's dedupe-key protection.
func (r *externalItemRepository) Promote(ctx context.Context, id, entryID string) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE external_items SET entry_id = ? WHERE id = ? AND entry_id IS NULL`, entryID, id)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func scanExternalItem(row rowScanner) (domain.ExternalItem, error) {
	var i domain.ExternalItem
	var provenanceURL, entryID sql.NullString
	var publishedAt sql.NullString
	var fetchedAt, createdAt string

	if err := row.Scan(&i.ID, &i.SourceID, &i.ExternalID, &provenanceURL, &publishedAt, &fetchedAt,
		&i.DedupeKey, &entryID, &createdAt); err != nil {
		return domain.ExternalItem{}, mapReadError(err)
	}

	i.ProvenanceURL = stringPtr(provenanceURL)
	i.EntryID = stringPtr(entryID)

	var err error
	if i.PublishedAt, err = parseTimePtr(publishedAt); err != nil {
		return domain.ExternalItem{}, err
	}
	if i.FetchedAt, err = parseTime(fetchedAt); err != nil {
		return domain.ExternalItem{}, err
	}
	if i.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.ExternalItem{}, err
	}
	return i, nil
}
