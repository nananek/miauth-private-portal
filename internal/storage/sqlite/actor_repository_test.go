package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestActorRepository_EnsureReservedActors_IsIdempotent(t *testing.T) {
	db := newTestDB(t)

	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("first EnsureReservedActors: %v", err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("second EnsureReservedActors: %v", err)
	}

	assistant, err := db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatalf("get assistant actor: %v", err)
	}
	system, err := db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatalf("get system actor: %v", err)
	}
	if assistant.ID == system.ID {
		t.Error("assistant and system actors should have distinct IDs")
	}

	// A second EnsureReservedActors call must not have created a second
	// row of either type (the reserved types are singletons; see
	// idx_actors_singleton_type in migration 0016).
	got, err := db.Actors.Get(t.Context(), assistant.ID)
	if err != nil || got.ID != assistant.ID {
		t.Errorf("assistant actor ID should be stable across calls, got %+v, err %v", got, err)
	}
}

func TestActorRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Actors.Get(t.Context(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestActorRepository_Create(t *testing.T) {
	db := newTestDB(t)
	a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Actors.Get(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != domain.ActorOwner {
		t.Errorf("Type = %q, want owner", got.Type)
	}
}

// TestActorRepository_Create_RejectsSecondOwner backs the single-owner
// invariant: owner is one of the singleton actor types, so a second Owner
// actor must be rejected as a conflict rather than silently creating a
// second owner.
func TestActorRepository_Create_RejectsSecondOwner(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	if err := db.Actors.Create(t.Context(), domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	err := db.Actors.Create(t.Context(), domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: now})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second Create() error = %v, want ErrConflict", err)
	}
}

// TestActorRepository_Create_WithDisplayName covers Issue #23 PR1's
// display_name column: a Create call that provides one must round-trip
// through Get, while TestActorRepository_Create above (no DisplayName)
// already covers the nil/never-set case.
func TestActorRepository_Create_WithDisplayName(t *testing.T) {
	db := newTestDB(t)
	name := "Initial Name"
	a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now(), DisplayName: &name}
	if err := db.Actors.Create(t.Context(), a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Actors.Get(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName == nil || *got.DisplayName != "Initial Name" {
		t.Errorf("DisplayName = %v, want %q", got.DisplayName, "Initial Name")
	}
}

func TestActorRepository_SetDisplayName(t *testing.T) {
	db := newTestDB(t)
	a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), a); err != nil {
		t.Fatal(err)
	}

	if err := db.Actors.SetDisplayName(t.Context(), a.ID, "Updated Name"); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}
	got, err := db.Actors.Get(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName == nil || *got.DisplayName != "Updated Name" {
		t.Errorf("DisplayName after SetDisplayName = %v, want %q", got.DisplayName, "Updated Name")
	}

	// Clearing to "" must persist as an explicit empty string, not leave
	// the column NULL again (see SetDisplayName's doc comment on why "no
	// value written yet" and "explicitly cleared" are kept distinct).
	if err := db.Actors.SetDisplayName(t.Context(), a.ID, ""); err != nil {
		t.Fatalf("SetDisplayName clear: %v", err)
	}
	got, err = db.Actors.Get(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName == nil || *got.DisplayName != "" {
		t.Errorf("DisplayName after clearing = %v, want pointer to empty string", got.DisplayName)
	}
}

func TestActorRepository_SetDisplayName_NotFound(t *testing.T) {
	db := newTestDB(t)
	err := db.Actors.SetDisplayName(t.Context(), "does-not-exist", "x")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetDisplayName() error = %v, want ErrNotFound", err)
	}
}

// TestActorRepository_Create_AllowsManyOpenWebUIModels backs migration
// 0016's partial unique index from the fresh-database side: the reserved
// three types stay singletons (see
// TestActorRepository_Create_RejectsSecondOwner) while
// domain.ActorOpenWebUIModel, the one non-singleton type, may have a row
// per projected model.
func TestActorRepository_Create_AllowsManyOpenWebUIModels(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	first := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: now}
	second := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: now.Add(time.Second)}
	for _, a := range []domain.Actor{first, second} {
		if err := db.Actors.Create(t.Context(), a); err != nil {
			t.Fatalf("Create openwebui_model actor: %v", err)
		}
	}

	got, err := db.Actors.Get(t.Context(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != domain.ActorOpenWebUIModel {
		t.Errorf("Type = %q, want %q", got.Type, domain.ActorOpenWebUIModel)
	}
	if !got.IsRemote() || got.IsLoginable() {
		t.Errorf("openwebui_model actor should be remote and not loginable, got %+v", got)
	}
}

// TestActorRepository_Create_RejectsUnknownActorType backs the other half
// of 0016's rebuilt CHECK: widening it to admit openwebui_model must not
// have turned actor_type into a free-text column.
func TestActorRepository_Create_RejectsUnknownActorType(t *testing.T) {
	db := newTestDB(t)
	err := db.Actors.Create(t.Context(), domain.Actor{
		ID: domain.NewID(), Type: domain.ActorType("openwebui_workspace"), CreatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("Create() with an unknown actor_type should fail the CHECK constraint")
	}
	// A CHECK violation is caller error, not a conflict a caller retries
	// past (see mapWriteError's doc comment).
	if errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create() error = %v, want a plain error rather than ErrConflict", err)
	}
}

func TestActorRepository_ListByType(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().Truncate(time.Second)

	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Created newest-first so a repository that returned insertion order
	// instead of (created_at, id) order would fail below.
	var want []string
	for i, offset := range []time.Duration{2 * time.Second, time.Second, 0} {
		a := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: base.Add(offset)}
		if err := db.Actors.Create(t.Context(), a); err != nil {
			t.Fatalf("create model actor %d: %v", i, err)
		}
		want = append([]string{a.ID}, want...)
	}

	got, err := db.Actors.ListByType(t.Context(), domain.ActorOpenWebUIModel)
	if err != nil {
		t.Fatalf("ListByType: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListByType returned %d actors, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("ListByType[%d].ID = %q, want %q", i, got[i].ID, want[i])
		}
		if got[i].Type != domain.ActorOpenWebUIModel {
			t.Errorf("ListByType[%d].Type = %q, want %q", i, got[i].Type, domain.ActorOpenWebUIModel)
		}
	}

	// The reserved singletons are untouched by, and excluded from, the
	// model listing.
	assistants, err := db.Actors.ListByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	if len(assistants) != 1 {
		t.Errorf("ListByType(assistant) returned %d actors, want 1", len(assistants))
	}
}

// TestActorRepository_ListByType_EmptyIsNotAnError fixes the contract
// documented on domain.ActorRepository.ListByType: no rows is an empty
// slice, not ErrNotFound, so a caller can list before anything is seeded
// (the flag-off Open WebUI case) without special-casing it.
func TestActorRepository_ListByType_EmptyIsNotAnError(t *testing.T) {
	db := newTestDB(t)
	got, err := db.Actors.ListByType(t.Context(), domain.ActorOpenWebUIModel)
	if err != nil {
		t.Fatalf("ListByType: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListByType returned %d actors, want 0", len(got))
	}
}
