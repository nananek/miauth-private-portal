// This file is Issue #74's startup-time tool/web-search configuration
// resolver: it turns OPENWEBUI_TOOL_IDS/OPENWEBUI_WEB_SEARCH_ENABLED
// (Issue #72) plus the target's own model and tool registry into the
// tool_ids/features this deployment's every completions call carries.
//
// It is deliberately not part of Provider (a per-turn contract) and
// depends on nothing from internal/provider/openwebui directly — only
// on the ToolConfigResolver interface below, which
// internal/provider/openwebui.Client satisfies structurally. Importing
// the provider package here would cycle back to it (that package
// already imports this one for Provider), the same reason Provider
// itself is defined here rather than there.
package openwebui

import (
	"context"
	"fmt"
	"log/slog"
)

// ToolIDsNone is OPENWEBUI_TOOL_IDS's dedicated sentinel (Issue #74):
// set alone (never combined with other ids), it explicitly disables the
// model's own default tool_ids rather than falling back to them. This
// is the only way to force zero tools onto a model that has its own
// toolIds configured — an empty/unset OPENWEBUI_TOOL_IDS means "defer to
// the model", not "use none".
const ToolIDsNone = "none"

// ToolConfigResolver is the startup-only, read-only subset of the
// provider adapter ResolveEffectiveToolConfig needs: GetModelTools and
// ListAccessibleTools are both single calls made once at boot, never
// per-turn, which is why they live outside the Provider interface.
type ToolConfigResolver interface {
	// GetModelTools returns modelID's own configured toolIds and
	// defaultFeatureIds (both nil if the model has neither, or is not
	// found).
	GetModelTools(ctx context.Context, modelID string) (toolIDs []string, defaultFeatureIDs []string, err error)
	// ListAccessibleTools returns every tool id this adapter's own
	// account may invoke.
	ListAccessibleTools(ctx context.Context) ([]string, error)
}

// ResolvedToolConfig is what ResolveEffectiveToolConfig decides once at
// startup: the tool_ids/features every completions call this deployment
// makes should carry.
type ResolvedToolConfig struct {
	ToolIDs          []string
	WebSearchEnabled bool
}

// ResolveEffectiveToolConfig applies Issue #74's tool_ids priority rule:
//
//   - configuredToolIDs unset (nil/empty): use the model's own toolIds.
//   - configuredToolIDs a single ToolIDsNone entry: use no tools at all,
//     even if the model has its own toolIds configured.
//   - configuredToolIDs any other non-empty list: use it as-is, ignoring
//     the model's own toolIds.
//
// Either way, the candidate list is then filtered down to ids
// ListAccessibleTools actually reports — fail-closed, never "assume a
// stale or unlisted id would still work" — and every excluded id is
// logged individually as a warning so a stale/inaccessible
// configuration is visible rather than silently downgraded.
//
// WebSearchEnabled is passed through unchanged: OPENWEBUI_WEB_SEARCH_ENABLED
// is a plain, always-explicit opt-in flag (Issue #72), so there is no
// "unset" state to fall back to the model's own defaultFeatureIds from
// — unlike ToolIDs, a bool has no natural way to represent that third
// state without a config-schema change nothing else in this codebase's
// OPENWEBUI_* keys uses. The model's defaultFeatureIds (returned by
// GetModelTools) is deliberately not consulted here for that reason;
// wiring it in would need OPENWEBUI_WEB_SEARCH_ENABLED to become a
// tri-state first.
func ResolveEffectiveToolConfig(
	ctx context.Context,
	resolver ToolConfigResolver,
	modelID string,
	configuredToolIDs []string,
	configuredWebSearchEnabled bool,
	logger *slog.Logger,
) (ResolvedToolConfig, error) {
	if logger == nil {
		logger = slog.Default()
	}

	accessible, err := resolver.ListAccessibleTools(ctx)
	if err != nil {
		return ResolvedToolConfig{}, fmt.Errorf("openwebui: list accessible tools: %w", err)
	}
	accessibleSet := make(map[string]bool, len(accessible))
	for _, id := range accessible {
		accessibleSet[id] = true
	}

	modelToolIDs, _, err := resolver.GetModelTools(ctx, modelID)
	if err != nil {
		return ResolvedToolConfig{}, fmt.Errorf("openwebui: get model tools: %w", err)
	}

	var candidate []string
	switch {
	case len(configuredToolIDs) == 1 && configuredToolIDs[0] == ToolIDsNone:
		candidate = nil
	case len(configuredToolIDs) > 0:
		candidate = configuredToolIDs
	default:
		candidate = modelToolIDs
	}

	resolved := make([]string, 0, len(candidate))
	for _, id := range candidate {
		if accessibleSet[id] {
			resolved = append(resolved, id)
		} else {
			logger.Warn("openwebui: excluding stale or inaccessible tool id from resolved tool_ids",
				"tool_id", id, "model_id", modelID)
		}
	}

	return ResolvedToolConfig{ToolIDs: resolved, WebSearchEnabled: configuredWebSearchEnabled}, nil
}
