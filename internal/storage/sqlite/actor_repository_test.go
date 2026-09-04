package sqlite

import (
	"errors"
	"testing"

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
