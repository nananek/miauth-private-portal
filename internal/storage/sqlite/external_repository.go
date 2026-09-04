package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type externalSourceRepository struct{ q querier }

const externalSourceSelectColumns = `SELECT id, kind, uri, display_name, created_at FROM external_sources`

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

func (r *externalSourceRepository) List(ctx context.Context) ([]domain.ExternalSource, error) {
	rows, err := r.q.QueryContext(ctx, externalSourceSelectColumns+` ORDER BY created_at, id`)
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

func scanExternalSource(row rowScanner) (domain.ExternalSource, error) {
	var s domain.ExternalSource
	var displayName sql.NullString
	var createdAt string
	if err := row.Scan(&s.ID, &s.Kind, &s.URI, &displayName, &createdAt); err != nil {
		return domain.ExternalSource{}, mapReadError(err)
	}
	s.DisplayName = stringPtr(displayName)
	var err error
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
