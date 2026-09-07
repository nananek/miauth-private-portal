package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// This file implements the two Open WebUI registry repositories
// (migration 0017). Nothing here validates a base URL, presentation
// host, or secret ref: those are configuration-layer rules, checked
// before a row is ever built (Issue #52 PR3). What this layer owns is
// the persistence contract — which columns are immutable, which writes
// are compare-and-set, and which constraint turns each mistake into a
// domain error.

type openWebUIWorkspaceRepository struct{ q querier }

const openWebUIWorkspaceSelectColumns = `SELECT id, name, base_url, secret_ref, presentation_host,
	default_model_id, enabled, generation_enabled, chat_create_status, chat_continue_status,
	created_at, updated_at
	FROM openwebui_workspaces`

// Create inserts a workspace. DefaultModelID is normally nil here: a
// workspace and its first model are created in one transaction, and the
// default is pointed at the model afterwards with SetDefaultModel. The
// composite foreign key backing that column is deferred, so either order
// works inside a transaction, but the nil-then-point order is the one
// that also works statement by statement.
func (r *openWebUIWorkspaceRepository) Create(ctx context.Context, w domain.OpenWebUIWorkspace) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO openwebui_workspaces (id, name, base_url, secret_ref, presentation_host,
			default_model_id, enabled, generation_enabled, chat_create_status, chat_continue_status,
			created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.BaseURL, w.SecretRef, w.PresentationHost,
		nullableString(w.DefaultModelID), boolToInt(w.Enabled), boolToInt(w.GenerationEnabled),
		string(w.ChatCreateStatus), string(w.ChatContinueStatus),
		formatTime(w.CreatedAt), formatTime(w.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *openWebUIWorkspaceRepository) Get(ctx context.Context, id string) (domain.OpenWebUIWorkspace, error) {
	return scanOpenWebUIWorkspace(r.q.QueryRowContext(ctx, openWebUIWorkspaceSelectColumns+` WHERE id = ?`, id))
}

func (r *openWebUIWorkspaceRepository) GetByBaseURL(ctx context.Context, baseURL string) (domain.OpenWebUIWorkspace, error) {
	return scanOpenWebUIWorkspace(r.q.QueryRowContext(ctx, openWebUIWorkspaceSelectColumns+` WHERE base_url = ?`, baseURL))
}

// GetEnabled returns the single enabled workspace. A second enabled row
// is an error rather than a silently picked winner: this deployment has
// one workspace by design, and choosing between two would make which
// instance a message went to depend on row order.
//
// LIMIT 2 is deliberate — enough to notice a second row, not enough to
// scan the table to prove there is only one.
func (r *openWebUIWorkspaceRepository) GetEnabled(ctx context.Context) (domain.OpenWebUIWorkspace, error) {
	rows, err := r.q.QueryContext(ctx, openWebUIWorkspaceSelectColumns+` WHERE enabled = 1 ORDER BY created_at, id LIMIT 2`)
	if err != nil {
		return domain.OpenWebUIWorkspace{}, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return domain.OpenWebUIWorkspace{}, err
		}
		return domain.OpenWebUIWorkspace{}, domain.ErrNotFound
	}
	w, err := scanOpenWebUIWorkspace(rows)
	if err != nil {
		return domain.OpenWebUIWorkspace{}, err
	}
	if rows.Next() {
		return domain.OpenWebUIWorkspace{}, fmt.Errorf("more than one enabled Open WebUI workspace; exactly one is supported")
	}
	return w, rows.Err()
}

// List returns every workspace, ordered by (created_at, id) like every
// other list method in this package.
func (r *openWebUIWorkspaceRepository) List(ctx context.Context) ([]domain.OpenWebUIWorkspace, error) {
	rows, err := r.q.QueryContext(ctx, openWebUIWorkspaceSelectColumns+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []domain.OpenWebUIWorkspace
	for rows.Next() {
		w, err := scanOpenWebUIWorkspace(rows)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

// Update writes the mutable registry fields only. ID, created_at, both
// enable flags, and default_model_id are deliberately absent: those have
// their own methods, so a caller that read a row, changed a name, and
// wrote it back cannot also silently re-enable a workspace an operator
// had turned off.
func (r *openWebUIWorkspaceRepository) Update(ctx context.Context, w domain.OpenWebUIWorkspace) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_workspaces
		 SET name = ?, base_url = ?, secret_ref = ?, presentation_host = ?,
			chat_create_status = ?, chat_continue_status = ?, updated_at = ?
		 WHERE id = ?`,
		w.Name, w.BaseURL, w.SecretRef, w.PresentationHost,
		string(w.ChatCreateStatus), string(w.ChatContinueStatus), formatTime(w.UpdatedAt), w.ID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

// SetDefaultModel points a workspace at one of its own models. A model
// belonging to another workspace fails the composite foreign key rather
// than being checked here, so the rule holds against a concurrent writer
// too.
func (r *openWebUIWorkspaceRepository) SetDefaultModel(ctx context.Context, workspaceID, modelID string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_workspaces SET default_model_id = ?, updated_at = ? WHERE id = ?`,
		modelID, formatTime(at), workspaceID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *openWebUIWorkspaceRepository) SetEnabled(ctx context.Context, workspaceID string, enabled bool, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_workspaces SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), formatTime(at), workspaceID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *openWebUIWorkspaceRepository) SetGenerationEnabled(ctx context.Context, workspaceID string, enabled bool, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_workspaces SET generation_enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), formatTime(at), workspaceID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *openWebUIWorkspaceRepository) SetCapabilityStatus(
	ctx context.Context, workspaceID string, chatCreate, chatContinue domain.CapabilityStatus, at time.Time,
) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_workspaces SET chat_create_status = ?, chat_continue_status = ?, updated_at = ? WHERE id = ?`,
		string(chatCreate), string(chatContinue), formatTime(at), workspaceID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanOpenWebUIWorkspace(row rowScanner) (domain.OpenWebUIWorkspace, error) {
	var w domain.OpenWebUIWorkspace
	var defaultModelID sql.NullString
	var enabled, generationEnabled int
	var chatCreateStatus, chatContinueStatus, createdAt, updatedAt string
	if err := row.Scan(&w.ID, &w.Name, &w.BaseURL, &w.SecretRef, &w.PresentationHost,
		&defaultModelID, &enabled, &generationEnabled, &chatCreateStatus, &chatContinueStatus,
		&createdAt, &updatedAt,
	); err != nil {
		return domain.OpenWebUIWorkspace{}, mapReadError(err)
	}
	w.DefaultModelID = stringPtr(defaultModelID)
	w.Enabled = enabled != 0
	w.GenerationEnabled = generationEnabled != 0
	w.ChatCreateStatus = domain.CapabilityStatus(chatCreateStatus)
	w.ChatContinueStatus = domain.CapabilityStatus(chatContinueStatus)

	var err error
	if w.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.OpenWebUIWorkspace{}, err
	}
	if w.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.OpenWebUIWorkspace{}, err
	}
	return w, nil
}

type openWebUIModelRepository struct{ q querier }

const openWebUIModelSelectColumns = `SELECT id, workspace_id, external_model_id, display_name, actor_slug,
	actor_id, active, capabilities, external_updated_at, last_seen_at, created_at, updated_at
	FROM openwebui_models`

func (r *openWebUIModelRepository) Create(ctx context.Context, m domain.OpenWebUIModel) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO openwebui_models (id, workspace_id, external_model_id, display_name, actor_slug,
			actor_id, active, capabilities, external_updated_at, last_seen_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.WorkspaceID, m.ExternalModelID, m.DisplayName, m.ActorSlug,
		m.ActorID, boolToInt(m.Active), m.Capabilities.Encode(), formatTimePtr(m.ExternalUpdatedAt),
		formatTimePtr(m.LastSeenAt), formatTime(m.CreatedAt), formatTime(m.UpdatedAt),
	)
	return mapWriteError(err)
}

func (r *openWebUIModelRepository) Get(ctx context.Context, id string) (domain.OpenWebUIModel, error) {
	return scanOpenWebUIModel(r.q.QueryRowContext(ctx, openWebUIModelSelectColumns+` WHERE id = ?`, id))
}

func (r *openWebUIModelRepository) GetByActor(ctx context.Context, actorID string) (domain.OpenWebUIModel, error) {
	return scanOpenWebUIModel(r.q.QueryRowContext(ctx, openWebUIModelSelectColumns+` WHERE actor_id = ?`, actorID))
}

func (r *openWebUIModelRepository) GetByExternalID(ctx context.Context, workspaceID, externalModelID string) (domain.OpenWebUIModel, error) {
	return scanOpenWebUIModel(r.q.QueryRowContext(ctx,
		openWebUIModelSelectColumns+` WHERE workspace_id = ? AND external_model_id = ?`, workspaceID, externalModelID))
}

func (r *openWebUIModelRepository) GetByActorSlug(ctx context.Context, workspaceID, slug string) (domain.OpenWebUIModel, error) {
	return scanOpenWebUIModel(r.q.QueryRowContext(ctx,
		openWebUIModelSelectColumns+` WHERE workspace_id = ? AND actor_slug = ?`, workspaceID, slug))
}

// ListByWorkspace orders by (created_at, id): the same stable local
// ordering everything else in this service uses, and never anything
// derived from a provider id.
func (r *openWebUIModelRepository) ListByWorkspace(ctx context.Context, workspaceID string) ([]domain.OpenWebUIModel, error) {
	rows, err := r.q.QueryContext(ctx,
		openWebUIModelSelectColumns+` WHERE workspace_id = ? ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []domain.OpenWebUIModel
	for rows.Next() {
		m, err := scanOpenWebUIModel(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

// Update writes only display name, actor slug, capabilities, the
// provider's own timestamp, and last_seen_at. workspace_id,
// external_model_id and actor_id are not in the statement at all: the
// roadmap requires a model actor's local ID to survive a display-name or
// handle change, and the surest way to guarantee that is for the rename
// path to have no way to write those columns.
func (r *openWebUIModelRepository) Update(ctx context.Context, m domain.OpenWebUIModel) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_models
		 SET display_name = ?, actor_slug = ?, capabilities = ?, external_updated_at = ?, last_seen_at = ?, updated_at = ?
		 WHERE id = ?`,
		m.DisplayName, m.ActorSlug, m.Capabilities.Encode(), formatTimePtr(m.ExternalUpdatedAt),
		formatTimePtr(m.LastSeenAt), formatTime(m.UpdatedAt), m.ID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *openWebUIModelRepository) SetActive(ctx context.Context, modelID string, active bool, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE openwebui_models SET active = ?, updated_at = ? WHERE id = ?`,
		boolToInt(active), formatTime(at), modelID,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func scanOpenWebUIModel(row rowScanner) (domain.OpenWebUIModel, error) {
	var m domain.OpenWebUIModel
	var active int
	var capabilities, createdAt, updatedAt string
	var externalUpdatedAt, lastSeenAt sql.NullString
	if err := row.Scan(&m.ID, &m.WorkspaceID, &m.ExternalModelID, &m.DisplayName, &m.ActorSlug,
		&m.ActorID, &active, &capabilities, &externalUpdatedAt, &lastSeenAt, &createdAt, &updatedAt,
	); err != nil {
		return domain.OpenWebUIModel{}, mapReadError(err)
	}
	m.Active = active != 0

	caps, err := domain.ParseOpenWebUIModelCapabilities(capabilities)
	if err != nil {
		return domain.OpenWebUIModel{}, fmt.Errorf("decode model capabilities: %w", err)
	}
	m.Capabilities = caps

	if m.ExternalUpdatedAt, err = parseTimePtr(externalUpdatedAt); err != nil {
		return domain.OpenWebUIModel{}, err
	}
	if m.LastSeenAt, err = parseTimePtr(lastSeenAt); err != nil {
		return domain.OpenWebUIModel{}, err
	}
	if m.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.OpenWebUIModel{}, err
	}
	if m.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.OpenWebUIModel{}, err
	}
	return m, nil
}
