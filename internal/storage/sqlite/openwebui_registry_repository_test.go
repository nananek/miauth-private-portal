package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// testTime is a fixed base instant for the Open WebUI repository tests.
// A literal keeps the (created_at, id) ordering assertions below
// reproducible instead of depending on how fast the test machine gets
// through two calls to time.Now.
var testTime = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// mustCreateVirtualActor inserts an actors row of the type migration
// 0016 added, which is what openwebui_models.actor_id has to reference.
func mustCreateVirtualActor(t *testing.T, db *DB) string {
	t.Helper()
	a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: testTime}
	if err := db.Actors.Create(t.Context(), a); err != nil {
		t.Fatalf("create openwebui_model actor: %v", err)
	}
	return a.ID
}

// mustCreateWorkspace inserts a workspace with no default model yet,
// which is how the registry's own seeding creates one: the default is
// pointed at a model after that model exists.
func mustCreateWorkspace(t *testing.T, db *DB, baseURL string) domain.OpenWebUIWorkspace {
	t.Helper()
	w := domain.OpenWebUIWorkspace{
		ID:                 domain.NewID(),
		Name:               "Open WebUI",
		BaseURL:            baseURL,
		SecretRef:          "OPENWEBUI_API_KEY",
		PresentationHost:   "openwebui.example.net",
		ChatCreateStatus:   domain.CapabilityUnverified,
		ChatContinueStatus: domain.CapabilityUnverified,
		CreatedAt:          testTime,
		UpdatedAt:          testTime,
	}
	if err := db.OpenWebUIWorkspaces.Create(t.Context(), w); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return w
}

// mustCreateModel inserts an active model plus the VirtualActor row it
// projects through.
func mustCreateModel(t *testing.T, db *DB, workspaceID, externalModelID, slug string, at time.Time) domain.OpenWebUIModel {
	t.Helper()
	m := domain.OpenWebUIModel{
		ID:              domain.NewID(),
		WorkspaceID:     workspaceID,
		ExternalModelID: externalModelID,
		DisplayName:     "Display " + externalModelID,
		ActorSlug:       slug,
		ActorID:         mustCreateVirtualActor(t, db),
		Active:          true,
		CreatedAt:       at,
		UpdatedAt:       at,
	}
	if err := db.OpenWebUIModels.Create(t.Context(), m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	return m
}

// mustSeedWorkspaceWithDefaultModel is the shape the registry seeds at
// startup: one workspace, one model, the default pointed at it, all in
// one transaction.
func mustSeedWorkspaceWithDefaultModel(t *testing.T, db *DB) (domain.OpenWebUIWorkspace, domain.OpenWebUIModel) {
	t.Helper()
	w := mustCreateWorkspace(t, db, "https://openwebui.example.net")
	m := mustCreateModel(t, db, w.ID, "gpt-oss:20b", "model", testTime)
	if err := db.OpenWebUIWorkspaces.SetDefaultModel(t.Context(), w.ID, m.ID, testTime); err != nil {
		t.Fatalf("set default model: %v", err)
	}
	return w, m
}

func TestOpenWebUIWorkspaceRepository_CreateGetAndLookups(t *testing.T) {
	db := newTestDB(t)
	w := mustCreateWorkspace(t, db, "https://openwebui.example.net")

	got, err := db.OpenWebUIWorkspaces.Get(t.Context(), w.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BaseURL != w.BaseURL || got.SecretRef != "OPENWEBUI_API_KEY" || got.PresentationHost != "openwebui.example.net" {
		t.Errorf("Get() = %+v, want the created workspace", got)
	}
	if got.DefaultModelID != nil {
		t.Errorf("DefaultModelID = %v, want nil before a model exists", got.DefaultModelID)
	}
	// A fresh workspace is off and unverified: nothing about the target
	// is assumed until it has been observed.
	if got.Enabled || got.GenerationEnabled {
		t.Errorf("new workspace enabled = %v, generationEnabled = %v; want both false", got.Enabled, got.GenerationEnabled)
	}
	if got.ChatCreateStatus != domain.CapabilityUnverified || got.ChatContinueStatus != domain.CapabilityUnverified {
		t.Errorf("capability statuses = %q/%q, want both unverified", got.ChatCreateStatus, got.ChatContinueStatus)
	}
	if !got.CreatedAt.Equal(testTime) || !got.UpdatedAt.Equal(testTime) {
		t.Errorf("timestamps = %v/%v, want %v", got.CreatedAt, got.UpdatedAt, testTime)
	}

	byURL, err := db.OpenWebUIWorkspaces.GetByBaseURL(t.Context(), w.BaseURL)
	if err != nil || byURL.ID != w.ID {
		t.Fatalf("GetByBaseURL = %+v, err = %v, want workspace %q", byURL, err, w.ID)
	}
	if _, err := db.OpenWebUIWorkspaces.GetByBaseURL(t.Context(), "https://other.example.net"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByBaseURL(unknown) error = %v, want ErrNotFound", err)
	}
	if _, err := db.OpenWebUIWorkspaces.Get(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestOpenWebUIWorkspaceRepository_Create_RejectsDuplicateBaseURL backs
// the unique base URL that lets startup reconciliation recognise an
// already-registered instance instead of creating a second row for it.
func TestOpenWebUIWorkspaceRepository_Create_RejectsDuplicateBaseURL(t *testing.T) {
	db := newTestDB(t)
	first := mustCreateWorkspace(t, db, "https://openwebui.example.net")

	second := first
	second.ID = domain.NewID()
	if err := db.OpenWebUIWorkspaces.Create(t.Context(), second); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create with a duplicate base URL error = %v, want ErrConflict", err)
	}
}

func TestOpenWebUIWorkspaceRepository_Create_RejectsUnknownCapabilityStatus(t *testing.T) {
	db := newTestDB(t)
	w := domain.OpenWebUIWorkspace{
		ID: domain.NewID(), Name: "n", BaseURL: "https://openwebui.example.net",
		SecretRef: "OPENWEBUI_API_KEY", PresentationHost: "h",
		ChatCreateStatus: domain.CapabilityStatus("probably"), ChatContinueStatus: domain.CapabilityUnverified,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
	if err := db.OpenWebUIWorkspaces.Create(t.Context(), w); err == nil {
		t.Error("Create with an unknown capability status should fail the CHECK constraint")
	}
}

// TestOpenWebUIWorkspaceRepository_GetEnabled covers all three outcomes
// the method distinguishes: none enabled is ErrNotFound (the ordinary
// flag-off state), exactly one is returned, and two is an error rather
// than an arbitrarily chosen winner.
func TestOpenWebUIWorkspaceRepository_GetEnabled(t *testing.T) {
	db := newTestDB(t)
	first := mustCreateWorkspace(t, db, "https://a.example.net")
	second := mustCreateWorkspace(t, db, "https://b.example.net")

	if _, err := db.OpenWebUIWorkspaces.GetEnabled(t.Context()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetEnabled with no enabled workspace error = %v, want ErrNotFound", err)
	}

	if err := db.OpenWebUIWorkspaces.SetEnabled(t.Context(), first.ID, true, testTime); err != nil {
		t.Fatal(err)
	}
	got, err := db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil || got.ID != first.ID {
		t.Fatalf("GetEnabled = %+v, err = %v, want workspace %q", got, err, first.ID)
	}
	if !got.Enabled {
		t.Error("GetEnabled returned a workspace with Enabled false")
	}

	if err := db.OpenWebUIWorkspaces.SetEnabled(t.Context(), second.ID, true, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenWebUIWorkspaces.GetEnabled(t.Context()); err == nil {
		t.Error("GetEnabled with two enabled workspaces should be an error, not a silently chosen winner")
	} else if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetEnabled with two enabled workspaces error = %v, want a distinct invariant error", err)
	}
}

// TestOpenWebUIWorkspaceRepository_SetDefaultModel_EnforcesSameWorkspace
// is the roadmap's "default_model_id points to exactly one model in its
// workspace", enforced by the composite foreign key rather than by a Go
// check that a concurrent writer could slip past.
func TestOpenWebUIWorkspaceRepository_SetDefaultModel_EnforcesSameWorkspace(t *testing.T) {
	db := newTestDB(t)
	owning := mustCreateWorkspace(t, db, "https://a.example.net")
	other := mustCreateWorkspace(t, db, "https://b.example.net")
	model := mustCreateModel(t, db, owning.ID, "gpt-oss:20b", "model", testTime)

	if err := db.OpenWebUIWorkspaces.SetDefaultModel(t.Context(), owning.ID, model.ID, testTime); err != nil {
		t.Fatalf("SetDefaultModel within the owning workspace: %v", err)
	}
	got, err := db.OpenWebUIWorkspaces.Get(t.Context(), owning.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultModelID == nil || *got.DefaultModelID != model.ID {
		t.Fatalf("DefaultModelID = %v, want %q", got.DefaultModelID, model.ID)
	}

	if err := db.OpenWebUIWorkspaces.SetDefaultModel(t.Context(), other.ID, model.ID, testTime); err == nil {
		t.Error("a model from another workspace must not be usable as a default")
	}
	if err := db.OpenWebUIWorkspaces.SetDefaultModel(t.Context(), owning.ID, "does-not-exist", testTime); err == nil {
		t.Error("a nonexistent model must not be usable as a default")
	}

	// The rejected writes left the existing default alone.
	after, err := db.OpenWebUIWorkspaces.Get(t.Context(), owning.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DefaultModelID == nil || *after.DefaultModelID != model.ID {
		t.Errorf("DefaultModelID after rejected writes = %v, want unchanged %q", after.DefaultModelID, model.ID)
	}
	otherAfter, err := db.OpenWebUIWorkspaces.Get(t.Context(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if otherAfter.DefaultModelID != nil {
		t.Errorf("other workspace DefaultModelID = %v, want nil", otherAfter.DefaultModelID)
	}
}

// TestOpenWebUIWorkspaceRepository_UpdateLeavesGatesAlone pins Update's
// deliberately narrow column list: a caller that read a row, renamed it,
// and wrote it back must not be able to re-enable a workspace an
// operator turned off, or move its default model.
func TestOpenWebUIWorkspaceRepository_UpdateLeavesGatesAlone(t *testing.T) {
	db := newTestDB(t)
	w, model := mustSeedWorkspaceWithDefaultModel(t, db)

	later := testTime.Add(time.Hour)
	edited := w
	edited.Name = "Renamed"
	edited.BaseURL = "https://renamed.example.net"
	edited.SecretRef = "OPENWEBUI_API_KEY"
	edited.PresentationHost = "renamed.example.net"
	edited.ChatCreateStatus = domain.CapabilityVerified
	edited.ChatContinueStatus = domain.CapabilityUnsupported
	edited.Enabled = true
	edited.GenerationEnabled = true
	edited.DefaultModelID = nil
	edited.UpdatedAt = later
	if err := db.OpenWebUIWorkspaces.Update(t.Context(), edited); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := db.OpenWebUIWorkspaces.Get(t.Context(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" || got.BaseURL != "https://renamed.example.net" || got.PresentationHost != "renamed.example.net" {
		t.Errorf("Update did not write the mutable fields: %+v", got)
	}
	if got.ChatCreateStatus != domain.CapabilityVerified || got.ChatContinueStatus != domain.CapabilityUnsupported {
		t.Errorf("capability statuses = %q/%q, want verified/unsupported", got.ChatCreateStatus, got.ChatContinueStatus)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, later)
	}
	if got.Enabled || got.GenerationEnabled {
		t.Errorf("Update changed a gate flag: enabled = %v, generationEnabled = %v", got.Enabled, got.GenerationEnabled)
	}
	if got.DefaultModelID == nil || *got.DefaultModelID != model.ID {
		t.Errorf("Update changed the default model: %v, want %q", got.DefaultModelID, model.ID)
	}
	if !got.CreatedAt.Equal(testTime) {
		t.Errorf("Update changed CreatedAt to %v", got.CreatedAt)
	}
}

func TestOpenWebUIWorkspaceRepository_WritesOnUnknownIDAreNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	missing := domain.OpenWebUIWorkspace{
		ID: "does-not-exist", Name: "n", BaseURL: "https://x.example.net", SecretRef: "OPENWEBUI_API_KEY",
		PresentationHost: "h", ChatCreateStatus: domain.CapabilityUnverified,
		ChatContinueStatus: domain.CapabilityUnverified, CreatedAt: testTime, UpdatedAt: testTime,
	}
	for name, err := range map[string]error{
		"Update":              db.OpenWebUIWorkspaces.Update(ctx, missing),
		"SetEnabled":          db.OpenWebUIWorkspaces.SetEnabled(ctx, "does-not-exist", true, testTime),
		"SetCapabilityStatus": db.OpenWebUIWorkspaces.SetCapabilityStatus(ctx, "does-not-exist", domain.CapabilityVerified, domain.CapabilityVerified, testTime),
	} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s on an unknown workspace error = %v, want ErrNotFound", name, err)
		}
	}
}

func TestOpenWebUIWorkspaceRepository_SetCapabilityStatus(t *testing.T) {
	db := newTestDB(t)
	w, _ := mustSeedWorkspaceWithDefaultModel(t, db)
	later := testTime.Add(time.Hour)

	if err := db.OpenWebUIWorkspaces.SetCapabilityStatus(
		t.Context(), w.ID, domain.CapabilityVerified, domain.CapabilityUnsupported, later,
	); err != nil {
		t.Fatalf("SetCapabilityStatus: %v", err)
	}
	got, err := db.OpenWebUIWorkspaces.Get(t.Context(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChatCreateStatus != domain.CapabilityVerified || got.ChatContinueStatus != domain.CapabilityUnsupported {
		t.Errorf("statuses = %q/%q, want verified/unsupported", got.ChatCreateStatus, got.ChatContinueStatus)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, later)
	}
}

// TestOpenWebUIRegistry_DisableWorkspaceAndModelsInOneTransaction is the
// roadmap's "workspace disable and related actor behavior must be one
// transaction". The repositories are per-statement; what makes the pair
// atomic is running both through one UnitOfWork, so this checks that
// composition rather than either write alone — including that a failure
// partway leaves neither applied.
func TestOpenWebUIRegistry_DisableWorkspaceAndModelsInOneTransaction(t *testing.T) {
	db := newTestDB(t)
	w, model := mustSeedWorkspaceWithDefaultModel(t, db)
	if err := db.OpenWebUIWorkspaces.SetEnabled(t.Context(), w.ID, true, testTime); err != nil {
		t.Fatal(err)
	}
	later := testTime.Add(time.Hour)

	wantErr := errors.New("deliberate failure after the first write")
	err := db.WithinTx(t.Context(), func(ctx context.Context, repos domain.Repos) error {
		if err := repos.OpenWebUIWorkspaces.SetEnabled(ctx, w.ID, false, later); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithinTx error = %v, want the deliberate failure", err)
	}
	rolledBack, err := db.OpenWebUIWorkspaces.Get(t.Context(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rolledBack.Enabled {
		t.Fatal("a failed transaction left the workspace disabled")
	}

	if err := db.WithinTx(t.Context(), func(ctx context.Context, repos domain.Repos) error {
		if err := repos.OpenWebUIWorkspaces.SetEnabled(ctx, w.ID, false, later); err != nil {
			return err
		}
		models, err := repos.OpenWebUIModels.ListByWorkspace(ctx, w.ID)
		if err != nil {
			return err
		}
		for _, m := range models {
			if err := repos.OpenWebUIModels.SetActive(ctx, m.ID, false, later); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("disable transaction: %v", err)
	}

	gotWorkspace, err := db.OpenWebUIWorkspaces.Get(t.Context(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotWorkspace.Enabled {
		t.Error("workspace should be disabled after the transaction")
	}
	gotModel, err := db.OpenWebUIModels.Get(t.Context(), model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotModel.Active {
		t.Error("the workspace's model should be inactive after the same transaction")
	}
}

func TestOpenWebUIModelRepository_CreateGetAndLookups(t *testing.T) {
	db := newTestDB(t)
	w, model := mustSeedWorkspaceWithDefaultModel(t, db)

	got, err := db.OpenWebUIModels.Get(t.Context(), model.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExternalModelID != "gpt-oss:20b" || got.ActorSlug != "model" || got.ActorID != model.ActorID {
		t.Errorf("Get() = %+v, want the created model", got)
	}
	if !got.Active {
		t.Error("a newly created model should be active")
	}
	if got.ExternalUpdatedAt != nil {
		t.Errorf("ExternalUpdatedAt = %v, want nil", got.ExternalUpdatedAt)
	}
	if (got.Capabilities != domain.OpenWebUIModelCapabilities{}) {
		t.Errorf("Capabilities = %+v, want nothing verified", got.Capabilities)
	}

	byActor, err := db.OpenWebUIModels.GetByActor(t.Context(), model.ActorID)
	if err != nil || byActor.ID != model.ID {
		t.Fatalf("GetByActor = %+v, err = %v, want model %q", byActor, err, model.ID)
	}
	byExternal, err := db.OpenWebUIModels.GetByExternalID(t.Context(), w.ID, "gpt-oss:20b")
	if err != nil || byExternal.ID != model.ID {
		t.Fatalf("GetByExternalID = %+v, err = %v, want model %q", byExternal, err, model.ID)
	}
	// The external id is only meaningful within its workspace.
	other := mustCreateWorkspace(t, db, "https://b.example.net")
	if _, err := db.OpenWebUIModels.GetByExternalID(t.Context(), other.ID, "gpt-oss:20b"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByExternalID in another workspace error = %v, want ErrNotFound", err)
	}
	if _, err := db.OpenWebUIModels.GetByActor(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByActor(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestOpenWebUIModelRepository_Create_UniquenessConstraints covers the
// three ways two model rows may not overlap: the same provider model
// twice in one workspace, two models sharing one VirtualActor, and two
// models in one workspace sharing a handle slug. The same external id in
// a *different* workspace is fine — it is the provider's namespace, not
// ours.
func TestOpenWebUIModelRepository_Create_UniquenessConstraints(t *testing.T) {
	db := newTestDB(t)
	w := mustCreateWorkspace(t, db, "https://a.example.net")
	existing := mustCreateModel(t, db, w.ID, "gpt-oss:20b", "model", testTime)

	duplicateExternal := existing
	duplicateExternal.ID = domain.NewID()
	duplicateExternal.ActorID = mustCreateVirtualActor(t, db)
	duplicateExternal.ActorSlug = "other"
	if err := db.OpenWebUIModels.Create(t.Context(), duplicateExternal); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate (workspace, external_model_id) error = %v, want ErrConflict", err)
	}

	duplicateActor := existing
	duplicateActor.ID = domain.NewID()
	duplicateActor.ExternalModelID = "other-model"
	duplicateActor.ActorSlug = "other"
	if err := db.OpenWebUIModels.Create(t.Context(), duplicateActor); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate actor_id error = %v, want ErrConflict", err)
	}

	duplicateSlug := existing
	duplicateSlug.ID = domain.NewID()
	duplicateSlug.ExternalModelID = "other-model"
	duplicateSlug.ActorID = mustCreateVirtualActor(t, db)
	if err := db.OpenWebUIModels.Create(t.Context(), duplicateSlug); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate (workspace, actor_slug) error = %v, want ErrConflict", err)
	}

	other := mustCreateWorkspace(t, db, "https://b.example.net")
	elsewhere := mustCreateModel(t, db, other.ID, "gpt-oss:20b", "model", testTime)
	got, err := db.OpenWebUIModels.GetByExternalID(t.Context(), other.ID, "gpt-oss:20b")
	if err != nil {
		t.Fatalf("GetByExternalID in the other workspace: %v", err)
	}
	if got.ID != elsewhere.ID {
		t.Errorf("GetByExternalID resolved to %q, want the other workspace's own model %q", got.ID, elsewhere.ID)
	}
}

func TestOpenWebUIModelRepository_Create_RejectsUnknownWorkspaceOrActor(t *testing.T) {
	db := newTestDB(t)
	w := mustCreateWorkspace(t, db, "https://a.example.net")

	noWorkspace := domain.OpenWebUIModel{
		ID: domain.NewID(), WorkspaceID: "does-not-exist", ExternalModelID: "m", DisplayName: "d",
		ActorSlug: "s", ActorID: mustCreateVirtualActor(t, db), Active: true,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
	if err := db.OpenWebUIModels.Create(t.Context(), noWorkspace); err == nil {
		t.Error("a model in a nonexistent workspace should fail the foreign key")
	}

	noActor := domain.OpenWebUIModel{
		ID: domain.NewID(), WorkspaceID: w.ID, ExternalModelID: "m", DisplayName: "d",
		ActorSlug: "s", ActorID: "does-not-exist", Active: true,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
	if err := db.OpenWebUIModels.Create(t.Context(), noActor); err == nil {
		t.Error("a model naming a nonexistent actor should fail the foreign key")
	}
}

// TestOpenWebUIModelRepository_Update_KeepsIdentityStable is the
// roadmap's stable-actor-ID requirement: renaming a model and changing
// its handle slug must leave its local id, workspace, provider id and
// actor untouched, so entries the actor already authored keep resolving.
func TestOpenWebUIModelRepository_Update_KeepsIdentityStable(t *testing.T) {
	db := newTestDB(t)
	_, model := mustSeedWorkspaceWithDefaultModel(t, db)
	later := testTime.Add(time.Hour)
	externalUpdated := testTime.Add(30 * time.Minute)

	edited := model
	edited.DisplayName = "Renamed Model"
	edited.ActorSlug = "renamed"
	edited.Capabilities = domain.OpenWebUIModelCapabilities{ChatCreate: true, ChatContinue: true}
	edited.ExternalUpdatedAt = &externalUpdated
	edited.UpdatedAt = later
	// Values Update must ignore rather than write.
	edited.WorkspaceID = "does-not-exist"
	edited.ExternalModelID = "rewritten"
	edited.ActorID = "does-not-exist"
	edited.Active = false
	if err := db.OpenWebUIModels.Update(t.Context(), edited); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := db.OpenWebUIModels.Get(t.Context(), model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Renamed Model" || got.ActorSlug != "renamed" {
		t.Errorf("Update did not write the presentation fields: %+v", got)
	}
	if got.Capabilities != (domain.OpenWebUIModelCapabilities{ChatCreate: true, ChatContinue: true}) {
		t.Errorf("Capabilities = %+v, want both true", got.Capabilities)
	}
	if got.ExternalUpdatedAt == nil || !got.ExternalUpdatedAt.Equal(externalUpdated) {
		t.Errorf("ExternalUpdatedAt = %v, want %v", got.ExternalUpdatedAt, externalUpdated)
	}
	if got.ID != model.ID || got.WorkspaceID != model.WorkspaceID ||
		got.ExternalModelID != model.ExternalModelID || got.ActorID != model.ActorID {
		t.Errorf("Update changed an immutable field: %+v, want identity of %+v", got, model)
	}
	if !got.Active {
		t.Error("Update changed the active flag; SetActive owns it")
	}

	// The rename is genuinely reversible on the actor: the same
	// VirtualActor still resolves to this model.
	byActor, err := db.OpenWebUIModels.GetByActor(t.Context(), model.ActorID)
	if err != nil || byActor.ID != model.ID {
		t.Errorf("GetByActor after rename = %+v, err = %v, want model %q", byActor, err, model.ID)
	}
}

func TestOpenWebUIModelRepository_SetActive(t *testing.T) {
	db := newTestDB(t)
	_, model := mustSeedWorkspaceWithDefaultModel(t, db)
	later := testTime.Add(time.Hour)

	if err := db.OpenWebUIModels.SetActive(t.Context(), model.ID, false, later); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	got, err := db.OpenWebUIModels.Get(t.Context(), model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Active {
		t.Error("Active = true after SetActive(false)")
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, later)
	}
	if err := db.OpenWebUIModels.SetActive(t.Context(), "does-not-exist", false, later); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetActive on an unknown model error = %v, want ErrNotFound", err)
	}
}

func TestOpenWebUIModelRepository_ListByWorkspace(t *testing.T) {
	db := newTestDB(t)
	w := mustCreateWorkspace(t, db, "https://a.example.net")
	other := mustCreateWorkspace(t, db, "https://b.example.net")

	// Created newest-first so insertion order and (created_at, id) order
	// disagree.
	third := mustCreateModel(t, db, w.ID, "m3", "s3", testTime.Add(2*time.Minute))
	second := mustCreateModel(t, db, w.ID, "m2", "s2", testTime.Add(time.Minute))
	first := mustCreateModel(t, db, w.ID, "m1", "s1", testTime)
	mustCreateModel(t, db, other.ID, "m1", "s1", testTime)

	got, err := db.OpenWebUIModels.ListByWorkspace(t.Context(), w.ID)
	if err != nil {
		t.Fatalf("ListByWorkspace: %v", err)
	}
	want := []string{first.ID, second.ID, third.ID}
	if len(got) != len(want) {
		t.Fatalf("ListByWorkspace returned %d models, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("ListByWorkspace[%d].ID = %q, want %q", i, got[i].ID, want[i])
		}
	}

	empty, err := db.OpenWebUIModels.ListByWorkspace(t.Context(), "does-not-exist")
	if err != nil {
		t.Fatalf("ListByWorkspace(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByWorkspace(unknown) returned %d models, want 0", len(empty))
	}
}

// TestOpenWebUIModelRepository_CapabilitiesRoundTripThroughStorage is
// the serialization contract at the storage boundary rather than in
// domain: what Encode wrote must be what a later Get decodes, and a
// document written by a newer build (an unknown member) must still load
// rather than making the whole row unreadable.
func TestOpenWebUIModelRepository_CapabilitiesRoundTripThroughStorage(t *testing.T) {
	db := newTestDB(t)
	w := mustCreateWorkspace(t, db, "https://a.example.net")
	model := mustCreateModel(t, db, w.ID, "gpt-oss:20b", "model", testTime)

	want := domain.OpenWebUIModelCapabilities{ChatCreate: true}
	edited := model
	edited.Capabilities = want
	edited.UpdatedAt = testTime
	if err := db.OpenWebUIModels.Update(t.Context(), edited); err != nil {
		t.Fatal(err)
	}
	got, err := db.OpenWebUIModels.Get(t.Context(), model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Capabilities != want {
		t.Errorf("Capabilities = %+v, want %+v", got.Capabilities, want)
	}

	// A document this build did not write, of the kind a newer one
	// would: the known member is read and the unknown one ignored.
	if _, err := db.sqlDB.ExecContext(t.Context(),
		`UPDATE openwebui_models SET capabilities = ? WHERE id = ?`,
		`{"chat_continue":true,"future_capability":"yes"}`, model.ID,
	); err != nil {
		t.Fatal(err)
	}
	got, err = db.OpenWebUIModels.Get(t.Context(), model.ID)
	if err != nil {
		t.Fatalf("a capabilities document with an unknown member should still load: %v", err)
	}
	if got.Capabilities != (domain.OpenWebUIModelCapabilities{ChatContinue: true}) {
		t.Errorf("Capabilities = %+v, want only ChatContinue", got.Capabilities)
	}

	// Corruption, on the other hand, is reported rather than read as
	// "nothing verified".
	if _, err := db.sqlDB.ExecContext(t.Context(),
		`UPDATE openwebui_models SET capabilities = ? WHERE id = ?`, `{"chat_create":`, model.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenWebUIModels.Get(t.Context(), model.ID); err == nil {
		t.Error("a malformed capabilities document should surface as an error")
	}
}
