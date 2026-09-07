// This file is Issue #75 AC#11's per-model web_search default
// resolution (ADR-0005 D21): what D17/D20 explicitly left a plain,
// always-explicit, deployment-wide OPENWEBUI_WEB_SEARCH_ENABLED bool is
// now resolved per model too, when that config key is left unset — the
// same "read it from the same GET /api/models response catalog sync
// already fetches, once per round" pattern toolcache.go established for
// tool_ids.
package openwebui

import "sync"

// modelDefaultsWebSearch is the one defaultFeatureIds member this
// adapter resolves (docs/compat/openwebui-0.11.3.md). Issue #72/#75's
// wire support for `features` is web_search only (featuresBody,
// internal/provider/openwebui/client.go) — resolving any other
// defaultFeatureIds entry here would have no corresponding request field
// to expand it into.
const modelDefaultsWebSearch = "web_search"

// FeatureDefaultCache holds, per active model, whether that model's most
// recently synced GET /api/models info.meta.defaultFeatureIds included
// "web_search". It backs the "unset" branch of ADR-0005 D21's priority
// rule: an explicit OPENWEBUI_WEB_SEARCH_ENABLED always overrides every
// model uniformly and never consults this cache at all; only a nil
// (unset) config value falls back to it.
//
// Like ToolConfigCache, it is intentionally not persisted (a live
// reflection of the provider's own state, refreshed every sync round)
// and is written only by Registry.SyncCatalog and read only by TurnJob.
type FeatureDefaultCache struct {
	mu      sync.RWMutex
	byModel map[string]bool
}

// NewFeatureDefaultCache builds an empty FeatureDefaultCache. cmd/server
// builds exactly one and shares it between the catalog-sync producer and
// TurnJob, the consumer — the same lifecycle ToolConfigCache already has.
func NewFeatureDefaultCache() *FeatureDefaultCache {
	return &FeatureDefaultCache{byModel: make(map[string]bool)}
}

// Get reports whether externalModelID's most recently synced
// defaultFeatureIds included "web_search". A cache miss (never synced,
// or synced but the model does not carry the id) resolves to false —
// never turning a feature on this adapter cannot confirm the model
// itself defaults to.
func (c *FeatureDefaultCache) Get(externalModelID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byModel[externalModelID]
}

// Replace atomically swaps the entire cache contents for byModel — a
// full replace, never a merge, for the same reason ToolConfigCache.Replace
// is: a model no longer in the round that just resolved must not keep
// answering Get with a stale value from an earlier round.
func (c *FeatureDefaultCache) Replace(byModel map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byModel = byModel
}

// resolveFeatureDefaults computes eligible's per-model "defaults to
// web_search" flags for one sync round, straight from each RemoteModel's
// own DefaultFeatureIDs (already captured by the same GET /api/models
// call Registry.SyncCatalog used to reconcile the registry — no second
// provider call). Unlike resolveToolConfig, this needs no fail-closed
// filtering against a separate accessible-something list: a model's own
// advertised default feature is trusted the same way its DisplayName
// already is, not treated as a capability claim to verify (ADR-0005 D21).
func resolveFeatureDefaults(eligible []RemoteModel) map[string]bool {
	resolved := make(map[string]bool, len(eligible))
	for _, rm := range eligible {
		for _, id := range rm.DefaultFeatureIDs {
			if id == modelDefaultsWebSearch {
				resolved[rm.ID] = true
				break
			}
		}
	}
	return resolved
}
