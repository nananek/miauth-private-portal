package openwebui

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sort"
	"testing"
)

// fakeToolConfigResolver is a test double for ToolConfigResolver: no
// HTTP, just canned return values, mirroring the fakes
// internal/openwebui's other _test.go files use for Provider.
type fakeToolConfigResolver struct {
	modelToolIDs           []string
	modelDefaultFeatureIDs []string
	accessibleTools        []string
	getModelToolsErr       error
	listAccessibleErr      error
}

func (f *fakeToolConfigResolver) GetModelTools(ctx context.Context, modelID string) ([]string, []string, error) {
	if f.getModelToolsErr != nil {
		return nil, nil, f.getModelToolsErr
	}
	return f.modelToolIDs, f.modelDefaultFeatureIDs, nil
}

func (f *fakeToolConfigResolver) ListAccessibleTools(ctx context.Context) ([]string, error) {
	if f.listAccessibleErr != nil {
		return nil, f.listAccessibleErr
	}
	return f.accessibleTools, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// TestResolveEffectiveToolConfig_UnsetFallsBackToModelDefaults backs the
// 3-state priority rule's first state: OPENWEBUI_TOOL_IDS unset (nil)
// defers to the model's own configured toolIds.
func TestResolveEffectiveToolConfig_UnsetFallsBackToModelDefaults(t *testing.T) {
	resolver := &fakeToolConfigResolver{
		modelToolIDs:    []string{"web_search", "calculator"},
		accessibleTools: []string{"web_search", "calculator", "other_tool"},
	}
	got, err := ResolveEffectiveToolConfig(t.Context(), resolver, "model-1", nil, false, discardLogger())
	if err != nil {
		t.Fatalf("ResolveEffectiveToolConfig: %v", err)
	}
	if !reflect.DeepEqual(sorted(got.ToolIDs), sorted([]string{"web_search", "calculator"})) {
		t.Errorf("ToolIDs = %v, want the model's own toolIds", got.ToolIDs)
	}
}

// TestResolveEffectiveToolConfig_ExplicitListOverridesModelDefaults backs
// the second state: a non-empty OPENWEBUI_TOOL_IDS list wins over
// whatever the model itself has configured.
func TestResolveEffectiveToolConfig_ExplicitListOverridesModelDefaults(t *testing.T) {
	resolver := &fakeToolConfigResolver{
		modelToolIDs:    []string{"web_search"},
		accessibleTools: []string{"web_search", "calculator"},
	}
	got, err := ResolveEffectiveToolConfig(t.Context(), resolver, "model-1", []string{"calculator"}, false, discardLogger())
	if err != nil {
		t.Fatalf("ResolveEffectiveToolConfig: %v", err)
	}
	if !reflect.DeepEqual(got.ToolIDs, []string{"calculator"}) {
		t.Errorf("ToolIDs = %v, want the configured list only", got.ToolIDs)
	}
}

// TestResolveEffectiveToolConfig_NoneSentinelDisablesModelDefaults backs
// the third state: OPENWEBUI_TOOL_IDS="none" explicitly disables tools
// even though the model has its own toolIds configured.
func TestResolveEffectiveToolConfig_NoneSentinelDisablesModelDefaults(t *testing.T) {
	resolver := &fakeToolConfigResolver{
		modelToolIDs:    []string{"web_search", "calculator"},
		accessibleTools: []string{"web_search", "calculator"},
	}
	got, err := ResolveEffectiveToolConfig(t.Context(), resolver, "model-1", []string{ToolIDsNone}, false, discardLogger())
	if err != nil {
		t.Fatalf("ResolveEffectiveToolConfig: %v", err)
	}
	if len(got.ToolIDs) != 0 {
		t.Errorf("ToolIDs = %v, want none (the sentinel disables the model's own defaults)", got.ToolIDs)
	}
}

// TestResolveEffectiveToolConfig_ExcludesInaccessibleIDs backs the
// fail-closed exclusion rule: a configured or model-default id that
// ListAccessibleTools does not report is dropped, never sent as-is.
func TestResolveEffectiveToolConfig_ExcludesInaccessibleIDs(t *testing.T) {
	resolver := &fakeToolConfigResolver{
		accessibleTools: []string{"calculator"},
	}
	got, err := ResolveEffectiveToolConfig(t.Context(), resolver, "model-1",
		[]string{"calculator", "stale_tool"}, false, discardLogger())
	if err != nil {
		t.Fatalf("ResolveEffectiveToolConfig: %v", err)
	}
	if !reflect.DeepEqual(got.ToolIDs, []string{"calculator"}) {
		t.Errorf("ToolIDs = %v, want only the accessible id (stale_tool excluded)", got.ToolIDs)
	}
}

// TestResolveEffectiveToolConfig_WebSearchPassesThroughUnchanged backs
// this resolver's documented simplification: WebSearchEnabled is always
// the configured value verbatim, never inferred from the model's own
// defaultFeatureIds (OPENWEBUI_WEB_SEARCH_ENABLED has no "unset" state
// to fall back from).
func TestResolveEffectiveToolConfig_WebSearchPassesThroughUnchanged(t *testing.T) {
	resolver := &fakeToolConfigResolver{
		modelDefaultFeatureIDs: []string{"web_search"},
	}
	got, err := ResolveEffectiveToolConfig(t.Context(), resolver, "model-1", nil, false, discardLogger())
	if err != nil {
		t.Fatalf("ResolveEffectiveToolConfig: %v", err)
	}
	if got.WebSearchEnabled {
		t.Error("WebSearchEnabled = true, want false (configured value, not the model's defaultFeatureIds)")
	}

	got, err = ResolveEffectiveToolConfig(t.Context(), resolver, "model-1", nil, true, discardLogger())
	if err != nil {
		t.Fatalf("ResolveEffectiveToolConfig: %v", err)
	}
	if !got.WebSearchEnabled {
		t.Error("WebSearchEnabled = false, want true (configured value)")
	}
}

// TestResolveEffectiveToolConfig_ListAccessibleErrorPropagates and
// TestResolveEffectiveToolConfig_GetModelToolsErrorPropagates back the
// fail-closed contract: this function reports resolution failure as an
// error rather than guessing, so the caller (cmd/server/main.go) can
// decide to disable tool_ids/web_search for the run instead of treating
// a startup-time provider outage as fatal.
func TestResolveEffectiveToolConfig_ListAccessibleErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	resolver := &fakeToolConfigResolver{listAccessibleErr: wantErr}
	_, err := ResolveEffectiveToolConfig(t.Context(), resolver, "model-1", nil, false, discardLogger())
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapping %v", err, wantErr)
	}
}

func TestResolveEffectiveToolConfig_GetModelToolsErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	resolver := &fakeToolConfigResolver{getModelToolsErr: wantErr}
	_, err := ResolveEffectiveToolConfig(t.Context(), resolver, "model-1", nil, false, discardLogger())
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapping %v", err, wantErr)
	}
}
