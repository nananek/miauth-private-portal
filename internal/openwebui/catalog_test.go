package openwebui

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// fakeCatalogProvider is a test-controlled CatalogProvider: either a
// fixed model list or a fixed error, never both meaningfully at once
// (SyncCatalog only ever consults Err when it is non-nil). accessibleTools/
// accessibleErr back its ListAccessibleTools half independently, since a
// test exercising the model registry side has no reason to also fix a
// tool list.
type fakeCatalogProvider struct {
	models        []RemoteModel
	err           error
	accessibleIDs []string
	accessibleErr error
}

func (f *fakeCatalogProvider) ListModels(ctx context.Context) ([]RemoteModel, error) {
	return f.models, f.err
}

func (f *fakeCatalogProvider) ListAccessibleTools(ctx context.Context) ([]string, error) {
	return f.accessibleIDs, f.accessibleErr
}

func TestEligibleRemoteModels_FiltersBlankArenaAndDuplicateIDs(t *testing.T) {
	models := []RemoteModel{
		{ID: "gpt-oss:20b", Name: "GPT OSS 20B"},
		{ID: ""},
		{ID: "arena-model", Name: "Arena Model"},
		{ID: "flagged-arena", Name: "Flagged", IsArena: true},
		{ID: "gpt-oss:20b", Name: "duplicate of the first"},
		{ID: "gpt-oss:120b", Name: "GPT OSS 120B"},
	}
	got := eligibleRemoteModels(models, discardLogger())
	if len(got) != 2 {
		t.Fatalf("eligibleRemoteModels returned %d models, want 2: %+v", len(got), got)
	}
	if got[0].ID != "gpt-oss:20b" || got[1].ID != "gpt-oss:120b" {
		t.Errorf("eligibleRemoteModels = %+v, want gpt-oss:20b then gpt-oss:120b", got)
	}
}

func TestSyncCatalog_DisabledReturnsErrDisabled(t *testing.T) {
	cfg := validRegistryConfig()
	cfg.Enabled = false
	tr := newTestRegistry(t, cfg)
	if _, err := tr.SyncCatalog(t.Context(), &fakeCatalogProvider{}, nil, nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("SyncCatalog() error = %v, want ErrDisabled", err)
	}
}

// TestSyncCatalog_ProviderErrorLeavesRegistryUntouched backs the outage
// rule: ListModels failing (timeout, 5xx, malformed body) must not
// deactivate, create, or otherwise change a single row.
func TestSyncCatalog_ProviderErrorLeavesRegistryUntouched(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := tr.db.OpenWebUIModels.ListByWorkspace(t.Context(), mustEnabledWorkspaceID(t, tr))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tr.SyncCatalog(t.Context(), &fakeCatalogProvider{err: errors.New("boom")}, nil, discardLogger()); err == nil {
		t.Fatal("SyncCatalog with a failing provider should return an error")
	}

	after, err := tr.db.OpenWebUIModels.ListByWorkspace(t.Context(), mustEnabledWorkspaceID(t, tr))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || after[0] != before[0] {
		t.Errorf("registry changed after a failed sync: before %+v, after %+v", before, after)
	}
}

// TestSyncCatalog_CreatesNewModelsWithGeneratedSlugs is the core of the
// catalog: an unseen id in the response mints a new actor and model row,
// with DisplayName and ActorSlug both derived from the remote model's own
// name, and an already-registered model's DisplayName is kept in step
// while its ActorSlug is left exactly as first assigned.
func TestSyncCatalog_CreatesNewModelsWithGeneratedSlugs(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seededDefault, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	tr.clock.Advance(time.Hour)
	provider := &fakeCatalogProvider{models: []RemoteModel{
		{ID: "gpt-oss:20b", Name: "GPT OSS 20B"},
		{ID: "gpt-oss:120b", Name: "GPT OSS 120B"},
	}}
	result, err := tr.SyncCatalog(t.Context(), provider, nil, discardLogger())
	if err != nil {
		t.Fatalf("SyncCatalog: %v", err)
	}
	if result.Created != 1 || result.Updated != 1 || result.Reactivated != 0 || result.Deactivated != 0 {
		t.Errorf("SyncCatalog result = %+v, want {Created:1 Updated:1}", result)
	}

	updatedDefault, err := tr.db.OpenWebUIModels.Get(t.Context(), seededDefault.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedDefault.DisplayName != "GPT OSS 20B" {
		t.Errorf("existing model DisplayName = %q, want the synced name", updatedDefault.DisplayName)
	}
	if updatedDefault.ActorSlug != seededDefault.ActorSlug {
		t.Errorf("existing model ActorSlug changed from %q to %q; a sync must never recompute it", seededDefault.ActorSlug, updatedDefault.ActorSlug)
	}
	if updatedDefault.LastSeenAt == nil {
		t.Error("existing model LastSeenAt is nil after appearing in a sync round")
	}

	created, err := tr.db.OpenWebUIModels.GetByExternalID(t.Context(), workspace.ID, "gpt-oss:120b")
	if err != nil {
		t.Fatalf("GetByExternalID(new model): %v", err)
	}
	if created.DisplayName != "GPT OSS 120B" {
		t.Errorf("new model DisplayName = %q, want GPT OSS 120B", created.DisplayName)
	}
	if created.ActorSlug != "gpt_oss_120b" {
		t.Errorf("new model ActorSlug = %q, want gpt_oss_120b", created.ActorSlug)
	}
	if created.ActorSlug == updatedDefault.ActorSlug {
		t.Error("the two models were assigned the same slug")
	}
	if !created.Active {
		t.Error("a newly synced model should be active")
	}
	if created.LastSeenAt == nil {
		t.Error("a newly synced model should have LastSeenAt set")
	}

	actor, err := tr.db.Actors.Get(t.Context(), created.ActorID)
	if err != nil {
		t.Fatalf("Get new model actor: %v", err)
	}
	if actor.Type != domain.ActorOpenWebUIModel {
		t.Errorf("new model actor.Type = %q, want %q", actor.Type, domain.ActorOpenWebUIModel)
	}
}

// TestSyncCatalog_DeactivatesModelsNotInThisRoundAndReactivatesOnReturn
// covers both directions: a model this sync round did not see is
// deactivated (and stops resolving), and one that reappears in a later
// round is reactivated under its original id, actor, and slug rather
// than being re-created.
func TestSyncCatalog_DeactivatesModelsNotInThisRoundAndReactivatesOnReturn(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seededDefault, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	tr.clock.Advance(time.Hour)
	firstRound := &fakeCatalogProvider{models: []RemoteModel{
		{ID: "gpt-oss:20b", Name: "GPT OSS 20B"},
		{ID: "gpt-oss:120b", Name: "GPT OSS 120B"},
	}}
	if _, err := tr.SyncCatalog(t.Context(), firstRound, nil, discardLogger()); err != nil {
		t.Fatalf("first SyncCatalog: %v", err)
	}
	secondModel, err := tr.db.OpenWebUIModels.GetByExternalID(t.Context(), workspace.ID, "gpt-oss:120b")
	if err != nil {
		t.Fatal(err)
	}

	// Second round: gpt-oss:120b disappears from the response.
	tr.clock.Advance(time.Hour)
	secondRound := &fakeCatalogProvider{models: []RemoteModel{
		{ID: "gpt-oss:20b", Name: "GPT OSS 20B"},
	}}
	result, err := tr.SyncCatalog(t.Context(), secondRound, nil, discardLogger())
	if err != nil {
		t.Fatalf("second SyncCatalog: %v", err)
	}
	if result.Deactivated != 1 {
		t.Errorf("second round Deactivated = %d, want 1", result.Deactivated)
	}
	afterDisappearing, err := tr.db.OpenWebUIModels.Get(t.Context(), secondModel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterDisappearing.Active {
		t.Error("a model absent from a successful sync round should be deactivated")
	}
	if _, err := tr.ResolveVirtualActor(t.Context(), secondModel.ActorID); !errors.Is(err, ErrNotVirtualActor) {
		t.Errorf("ResolveVirtualActor(deactivated model) error = %v, want ErrNotVirtualActor", err)
	}

	// Third round: it reappears.
	tr.clock.Advance(time.Hour)
	thirdRound := &fakeCatalogProvider{models: []RemoteModel{
		{ID: "gpt-oss:20b", Name: "GPT OSS 20B"},
		{ID: "gpt-oss:120b", Name: "GPT OSS 120B (renamed)"},
	}}
	result, err = tr.SyncCatalog(t.Context(), thirdRound, nil, discardLogger())
	if err != nil {
		t.Fatalf("third SyncCatalog: %v", err)
	}
	if result.Reactivated != 1 {
		t.Errorf("third round Reactivated = %d, want 1", result.Reactivated)
	}
	reactivated, err := tr.db.OpenWebUIModels.Get(t.Context(), secondModel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reactivated.Active {
		t.Error("the reappearing model should be active again")
	}
	if reactivated.ID != secondModel.ID || reactivated.ActorID != secondModel.ActorID || reactivated.ActorSlug != secondModel.ActorSlug {
		t.Errorf("reappearing model got a new identity: %+v, want the original %+v", reactivated, secondModel)
	}
	if reactivated.DisplayName != "GPT OSS 120B (renamed)" {
		t.Errorf("reactivated model DisplayName = %q, want the latest synced name", reactivated.DisplayName)
	}

	// The whole time, the seeded default (present in every round) stayed
	// untouched and active.
	stillDefault, err := tr.db.OpenWebUIModels.Get(t.Context(), seededDefault.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stillDefault.Active {
		t.Error("a model present in every round should never be deactivated")
	}
}

// TestSyncCatalog_EmptySuccessfulListDeactivatesEverything backs the
// distinction the roadmap draws between a provider outage (returns
// unchanged, see above) and a *successful* response reporting zero
// models: the latter is a genuine snapshot, and every previously active
// model is deactivated accordingly.
func TestSyncCatalog_EmptySuccessfulListDeactivatesEverything(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seededDefault, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	tr.clock.Advance(time.Hour)
	result, err := tr.SyncCatalog(t.Context(), &fakeCatalogProvider{models: nil}, nil, discardLogger())
	if err != nil {
		t.Fatalf("SyncCatalog: %v", err)
	}
	if result.Deactivated != 1 {
		t.Errorf("Deactivated = %d, want 1", result.Deactivated)
	}
	got, err := tr.db.OpenWebUIModels.Get(t.Context(), seededDefault.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Active {
		t.Error("a successful empty catalog response should deactivate every model")
	}
}

// TestResolveToolConfig_FiltersInaccessibleIDs backs Issue #75 PR5's
// fail-closed rule: a model's own ToolIDs are kept only when
// accessibleTools also reports them, mirroring Issue #74's original
// exclusion behavior — now applied per model rather than to one
// resolved-at-boot default.
func TestResolveToolConfig_FiltersInaccessibleIDs(t *testing.T) {
	eligible := []RemoteModel{
		{ID: "model-a", ToolIDs: []string{"web_search", "stale_tool"}},
		{ID: "model-b", ToolIDs: []string{"calculator"}},
		{ID: "model-c"}, // no toolIds at all
	}
	got := resolveToolConfig(eligible, []string{"web_search", "calculator"}, discardLogger())

	if !reflect.DeepEqual(got["model-a"], []string{"web_search"}) {
		t.Errorf("model-a = %v, want [web_search] (stale_tool excluded)", got["model-a"])
	}
	if !reflect.DeepEqual(got["model-b"], []string{"calculator"}) {
		t.Errorf("model-b = %v, want [calculator]", got["model-b"])
	}
	if len(got["model-c"]) != 0 {
		t.Errorf("model-c = %v, want none", got["model-c"])
	}
}

// TestSyncCatalog_PopulatesToolCachePerModel backs the integration this
// package's whole point is: a sync round resolves each eligible model's
// own tool_ids into the shared ToolConfigCache, filtered against
// ListAccessibleTools, keyed by ExternalModelID.
func TestSyncCatalog_PopulatesToolCachePerModel(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}

	provider := &fakeCatalogProvider{
		models: []RemoteModel{
			{ID: "gpt-oss:20b", Name: "GPT OSS 20B", ToolIDs: []string{"web_search", "stale_tool"}},
			{ID: "gpt-oss:120b", Name: "GPT OSS 120B", ToolIDs: []string{"calculator"}},
		},
		accessibleIDs: []string{"web_search", "calculator"},
	}
	toolCache := NewToolConfigCache()
	if _, err := tr.SyncCatalog(t.Context(), provider, toolCache, discardLogger()); err != nil {
		t.Fatalf("SyncCatalog: %v", err)
	}

	if got := toolCache.Get("gpt-oss:20b"); !reflect.DeepEqual(got, []string{"web_search"}) {
		t.Errorf("toolCache.Get(gpt-oss:20b) = %v, want [web_search]", got)
	}
	if got := toolCache.Get("gpt-oss:120b"); !reflect.DeepEqual(got, []string{"calculator"}) {
		t.Errorf("toolCache.Get(gpt-oss:120b) = %v, want [calculator]", got)
	}
}

// TestSyncCatalog_ListAccessibleToolsErrorKeepsPreviousToolCache backs
// the fail-open rule: a transient failure resolving the accessible-tools
// list must not blow away a previous round's good cache, and must not
// fail the whole sync (the registry write itself already succeeded).
func TestSyncCatalog_ListAccessibleToolsErrorKeepsPreviousToolCache(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}

	toolCache := NewToolConfigCache()
	toolCache.Replace(map[string][]string{"gpt-oss:20b": {"web_search"}})

	tr.clock.Advance(time.Hour)
	provider := &fakeCatalogProvider{
		models:        []RemoteModel{{ID: "gpt-oss:20b", Name: "GPT OSS 20B", ToolIDs: []string{"calculator"}}},
		accessibleErr: errors.New("tools endpoint unreachable"),
	}
	result, err := tr.SyncCatalog(t.Context(), provider, toolCache, discardLogger())
	if err != nil {
		t.Fatalf("SyncCatalog should still succeed on the registry side: %v", err)
	}
	if result.Updated != 1 {
		t.Errorf("Updated = %d, want 1 (the registry write is independent of tool resolution)", result.Updated)
	}
	if got := toolCache.Get("gpt-oss:20b"); !reflect.DeepEqual(got, []string{"web_search"}) {
		t.Errorf("toolCache.Get(gpt-oss:20b) = %v, want the previous round's value kept", got)
	}
}

// mustEnabledWorkspaceID is a small convenience for tests that only need
// the enabled workspace's id to list its models.
func mustEnabledWorkspaceID(t *testing.T, tr *testRegistry) string {
	t.Helper()
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return workspace.ID
}
