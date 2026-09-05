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
	// DisplayName is nil until explicitly set. It is the only Actor field
	// this deployment lets the owner self-edit (Issue #23 PR1's POST
	// /api/i/update); Username has no such field — see
	// docs/compat/aria-v1.5.11.md's "POST /api/i/update" section for why
	// this is deliberately narrower than Misskey's real profile-edit
	// surface, not an oversight.
	DisplayName *string
}

// ActorRepository persists and looks up this service's local actors.
type ActorRepository interface {
	// EnsureReservedActors idempotently creates the Assistant and System
	// actors if they do not already exist. It never creates the Owner
	// actor: that is created transactionally when an operator first
	// approves a local MiAuth session.
	EnsureReservedActors(ctx context.Context) error
	// Create inserts a new actor. It is the only way to create the Owner
	// actor; the actors table's UNIQUE(actor_type) constraint rejects a
	// second Owner row with ErrConflict, so a caller never needs a
	// separate existence check before calling it inside an approval
	// transaction.
	Create(ctx context.Context, a Actor) error
	Get(ctx context.Context, id string) (Actor, error)
	GetByType(ctx context.Context, actorType ActorType) (Actor, error)
	// SetDisplayName updates actorID's mutable display name, including
	// clearing it back to empty. It returns ErrNotFound if actorID does
	// not exist. Callers are responsible for restricting this to the
	// Owner actor (internal/miauth.Service.UpdateOwnerDisplayName does):
	// this method itself applies to whatever actorID it is given.
	SetDisplayName(ctx context.Context, actorID string, displayName string) error
}
