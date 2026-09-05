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

const externalSourceSelectColumns = `SELECT id, kind, uri, display_name, cursor, last_fetched_at, last_error,
	consecutive_failures, created_at FROM external_sources`

func (r *externalSourceRepository) Create(ctx context.Context, s domain.ExternalSource) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO external_sources (id, kind, uri, display_name, created_at) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.Kind, s.URI, nullableString(s.DisplayName), formatTime(s.CreatedAt),
	)
	return mapWriteError(err)
}

func (r *externalSourceRepository) Get(ctx context.Context, id string) (domain.ExternalSource, error) {
	row := r.q.QueryRowContext(ctx, externalSourceSelectColumns+` WHERE id = ?`, id)
	return scanExternalSource(row)
}

func (r *externalSourceRepository) List(ctx context.Context, kind string) ([]domain.ExternalSource, error) {
	rows, err := r.q.QueryContext(ctx, externalSourceSelectColumns+` WHERE kind = ? ORDER BY created_at, id`, kind)
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

// EnsureFromConfig is not run inside one transaction across all sources:
// each Create is independent, so a conflict on one entry (already
// seeded in a prior run) never blocks the others from being created. It
// does not touch an existing source's display_name or other fields, by
// design: EnsureFromConfig is a create-if-missing seed, not an upsert.
func (r *externalSourceRepository) EnsureFromConfig(ctx context.Context, sources []domain.ExternalSource) error {
	for _, s := range sources {
		if err := r.Create(ctx, s); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				continue
			}
			return fmt.Errorf("ensure external source from config: %w", err)
		}
	}
	return nil
}

func scanExternalSource(row rowScanner) (domain.ExternalSource, error) {
	var s domain.ExternalSource
	var displayName, cursor, lastFetchedAt, lastError sql.NullString
	var createdAt string
	if err := row.Scan(&s.ID, &s.Kind, &s.URI, &displayName, &cursor, &lastFetchedAt, &lastError,
		&s.ConsecutiveFailures, &createdAt); err != nil {
		return domain.ExternalSource{}, mapReadError(err)
	}
	s.DisplayName = stringPtr(displayName)
	s.Cursor = stringPtr(cursor)
	s.LastError = stringPtr(lastError)

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
