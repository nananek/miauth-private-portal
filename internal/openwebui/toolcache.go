// This file is Issue #75 PR5's per-model tool resolution: what Issue
// #74 originally resolved once, at boot, for the single configured
// default model (OPENWEBUI_TOOL_IDS's three-state priority against that
// one model's own info.meta.toolIds) is now resolved for every active
// model, every catalog sync round (Registry.SyncCatalog, catalog.go),
// because a deployment can have more than one model now. Per owner
// decision, there is no deployment-wide OPENWEBUI_TOOL_IDS override left
// to prioritize against: every model's tool_ids come from its own
// info.meta.toolIds, filtered fail-closed against what this credential
// may actually invoke, exactly as Issue #74 already did for the model's
// *own* defaults when no override was configured.
package openwebui

import (
	"log/slog"
	"sync"
)

// ToolConfigCache holds the tool_ids each active model resolved to as of
// the most recent successful catalog sync round, keyed by
// ExternalModelID. It is intentionally not persisted: the resolved list
// is a live reflection of the provider's own state (its model config and
// this credential's own tool grants), so a value here that outlived a
// restart could be stale in a way nothing would ever refresh.
//
// TurnJob reads it per turn (a cache miss — an unsynced or newly
// discovered model — resolves to no tools, the safe default); Registry.
// SyncCatalog is its only writer.
type ToolConfigCache struct {
	mu      sync.RWMutex
	byModel map[string][]string
}

// NewToolConfigCache builds an empty ToolConfigCache. cmd/server builds
// exactly one and shares it between the catalog-sync producer
// (Registry.SyncCatalog, via CatalogSyncJob and the startup sync call)
// and TurnJob, the consumer.
func NewToolConfigCache() *ToolConfigCache {
	return &ToolConfigCache{byModel: make(map[string][]string)}
}

// Get returns externalModelID's resolved tool_ids, or nil if no
// successful sync round has ever resolved anything for it (a brand new
// model discovered by a StartChat before the next sync, say) — nil
// means "send no tool_ids key at all", the same safe default an
// inaccessible or unconfigured id already falls back to.
func (c *ToolConfigCache) Get(externalModelID string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byModel[externalModelID]
}

// Replace atomically swaps the entire cache contents for byModel. It is
// a full replace, never a merge: a model no longer in the round that
// just resolved (deactivated, or simply absent from this response) must
// not keep answering Get with a stale list from an earlier round, and a
// concurrent reader must never observe a mix of two different rounds'
// values.
func (c *ToolConfigCache) Replace(byModel map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byModel = byModel
}

// resolveToolConfig computes eligible's per-model tool_ids for one sync
// round: each model's own RemoteModel.ToolIDs (already captured by the
// same GET /api/models call Registry.SyncCatalog used to reconcile the
// registry), filtered fail-closed against accessibleTools — an id this
// credential cannot actually invoke is dropped and logged individually,
// never sent as-is, mirroring Issue #74's original exclusion rule.
func resolveToolConfig(eligible []RemoteModel, accessibleTools []string, logger *slog.Logger) map[string][]string {
	accessibleSet := make(map[string]bool, len(accessibleTools))
	for _, id := range accessibleTools {
		accessibleSet[id] = true
	}

	resolved := make(map[string][]string, len(eligible))
	for _, rm := range eligible {
		var ids []string
		for _, id := range rm.ToolIDs {
			if accessibleSet[id] {
				ids = append(ids, id)
			} else {
				logger.Warn("openwebui: catalog sync excluding inaccessible tool id from resolved tool_ids",
					"tool_id", id, "model_id", rm.ID)
			}
		}
		resolved[rm.ID] = ids
	}
	return resolved
}
