package domain

import (
	"context"
	"time"
)

// ActorType distinguishes this service's local actors. It is still a
// single-owner deployment — owner, assistant and system remain
// singletons — but since Issue #52 an ActorOpenWebUIModel row may exist
// once per projected Open WebUI model.
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
	// ActorOpenWebUIModel is a presentation actor for one Open WebUI
	// model (Issue #52, docs/roadmap/openwebui.md's "VirtualActor").
	// Unlike the other three types it is not a singleton: there is one
	// row per projected model. It is presented to Aria as a remote user
	// (@<slug>@<presentation_host>) purely so a client can tell model
	// replies apart from the assistant's; that host is a fixed
	// presentation value, not federation, and the row is never accepted
	// by MiAuth.
	ActorOpenWebUIModel ActorType = "openwebui_model"
	// ActorExternalSource is a presentation actor for one RSS-kind
	// domain.ExternalSource (Issue #77 PR4, ADR-0007). Like
	// ActorOpenWebUIModel it is not a singleton — one row per registered
	// feed — and is never accepted by MiAuth. Unlike ActorOpenWebUIModel,
	// its projected host (docs/compat/aria-v1.5.11.md's Note.user.host)
	// is the feed's own real origin, not a synthetic deployment-wide
	// value: ADR-0007 records why that is safe for this non-federating
	// deployment and the condition ("Revisit if") that would invalidate
	// the decision. imap-kind sources are deliberately excluded from
	// this type and keep projecting as the shared ActorSystem actor.
	ActorExternalSource ActorType = "external_source"
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

// IsLoginable reports whether this actor can be bound to a local MiAuth
// session and hold local API tokens. Only the owner can: every other
// type exists to author entries, not to authenticate. internal/miauth
// enforces this structurally (it only ever resolves ActorOwner), and
// this predicate lets callers state the same rule without re-deriving
// which types are presentation-only.
func (a Actor) IsLoginable() bool { return a.Type == ActorOwner }

// CanMiAuth reports whether a local MiAuth approval may bind to this
// actor. It is deliberately a separate predicate from IsLoginable rather
// than an alias: they answer to different rules (ADR-0002's host-local
// approval versus token issuance), and a future actor type could
// plausibly change one without the other.
func (a Actor) CanMiAuth() bool { return a.Type == ActorOwner }

// CanOwnSecret reports whether credentials may be attached to this actor
// row. No actor type may: ADR-0005 D10 keeps every external credential
// in configuration, and the database stores only the config key's name
// (a secret_ref), never a secret value. The predicate exists so that
// rule is asserted in one place instead of being an unstated property of
// there simply being no column for it.
func (a Actor) CanOwnSecret() bool { return false }

// IsRemote reports whether this actor is projected to Aria with a
// non-null UserLite.host. ActorOpenWebUIModel and ActorExternalSource
// are; see their doc comments for why that is presentation (or, for
// ActorExternalSource, a deliberately real-but-non-federating host —
// ADR-0007), never actual federation.
func (a Actor) IsRemote() bool {
	return a.Type == ActorOpenWebUIModel || a.Type == ActorExternalSource
}

// IsPresentationOnly reports whether this actor exists solely to author
// and label entries — the inverse of IsLoginable.
func (a Actor) IsPresentationOnly() bool { return !a.IsLoginable() }

// ActorRepository persists and looks up this service's local actors.
type ActorRepository interface {
	// EnsureReservedActors idempotently creates the Assistant and System
	// actors if they do not already exist. It never creates the Owner
	// actor: that is created transactionally when an operator first
	// approves a local MiAuth session.
	EnsureReservedActors(ctx context.Context) error
	// Create inserts a new actor. It is the only way to create the Owner
	// actor; the actors table's partial unique index over the singleton
	// types (migration 0016) rejects a second Owner row with ErrConflict,
	// so a caller never needs a separate existence check before calling
	// it inside an approval transaction.
	Create(ctx context.Context, a Actor) error
	Get(ctx context.Context, id string) (Actor, error)
	// GetByType returns the single actor of a singleton type (owner,
	// assistant, system). It is not meaningful for ActorOpenWebUIModel,
	// which may have many rows; use ListByType for that.
	GetByType(ctx context.Context, actorType ActorType) (Actor, error)
	// ListByType returns every actor of the given type in a stable
	// (created_at, id) order. It exists for ActorOpenWebUIModel, the one
	// non-singleton type. Having no actors of a type is not an error: it
	// returns an empty result, not ErrNotFound.
	ListByType(ctx context.Context, actorType ActorType) ([]Actor, error)
	// SetDisplayName updates actorID's mutable display name, including
	// clearing it back to empty. It returns ErrNotFound if actorID does
	// not exist. Callers are responsible for restricting this to the
	// Owner actor (internal/miauth.Service.UpdateOwnerDisplayName does):
	// this method itself applies to whatever actorID it is given.
	SetDisplayName(ctx context.Context, actorID string, displayName string) error
}
