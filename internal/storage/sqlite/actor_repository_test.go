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
	// row of either type (actors.actor_type is UNIQUE).
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
// invariant: actors.actor_type is UNIQUE, so a second Owner actor must be
// rejected as a conflict rather than silently creating a second owner.
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
