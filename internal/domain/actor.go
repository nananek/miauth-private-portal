package domain

import (
	"context"
	"time"
)

// ActorType distinguishes the service's three fixed local actors. This is
// a single-owner deployment: there is at most one Actor of each type.
type ActorType string

const (
	// ActorOwner is the only login-capable local actor.
	ActorOwner ActorType = "owner"
	// ActorAssistant is a presentation actor for LLM-generated replies
	// and follow-up questions. It is never accepted by MiAuth.
	ActorAssistant ActorType = "assistant"
	// ActorSystem is a presentation actor for ingestion/status entries.
	// It is never accepted by MiAuth.
	ActorSystem ActorType = "system"
)

// Actor is a local identity: the single login-capable owner, or one of
// the two reserved presentation actors used to author generated or
// ingested entries.
type Actor struct {
	ID        string
	Type      ActorType
	CreatedAt time.Time
}

// ActorRepository persists and looks up this service's local actors.
type ActorRepository interface {
	// EnsureReservedActors idempotently creates the Assistant and System
	// actors if they do not already exist. It never creates the Owner
	// actor: that is created transactionally alongside an OwnerBinding
	// (see OwnerBindingRepository.Bind).
	EnsureReservedActors(ctx context.Context) error
	// Create inserts a new actor. It is the only way to create the Owner
	// actor; the actors table's UNIQUE(actor_type) constraint rejects a
	// second Owner row with ErrConflict, so a caller never needs a
	// separate existence check before calling it inside the same
	// owner-binding transaction as OwnerBindingRepository.Bind.
	Create(ctx context.Context, a Actor) error
	Get(ctx context.Context, id string) (Actor, error)
	GetByType(ctx context.Context, actorType ActorType) (Actor, error)
}
