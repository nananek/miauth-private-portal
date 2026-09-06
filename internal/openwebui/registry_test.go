package openwebui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestSeed_CreatesWorkspaceModelAndVirtualActor(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())

	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatalf("GetEnabled: %v", err)
	}
	if workspace.BaseURL != "https://openwebui.example.net" || workspace.Name != "Open WebUI" {
		t.Errorf("workspace = %+v, want the seeded values", workspace)
	}
	if workspace.PresentationHost != "openwebui.example.net" || workspace.SecretRef != SecretRefAPIKey {
		t.Errorf("workspace = %+v, want presentation host/secret_ref from config", workspace)
	}
	if workspace.DefaultModelID == nil {
		t.Fatal("workspace has no default model after Seed")
	}

	model, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatalf("Get default model: %v", err)
	}
	if model.ExternalModelID != "gpt-oss:20b" || model.ActorSlug != "model" || model.DisplayName != "GPT-OSS 20B" {
		t.Errorf("model = %+v, want the seeded values", model)
	}
	if !model.Active {
		t.Error("the default model should be active")
	}

	actor, err := tr.db.Actors.Get(t.Context(), model.ActorID)
	if err != nil {
		t.Fatalf("Get model actor: %v", err)
	}
	if actor.Type != domain.ActorOpenWebUIModel {
		t.Errorf("actor.Type = %q, want %q", actor.Type, domain.ActorOpenWebUIModel)
	}
	// The registry never writes actors.display_name: that column is the
	// owner's own self-edited field, and a model's presentation name
	// lives on the model row instead.
	if actor.DisplayName != nil {
		t.Errorf("model actor DisplayName = %v, want nil", actor.DisplayName)
	}

	virtual, err := tr.ResolveVirtualActor(t.Context(), model.ActorID)
	if err != nil {
		t.Fatalf("ResolveVirtualActor: %v", err)
	}
	if virtual.Handle() != "@model@openwebui.example.net" {
		t.Errorf("Handle() = %q, want @model@openwebui.example.net", virtual.Handle())
	}
	if virtual.DisplayName != "GPT-OSS 20B" {
		t.Errorf("DisplayName = %q, want GPT-OSS 20B", virtual.DisplayName)
	}
}

// TestSeed_IsIdempotentWithStableIdentity is the roadmap's "a model
// actor's stable local ID must survive display-name or handle changes":
// re-running Seed with different presentation values must not disturb
// the workspace id, the model id, or the actor id, even though the
// presentation fields themselves change.
// TestSeed_ReconcilesGenerationEnabledFromConfig backs §4's "Registry.Seed
// writes generation_enabled per config on every run": Issue #52 creates
// the column and the write but has no bridge to gate yet, so this is the
// only way the flag can currently reach the database — and it must track
// config both ways (a later false must clear an earlier true), not just
// turn it on once and leave it.
func TestSeed_ReconcilesGenerationEnabledFromConfig(t *testing.T) {
	cfg := validRegistryConfig()
	cfg.GenerationEnabled = true
	tr := newTestRegistry(t, cfg)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !workspace.GenerationEnabled {
		t.Error("GenerationEnabled = false after Seed with GenerationEnabled: true, want true")
	}

	tr.clock.Advance(time.Hour)
	cfg.GenerationEnabled = false
	tr.Registry = NewRegistry(tr.db, tr.db.Repos, cfg, tr.clock, nil, nil)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("second Seed: %v", err)
	}
	workspace, err = tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if workspace.GenerationEnabled {
		t.Error("GenerationEnabled = true after re-Seed with GenerationEnabled: false, want false")
	}
}

func TestSeed_IsIdempotentWithStableIdentity(t *testing.T) {
	cfg := validRegistryConfig()
	tr := newTestRegistry(t, cfg)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	firstWorkspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	firstModel, err := tr.db.OpenWebUIModels.Get(t.Context(), *firstWorkspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	tr.clock.Advance(time.Hour)
	changed := cfg
	changed.WorkspaceName = "Renamed Instance"
	changed.ModelDisplayName = "Renamed Model"
	changed.ModelSlug = "renamed"
	changed.PresentationHost = "renamed.example.net"
	tr.Registry = NewRegistry(tr.db, tr.db.Repos, changed, tr.clock, nil, nil)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("second Seed: %v", err)
	}

	secondWorkspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secondModel, err := tr.db.OpenWebUIModels.Get(t.Context(), *secondWorkspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	if secondWorkspace.ID != firstWorkspace.ID {
		t.Errorf("workspace ID changed: %q -> %q", firstWorkspace.ID, secondWorkspace.ID)
	}
	if secondModel.ID != firstModel.ID {
		t.Errorf("model ID changed: %q -> %q", firstModel.ID, secondModel.ID)
	}
	if secondModel.ActorID != firstModel.ActorID {
		t.Errorf("model actor ID changed: %q -> %q", firstModel.ActorID, secondModel.ActorID)
	}
	if secondModel.ExternalModelID != firstModel.ExternalModelID {
		t.Errorf("ExternalModelID changed: %q -> %q", firstModel.ExternalModelID, secondModel.ExternalModelID)
	}

	// The presentation fields did change.
	if secondWorkspace.Name != "Renamed Instance" || secondWorkspace.PresentationHost != "renamed.example.net" {
		t.Errorf("workspace presentation fields did not update: %+v", secondWorkspace)
	}
	if secondModel.DisplayName != "Renamed Model" || secondModel.ActorSlug != "renamed" {
		t.Errorf("model presentation fields did not update: %+v", secondModel)
	}

	// The old handle no longer resolves; the actor now projects the new one.
	virtual, err := tr.ResolveVirtualActor(t.Context(), secondModel.ActorID)
	if err != nil {
		t.Fatalf("ResolveVirtualActor after rename: %v", err)
	}
	if virtual.Handle() != "@renamed@renamed.example.net" {
		t.Errorf("Handle() = %q, want @renamed@renamed.example.net", virtual.Handle())
	}
}

// TestSeed_ChangingBaseURLDoesNotDeriveOrChangePresentationHost pins
// that presentation_host is never inferred from base_url: seeding a
// workspace, then re-seeding at a different base URL (a new workspace
// row, since GetByBaseURL will not find the old one) still uses exactly
// the configured presentation host, never one derived from either URL.
func TestSeed_DifferentBaseURLCreatesADistinctWorkspace(t *testing.T) {
	cfg := validRegistryConfig()
	tr := newTestRegistry(t, cfg)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	first, err := tr.db.OpenWebUIWorkspaces.GetByBaseURL(t.Context(), cfg.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	firstModel, err := tr.db.OpenWebUIModels.Get(t.Context(), *first.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	tr.clock.Advance(time.Hour)
	changed := cfg
	changed.BaseURL = "https://internal-instance.example.net"
	tr.Registry = NewRegistry(tr.db, tr.db.Repos, changed, tr.clock, nil, nil)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("second Seed: %v", err)
	}
	second, err := tr.db.OpenWebUIWorkspaces.GetByBaseURL(t.Context(), changed.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Error("a different base URL should not reuse the first workspace's row")
	}
	// The presentation host is unaffected by the base URL naming a
	// different host: it is a configured value, not derived.
	if second.PresentationHost != cfg.PresentationHost {
		t.Errorf("PresentationHost = %q, want the configured %q regardless of base_url", second.PresentationHost, cfg.PresentationHost)
	}

	// The bug this test now also guards: re-seeding at a changed base
	// URL must disable the workspace it left behind (and its model),
	// not just create a new enabled one — GetEnabled's
	// single-enabled-workspace invariant must survive an instance
	// switch, and the old model must stop being a resolvable
	// VirtualActor rather than staying projected under its old
	// presentation host forever.
	firstAfter, err := tr.db.OpenWebUIWorkspaces.Get(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstAfter.Enabled {
		t.Error("the workspace left behind by a base URL change should be disabled")
	}
	enabled, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatalf("GetEnabled after a base URL change: %v", err)
	}
	if enabled.ID != second.ID {
		t.Errorf("GetEnabled = %q, want the new workspace %q", enabled.ID, second.ID)
	}
	if _, err := tr.ResolveVirtualActor(t.Context(), firstModel.ActorID); !errors.Is(err, ErrNotVirtualActor) {
		t.Errorf("ResolveVirtualActor(old workspace's model) error = %v, want ErrNotVirtualActor", err)
	}
}

// TestSeed_DeactivatesOtherModelsAndReactivatesOnReselection is the
// MVP's "publishes only the default model" rule, plus its reverse: a
// model deactivated by a prior run becomes usable again once it is
// configured as the default.
func TestSeed_DeactivatesOtherModelsAndReactivatesOnReselection(t *testing.T) {
	cfg := validRegistryConfig()
	tr := newTestRegistry(t, cfg)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("seed model A as default: %v", err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	modelA, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	tr.clock.Advance(time.Hour)
	switched := cfg
	switched.DefaultModelID = "gpt-oss:120b"
	switched.ModelSlug = "big-model"
	tr.Registry = NewRegistry(tr.db, tr.db.Repos, switched, tr.clock, nil, nil)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("seed model B as default: %v", err)
	}

	afterSwitch, err := tr.db.OpenWebUIModels.Get(t.Context(), modelA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterSwitch.Active {
		t.Error("model A should be deactivated once it is no longer the default")
	}
	if _, err := tr.ResolveVirtualActor(t.Context(), modelA.ActorID); !errors.Is(err, ErrNotVirtualActor) {
		t.Errorf("ResolveVirtualActor(deactivated model) error = %v, want ErrNotVirtualActor", err)
	}

	workspace, err = tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}
	if modelB.ID == modelA.ID {
		t.Fatal("expected a distinct model row for the new external id")
	}
	if !modelB.Active {
		t.Error("the new default model should be active")
	}

	// Switching back reactivates A rather than leaving it dead forever.
	tr.clock.Advance(time.Hour)
	tr.Registry = NewRegistry(tr.db, tr.db.Repos, cfg, tr.clock, nil, nil)
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatalf("seed model A as default again: %v", err)
	}
	reactivated, err := tr.db.OpenWebUIModels.Get(t.Context(), modelA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reactivated.Active {
		t.Error("model A should be reactivated once it is the default again")
	}
	afterSwitchBack, err := tr.db.OpenWebUIModels.Get(t.Context(), modelB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterSwitchBack.Active {
		t.Error("model B should now be deactivated")
	}
}

func TestSeed_DisabledReturnsErrDisabledAndWritesNothing(t *testing.T) {
	cfg := validRegistryConfig()
	cfg.Enabled = false
	tr := newTestRegistry(t, cfg)

	if err := tr.Seed(t.Context()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Seed() error = %v, want ErrDisabled", err)
	}
	if _, err := tr.db.OpenWebUIWorkspaces.GetByBaseURL(t.Context(), cfg.BaseURL); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a disabled Seed should not have created a workspace row, GetByBaseURL error = %v", err)
	}
}

// TestSeed_RejectsUnknownSecretRef backs ADR-0005 D10: the database may
// only ever hold the one configuration key name this service knows how
// to resolve, never an arbitrary string an operator (or a future bug)
// might put in the config file.
func TestSeed_RejectsUnknownSecretRef(t *testing.T) {
	cfg := validRegistryConfig()
	cfg.SecretRef = "SOME_OTHER_KEY"
	tr := newTestRegistry(t, cfg)

	if err := tr.Seed(t.Context()); !errors.Is(err, ErrInvalidSecretRef) {
		t.Fatalf("Seed() error = %v, want ErrInvalidSecretRef", err)
	}
	if _, err := tr.db.OpenWebUIWorkspaces.GetByBaseURL(t.Context(), cfg.BaseURL); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a rejected Seed should not have created a workspace row, GetByBaseURL error = %v", err)
	}
}

func TestResolveVirtualActor_DisabledReturnsErrDisabled(t *testing.T) {
	cfg := validRegistryConfig()
	cfg.Enabled = false
	tr := newTestRegistry(t, cfg)
	if _, err := tr.ResolveVirtualActor(t.Context(), "does-not-matter"); !errors.Is(err, ErrDisabled) {
		t.Errorf("ResolveVirtualActor() error = %v, want ErrDisabled", err)
	}
}

// TestResolveVirtualActor_RejectsEveryNonModelActor is the projection
// half of "VirtualActors are the only remote-presented actor": the
// owner and the two reserved presentation actors must all fail to
// resolve, so the wire layer's fallback (a plain, non-remote projection)
// is what a caller gets for them.
func TestResolveVirtualActor_RejectsEveryNonModelActor(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	ownerID := mustCreateOwner(t, tr)
	assistant, err := tr.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	system, err := tr.db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}

	for name, actorID := range map[string]string{
		"owner": ownerID, "assistant": assistant.ID, "system": system.ID,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tr.ResolveVirtualActor(t.Context(), actorID); !errors.Is(err, ErrNotVirtualActor) {
				t.Errorf("ResolveVirtualActor(%s) error = %v, want ErrNotVirtualActor", name, err)
			}
		})
	}
}

func TestResolveVirtualActor_UnknownActorIDIsNotFound(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if _, err := tr.ResolveVirtualActor(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ResolveVirtualActor(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestResolveVirtualActor_RequiresEnabledWorkspace is the other gate
// besides the model's own Active flag: disabling the workspace must stop
// its models resolving without touching a single actor or entry row.
func TestResolveVirtualActor_RequiresEnabledWorkspace(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	model, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tr.ResolveVirtualActor(t.Context(), model.ActorID); err != nil {
		t.Fatalf("ResolveVirtualActor before disabling: %v", err)
	}

	if err := tr.db.OpenWebUIWorkspaces.SetEnabled(t.Context(), workspace.ID, false, tr.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ResolveVirtualActor(t.Context(), model.ActorID); !errors.Is(err, ErrNotVirtualActor) {
		t.Errorf("ResolveVirtualActor(disabled workspace) error = %v, want ErrNotVirtualActor", err)
	}

	// The actor row itself is untouched: re-enabling makes it resolve
	// again without re-seeding.
	if err := tr.db.OpenWebUIWorkspaces.SetEnabled(t.Context(), workspace.ID, true, tr.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ResolveVirtualActor(t.Context(), model.ActorID); err != nil {
		t.Errorf("ResolveVirtualActor after re-enabling: %v", err)
	}
}

func TestDefaultVirtualActor_ResolvesTheSeededDefault(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	virtual, err := tr.DefaultVirtualActor(t.Context())
	if err != nil {
		t.Fatalf("DefaultVirtualActor: %v", err)
	}
	if virtual.Handle() != "@model@openwebui.example.net" {
		t.Errorf("Handle() = %q, want @model@openwebui.example.net", virtual.Handle())
	}
}

func TestDefaultVirtualActor_DisabledReturnsErrDisabled(t *testing.T) {
	cfg := validRegistryConfig()
	cfg.Enabled = false
	tr := newTestRegistry(t, cfg)
	if _, err := tr.DefaultVirtualActor(t.Context()); !errors.Is(err, ErrDisabled) {
		t.Errorf("DefaultVirtualActor() error = %v, want ErrDisabled", err)
	}
}

func TestDefaultVirtualActor_NoEnabledWorkspaceIsNotFound(t *testing.T) {
	// Enabled but never seeded: there is no enabled workspace to resolve.
	tr := newTestRegistry(t, validRegistryConfig())
	if _, err := tr.DefaultVirtualActor(t.Context()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DefaultVirtualActor() error = %v, want ErrNotFound", err)
	}
}

// ownerOnlyCall names one owner-only method under test, so the shared
// guard tests below can run identically against all three without
// duplicating the actor-collection boilerplate three times.
type ownerOnlyCall struct {
	name string
	call func(ctx context.Context, tr *testRegistry, actorID string) error
}

func ownerOnlyCalls() []ownerOnlyCall {
	return []ownerOnlyCall{
		{name: "SetGenerationEnabled", call: func(ctx context.Context, tr *testRegistry, actorID string) error {
			return tr.SetGenerationEnabled(ctx, actorID, true)
		}},
		{name: "SetCapabilityStatus", call: func(ctx context.Context, tr *testRegistry, actorID string) error {
			return tr.SetCapabilityStatus(ctx, actorID, domain.CapabilityVerified, domain.CapabilityVerified)
		}},
		{name: "RenameModel", call: func(ctx context.Context, tr *testRegistry, actorID string) error {
			return tr.RenameModel(ctx, actorID, "New Name", "newslug")
		}},
	}
}

// TestOwnerOnlyMethods_RejectNonOwnerActors is the use-case-layer half
// of Issue #52's "owner-only" requirement (miauth's is the HTTP-facing
// half): every write this package exposes re-checks the actor type
// against the database, exactly as miauth.Service.UpdateOwnerDisplayName
// does, so no future caller can reach one of these with a presentation
// actor's id, including the very VirtualActor this package itself
// projects.
func TestOwnerOnlyMethods_RejectNonOwnerActors(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	model, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := tr.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	system, err := tr.db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}

	nonOwners := map[string]string{
		"assistant":    assistant.ID,
		"system":       system.ID,
		"VirtualActor": model.ActorID,
		"unknown":      "does-not-exist",
	}
	for _, oc := range ownerOnlyCalls() {
		for actorName, actorID := range nonOwners {
			t.Run(oc.name+"/"+actorName, func(t *testing.T) {
				err := oc.call(t.Context(), tr, actorID)
				if actorName == "unknown" {
					if !errors.Is(err, domain.ErrNotFound) {
						t.Errorf("%s(unknown actor) error = %v, want ErrNotFound", oc.name, err)
					}
					return
				}
				if !errors.Is(err, ErrNotOwner) {
					t.Errorf("%s(%s) error = %v, want ErrNotOwner", oc.name, actorName, err)
				}
			})
		}
	}
}

// TestOwnerOnlyMethods_SucceedForOwner is the positive case: the owner
// actor may call every one of these, and each write lands.
func TestOwnerOnlyMethods_SucceedForOwner(t *testing.T) {
	tr := newTestRegistry(t, validRegistryConfig())
	if err := tr.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	ownerID := mustCreateOwner(t, tr)

	if err := tr.SetGenerationEnabled(t.Context(), ownerID, true); err != nil {
		t.Fatalf("SetGenerationEnabled: %v", err)
	}
	workspace, err := tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !workspace.GenerationEnabled {
		t.Error("GenerationEnabled = false after SetGenerationEnabled(true)")
	}

	if err := tr.SetCapabilityStatus(t.Context(), ownerID, domain.CapabilityVerified, domain.CapabilityUnsupported); err != nil {
		t.Fatalf("SetCapabilityStatus: %v", err)
	}
	workspace, err = tr.db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if workspace.ChatCreateStatus != domain.CapabilityVerified || workspace.ChatContinueStatus != domain.CapabilityUnsupported {
		t.Errorf("capability statuses = %q/%q, want verified/unsupported", workspace.ChatCreateStatus, workspace.ChatContinueStatus)
	}

	if err := tr.RenameModel(t.Context(), ownerID, "Renamed", "renamed"); err != nil {
		t.Fatalf("RenameModel: %v", err)
	}
	model, err := tr.db.OpenWebUIModels.Get(t.Context(), *workspace.DefaultModelID)
	if err != nil {
		t.Fatal(err)
	}
	if model.DisplayName != "Renamed" || model.ActorSlug != "renamed" {
		t.Errorf("model after RenameModel = %+v, want Renamed/renamed", model)
	}
}

// TestOwnerOnlyMethods_DisabledReturnsErrDisabledBeforeCheckingActor
// checks the guard order: with the feature off, every one of these
// methods must fail closed on ErrDisabled without ever looking up the
// actor, so an invalid or nonexistent actor id never leaks through as a
// different error while the feature is off.
func TestOwnerOnlyMethods_DisabledReturnsErrDisabledBeforeCheckingActor(t *testing.T) {
	cfg := validRegistryConfig()
	cfg.Enabled = false
	tr := newTestRegistry(t, cfg)

	for _, oc := range ownerOnlyCalls() {
		t.Run(oc.name, func(t *testing.T) {
			if err := oc.call(t.Context(), tr, "this-actor-id-does-not-exist"); !errors.Is(err, ErrDisabled) {
				t.Errorf("%s() error = %v, want ErrDisabled", oc.name, err)
			}
		})
	}
}
