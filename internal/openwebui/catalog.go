// This file is Issue #75 PR1's catalog sync: it turns GET /api/models'
// response into the openwebui_models rows the rest of this service
// already knows how to project as VirtualActors (Registry.Seed minted
// exactly one such row; this reconciles every eligible one the
// configured account can currently see).
//
// It depends only on internal/domain, the same boundary Registry.Seed
// keeps: CatalogProvider is defined here, not imported from
// internal/provider/openwebui, because that package already imports this
// one (the same reason Provider itself lives here rather than there).
package openwebui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// RemoteModel is one GET /api/models entry (docs/compat/openwebui-
// 0.11.3.md), translated to what SyncCatalog needs to reconcile the
// registry and what Issue #75 PR5's per-model tool resolution shares from
// the same response rather than making a second call for it.
type RemoteModel struct {
	// ID is the provider's own opaque model id (a data[].id value) —
	// what this service stores as ExternalModelID. Never empty for an
	// eligible model; eligibleRemoteModels drops blank ids.
	ID string
	// Name is the provider's own display name for the model, used as a
	// newly registered model's initial DisplayName and as
	// GenerateActorSlug's input. An existing model's slug is never
	// recomputed from a later Name change (the roadmap's stable-handle
	// requirement), only its DisplayName is kept in step.
	Name string
	// IsArena reports the built-in "arena-model" entry's own signal
	// (compat: "A built-in arena-model entry is present alongside real
	// models"). eligibleRemoteModels excludes it alongside the literal
	// id, as defense in depth against a future rename of that literal.
	IsArena bool
	// ToolIDs and DefaultFeatureIDs mirror info.meta.toolIds/
	// defaultFeatureIds — nil when the model carries neither, or info
	// itself is null (compat: "the recorded /api/models shape gives
	// [info] as an object or null").
	ToolIDs           []string
	DefaultFeatureIDs []string
}

// CatalogProvider is the one call SyncCatalog needs from a provider
// adapter: list every model the configured account can currently see.
type CatalogProvider interface {
	ListModels(ctx context.Context) ([]RemoteModel, error)
}

// SyncResult summarizes what one SyncCatalog run changed, for a caller
// (Issue #75 PR4's startup bootstrap and periodic job) to log.
type SyncResult struct {
	Created     int
	Reactivated int
	Updated     int
	Deactivated int
}

// eligibleRemoteModels filters models out of catalog sync consideration
// before any registry row is touched: a blank id (never a valid
// ExternalModelID), the built-in arena-model entry, and every occurrence
// of an id after its first (logged as a warning — GET /api/models
// declaring the same id twice is schema drift this adapter has not
// observed, not something to silently pick a winner from).
//
// Everything else is included, deliberately: the hidden/visibility
// fields GET /api/models may carry are docs/compat/openwebui-0.11.3.md's
// own "weakest-evidenced part" of the whole document, so a model this
// filter is unsure about is kept rather than silently dropped from the
// registry.
func eligibleRemoteModels(models []RemoteModel, logger *slog.Logger) []RemoteModel {
	seen := make(map[string]bool, len(models))
	out := make([]RemoteModel, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		if m.ID == "arena-model" || m.IsArena {
			continue
		}
		if seen[m.ID] {
			logger.Warn("openwebui: catalog sync ignoring duplicate model id from GET /api/models", "model_id", m.ID)
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

// SyncCatalog reconciles the enabled workspace's models against provider,
// right now: every eligible model it currently reports is upserted
// (created, minting a new actor and slug, or — for an id already
// registered — has its DisplayName kept in step, never its ActorSlug),
// and every previously active model this round did not see is
// deactivated. A model that disappears and later reappears is
// reactivated rather than re-created, keeping its id, actor, and slug.
//
// provider.ListModels is called before any transaction opens (never hold
// a database transaction across a network call), and an error from it
// returns immediately without touching the registry at all — a provider
// outage or a malformed response must never deactivate every model this
// service knows about. A *successful* empty list is different: it is
// treated as a genuine "the account can currently see nothing", and
// every active model is deactivated accordingly.
func (r *Registry) SyncCatalog(ctx context.Context, provider CatalogProvider, logger *slog.Logger) (SyncResult, error) {
	if !r.cfg.Enabled {
		return SyncResult{}, ErrDisabled
	}
	if logger == nil {
		logger = slog.Default()
	}

	remote, err := provider.ListModels(ctx)
	if err != nil {
		return SyncResult{}, fmt.Errorf("openwebui: list remote models: %w", err)
	}
	eligible := eligibleRemoteModels(remote, logger)

	now := r.clock.Now().UTC()
	var result SyncResult
	err = r.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		workspace, err := repos.OpenWebUIWorkspaces.GetEnabled(ctx)
		if err != nil {
			return fmt.Errorf("get enabled workspace: %w", err)
		}

		existing, err := repos.OpenWebUIModels.ListByWorkspace(ctx, workspace.ID)
		if err != nil {
			return fmt.Errorf("list workspace models: %w", err)
		}
		existingByExternalID := make(map[string]domain.OpenWebUIModel, len(existing))
		takenSlugs := make(map[string]bool, len(existing))
		for _, m := range existing {
			existingByExternalID[m.ExternalModelID] = m
			takenSlugs[strings.ToLower(m.ActorSlug)] = true
		}

		seenExternalIDs := make(map[string]bool, len(eligible))
		for _, rm := range eligible {
			seenExternalIDs[rm.ID] = true

			cur, found := existingByExternalID[rm.ID]
			if !found {
				created, err := r.createSyncedModel(ctx, repos, workspace.ID, rm, takenSlugs, now)
				if err != nil {
					return err
				}
				takenSlugs[strings.ToLower(created.ActorSlug)] = true
				result.Created++
				continue
			}

			cur.DisplayName = rm.Name
			cur.LastSeenAt = &now
			cur.UpdatedAt = now
			if err := repos.OpenWebUIModels.Update(ctx, cur); err != nil {
				return fmt.Errorf("update model %s: %w", cur.ID, err)
			}
			if !cur.Active {
				if err := repos.OpenWebUIModels.SetActive(ctx, cur.ID, true, now); err != nil {
					return fmt.Errorf("reactivate model %s: %w", cur.ID, err)
				}
				result.Reactivated++
			} else {
				result.Updated++
			}
		}

		for _, m := range existing {
			if seenExternalIDs[m.ExternalModelID] || !m.Active {
				continue
			}
			if err := repos.OpenWebUIModels.SetActive(ctx, m.ID, false, now); err != nil {
				return fmt.Errorf("deactivate model %s: %w", m.ID, err)
			}
			result.Deactivated++
		}

		if r.cfg.DefaultModelID != "" && !seenExternalIDs[r.cfg.DefaultModelID] {
			logger.Warn("openwebui: configured default model was not reported by this catalog sync round",
				"default_model_id", r.cfg.DefaultModelID)
		}
		return nil
	})
	if err != nil {
		return SyncResult{}, err
	}
	return result, nil
}

// createSyncedModel registers a model SyncCatalog has not seen before:
// mints its VirtualActor row, generates its slug (never recomputed
// again — see GenerateActorSlug), and inserts the model row active.
func (r *Registry) createSyncedModel(
	ctx context.Context, repos domain.Repos, workspaceID string, rm RemoteModel, takenSlugs map[string]bool, now time.Time,
) (domain.OpenWebUIModel, error) {
	actor := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: now}
	if err := repos.Actors.Create(ctx, actor); err != nil {
		return domain.OpenWebUIModel{}, fmt.Errorf("create model actor: %w", err)
	}

	slug := GenerateActorSlug(rm.ID, rm.Name, r.reservedActorSlug(takenSlugs))
	created := domain.OpenWebUIModel{
		ID:              domain.NewID(),
		WorkspaceID:     workspaceID,
		ExternalModelID: rm.ID,
		DisplayName:     rm.Name,
		ActorSlug:       slug,
		ActorID:         actor.ID,
		Active:          true,
		LastSeenAt:      &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := repos.OpenWebUIModels.Create(ctx, created); err != nil {
		return domain.OpenWebUIModel{}, fmt.Errorf("create model: %w", err)
	}
	return created, nil
}

// reservedActorSlug builds GenerateActorSlug's reserved callback: a
// candidate is reserved if some other model in this workspace already
// has it (taken, checked case-insensitively even though every generated
// candidate is already lowercase — the set may also contain manually
// assigned slugs from before Issue #75), if it collides with the owner's
// own username, or if it is one of the fixed "assistant"/"system"
// presentation names — the same three-way check
// OPENWEBUI_MODEL_SLUG's own validation used to enforce before this
// issue made slugs a generated value.
func (r *Registry) reservedActorSlug(taken map[string]bool) func(candidate string) bool {
	return func(candidate string) bool {
		if taken[strings.ToLower(candidate)] {
			return true
		}
		if r.cfg.OwnerUsername != "" && strings.EqualFold(candidate, r.cfg.OwnerUsername) {
			return true
		}
		return candidate == "assistant" || candidate == "system"
	}
}
