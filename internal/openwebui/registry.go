// Package openwebui is the use-case layer for Issue #52's Open WebUI
// registry and identity projection: it turns the operator's
// configuration into the workspace, model and VirtualActor rows the
// storage layer holds, and answers "which VirtualActor, if any, does
// this actor id project to?" for the Aria wire layer.
//
// It depends only on internal/domain. There is no HTTP client, no SQL,
// and no Open WebUI endpoint knowledge here — the provider adapter is
// Issue #53's, and this package must not grow one. Configuration reaches
// it as the plain-typed RegistryConfig below rather than as a
// config.Config, the same translation every other use-case package gets
// from cmd/server.
package openwebui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

var (
	// ErrDisabled reports that the feature is off (OPENWEBUI_ENABLED is
	// false). Every method returns it rather than silently doing
	// nothing: a caller reaching this package with the feature disabled
	// has a wiring bug, and the projection layer treats any error as
	// "not a VirtualActor" anyway.
	ErrDisabled = errors.New("openwebui: feature is disabled")
	// ErrNotOwner reports that the actor asking for a change is not the
	// owner. It mirrors miauth.ErrNotOwner's role for POST /api/i/update:
	// there is no HTTP path to these methods today, so this is the
	// second lock on a door that is not yet built — but it is the lock
	// that has to already be there when Issue #53 builds it.
	ErrNotOwner = errors.New("openwebui: actor is not the owner")
	// ErrNotVirtualActor reports that an actor id does not project to a
	// usable VirtualActor: it is not an Open WebUI model actor, or its
	// model is inactive, or its workspace is disabled.
	ErrNotVirtualActor = errors.New("openwebui: actor is not a projectable Open WebUI model")
	// ErrInvalidSecretRef reports a secret_ref that is not the one
	// configuration key this service stores. ADR-0005 D10 keeps
	// credentials in configuration and the database holds only the key's
	// name, so a secret_ref naming anything else is either a typo an
	// operator would never be able to resolve, or an attempt to put
	// something other than a key name in that column.
	ErrInvalidSecretRef = errors.New("openwebui: secret_ref is not a known credential configuration key")
)

// SecretRefAPIKey is the only value this package writes into a
// workspace's secret_ref column: the name of the configuration key that
// holds the Open WebUI API key (config.KeyOpenWebUIAPIKey). It is
// duplicated here rather than imported so this package keeps depending
// on internal/domain alone; cmd/server passes the config constant into
// RegistryConfig.SecretRef, so the two are checked against each other at
// every startup and a rename fails closed instead of silently writing an
// unresolvable reference.
const SecretRefAPIKey = "OPENWEBUI_API_KEY"

// Clock supplies the current time, so tests can pin timestamps.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RegistryConfig is the operator's configuration, already validated by
// internal/config and flattened to plain types.
type RegistryConfig struct {
	// Enabled mirrors OPENWEBUI_ENABLED.
	Enabled bool
	// BaseURL identifies the instance and is the key Seed reconciles on:
	// a workspace row already carrying this base URL is updated in
	// place, keeping its id, rather than a second row being created.
	BaseURL string
	// SecretRef is the *name* of the configuration key holding the API
	// key, never the key itself. Seed rejects anything but
	// SecretRefAPIKey.
	SecretRef string
	// WorkspaceName, PresentationHost, ModelDisplayName and ModelSlug
	// are presentation values Seed reconciles onto the existing rows.
	WorkspaceName    string
	PresentationHost string
	ModelDisplayName string
	ModelSlug        string
	// DefaultModelID is the provider's own opaque model id. It is the
	// identity Seed reconciles a model row on, so changing it registers
	// a different model rather than renaming this one.
	DefaultModelID string
}

// Registry owns the Open WebUI workspace/model registry: seeding it from
// configuration at startup, projecting a model actor into the
// VirtualActor the wire layer needs, and applying the few owner-only
// changes Issue #53 will need.
type Registry struct {
	uow   domain.UnitOfWork
	repos domain.Repos
	clock Clock
	cfg   RegistryConfig
}

// NewRegistry builds a Registry. uow and repos commonly come from one
// storage adapter, but no concrete adapter type crosses this boundary.
func NewRegistry(uow domain.UnitOfWork, repos domain.Repos, cfg RegistryConfig, clock Clock) *Registry {
	if clock == nil {
		clock = realClock{}
	}
	return &Registry{uow: uow, repos: repos, clock: clock, cfg: cfg}
}

// Seed reconciles the configured instance and model into the registry,
// in one transaction, idempotently. cmd/server calls it at startup when
// the feature is enabled — the same "startup seeding" shape RSS uses for
// RSS_FEED_URLS, and for the same reason: the operator's configuration
// file is the owner-only interface, because only someone with host
// access can edit it (ADR-0002's host-local principle).
//
// It performs, in order: find-or-create the workspace by base URL;
// find-or-create the model by the provider's opaque model id, minting
// its VirtualActor row on first sight; point the workspace's default at
// it; deactivate every other model in the workspace, since the MVP
// publishes only the default one; disable every *other* workspace (and
// deactivate its models) — this deployment supports exactly one enabled
// workspace, so a re-seed after OPENWEBUI_BASE_URL changes must not
// leave the previous instance's workspace enabled alongside the new
// one; and enable the workspace.
//
// Two identities are deliberately never rewritten by a re-run: the
// workspace id and the model's actor id. That is the roadmap's stable
// actor ID requirement — renaming a model or changing its handle must
// not orphan the entries its actor already authored — and it is why
// display name and slug are the only presentation fields updated here.
//
// It is a single transaction because a half-applied registry is worse
// than none: a workspace enabled before its default model exists would
// advertise a model nothing could resolve.
func (r *Registry) Seed(ctx context.Context) error {
	if !r.cfg.Enabled {
		return ErrDisabled
	}
	if r.cfg.SecretRef != SecretRefAPIKey {
		return ErrInvalidSecretRef
	}

	now := r.clock.Now().UTC()
	return r.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		workspace, err := r.seedWorkspace(ctx, repos, now)
		if err != nil {
			return err
		}
		model, err := r.seedModel(ctx, repos, workspace.ID, now)
		if err != nil {
			return err
		}
		if err := repos.OpenWebUIWorkspaces.SetDefaultModel(ctx, workspace.ID, model.ID, now); err != nil {
			return fmt.Errorf("set default model: %w", err)
		}
		if err := r.deactivateOtherModels(ctx, repos, workspace.ID, model.ID, now); err != nil {
			return err
		}
		if err := r.deactivateOtherWorkspaces(ctx, repos, workspace.ID, now); err != nil {
			return err
		}
		if err := repos.OpenWebUIWorkspaces.SetEnabled(ctx, workspace.ID, true, now); err != nil {
			return fmt.Errorf("enable workspace: %w", err)
		}
		return nil
	})
}

// seedWorkspace finds the workspace by base URL or creates it. The
// created row starts disabled with no default model: Seed enables it
// only once the model it will point at exists.
func (r *Registry) seedWorkspace(ctx context.Context, repos domain.Repos, now time.Time) (domain.OpenWebUIWorkspace, error) {
	existing, err := repos.OpenWebUIWorkspaces.GetByBaseURL(ctx, r.cfg.BaseURL)
	switch {
	case err == nil:
		existing.Name = r.cfg.WorkspaceName
		existing.PresentationHost = r.cfg.PresentationHost
		existing.SecretRef = r.cfg.SecretRef
		existing.UpdatedAt = now
		if err := repos.OpenWebUIWorkspaces.Update(ctx, existing); err != nil {
			return domain.OpenWebUIWorkspace{}, fmt.Errorf("update workspace: %w", err)
		}
		return existing, nil
	case errors.Is(err, domain.ErrNotFound):
		created := domain.OpenWebUIWorkspace{
			ID:                 domain.NewID(),
			Name:               r.cfg.WorkspaceName,
			BaseURL:            r.cfg.BaseURL,
			SecretRef:          r.cfg.SecretRef,
			PresentationHost:   r.cfg.PresentationHost,
			ChatCreateStatus:   domain.CapabilityUnverified,
			ChatContinueStatus: domain.CapabilityUnverified,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		if err := repos.OpenWebUIWorkspaces.Create(ctx, created); err != nil {
			return domain.OpenWebUIWorkspace{}, fmt.Errorf("create workspace: %w", err)
		}
		return created, nil
	default:
		return domain.OpenWebUIWorkspace{}, fmt.Errorf("look up workspace: %w", err)
	}
}

// seedModel finds the model by the provider's opaque id or creates it,
// minting the VirtualActor row on first sight.
//
// The actor row's display_name is deliberately left unset. A
// VirtualActor's display name lives in the registry
// (openwebui_models.display_name), because actors.display_name is the
// owner's own self-edited field (Issue #23 PR1's POST /api/i/update) and
// having two places that could answer "what is this model called?" is
// how they come to disagree.
func (r *Registry) seedModel(ctx context.Context, repos domain.Repos, workspaceID string, now time.Time) (domain.OpenWebUIModel, error) {
	existing, err := repos.OpenWebUIModels.GetByExternalID(ctx, workspaceID, r.cfg.DefaultModelID)
	switch {
	case err == nil:
		existing.DisplayName = r.cfg.ModelDisplayName
		existing.ActorSlug = r.cfg.ModelSlug
		existing.UpdatedAt = now
		if err := repos.OpenWebUIModels.Update(ctx, existing); err != nil {
			return domain.OpenWebUIModel{}, fmt.Errorf("update model: %w", err)
		}
		// A model that had been deactivated by an earlier run (because
		// it was not the default then) becomes usable again on being
		// configured as the default.
		if !existing.Active {
			if err := repos.OpenWebUIModels.SetActive(ctx, existing.ID, true, now); err != nil {
				return domain.OpenWebUIModel{}, fmt.Errorf("reactivate model: %w", err)
			}
			existing.Active = true
		}
		return existing, nil
	case errors.Is(err, domain.ErrNotFound):
		actor := domain.Actor{ID: domain.NewID(), Type: domain.ActorOpenWebUIModel, CreatedAt: now}
		if err := repos.Actors.Create(ctx, actor); err != nil {
			return domain.OpenWebUIModel{}, fmt.Errorf("create model actor: %w", err)
		}
		created := domain.OpenWebUIModel{
			ID:              domain.NewID(),
			WorkspaceID:     workspaceID,
			ExternalModelID: r.cfg.DefaultModelID,
			DisplayName:     r.cfg.ModelDisplayName,
			ActorSlug:       r.cfg.ModelSlug,
			ActorID:         actor.ID,
			Active:          true,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := repos.OpenWebUIModels.Create(ctx, created); err != nil {
			return domain.OpenWebUIModel{}, fmt.Errorf("create model: %w", err)
		}
		return created, nil
	default:
		return domain.OpenWebUIModel{}, fmt.Errorf("look up model: %w", err)
	}
}

// deactivateOtherModels marks every model but the default inactive. The
// MVP publishes only the default model as an actor (roadmap OWUI-P), and
// deactivating rather than deleting keeps the actor row that entries
// already reference.
func (r *Registry) deactivateOtherModels(ctx context.Context, repos domain.Repos, workspaceID, defaultModelID string, now time.Time) error {
	models, err := repos.OpenWebUIModels.ListByWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("list workspace models: %w", err)
	}
	for _, m := range models {
		if m.ID == defaultModelID || !m.Active {
			continue
		}
		if err := repos.OpenWebUIModels.SetActive(ctx, m.ID, false, now); err != nil {
			return fmt.Errorf("deactivate model: %w", err)
		}
	}
	return nil
}

// deactivateOtherWorkspaces disables every workspace but targetWorkspaceID
// and deactivates each one's models along with it, in the same
// transaction as the rest of Seed.
//
// This is what keeps GetEnabled's single-enabled-workspace invariant
// true across a re-seed at a changed OPENWEBUI_BASE_URL: seedWorkspace
// only ever finds-or-creates the workspace named by the *current*
// config and only ever enables that one, so without this step a
// previously enabled workspace (and the model actor it was projecting
// under its own presentation_host) would stay enabled forever —
// breaking GetEnabled/DefaultVirtualActor/every owner-only write with
// "more than one enabled Open WebUI workspace" the next time anything
// calls them, and leaving that stale model resolvable through
// ResolveVirtualActor indefinitely. Disabling it here, atomically with
// enabling the new one, is what a single-workspace deployment actually
// switching instances requires.
func (r *Registry) deactivateOtherWorkspaces(ctx context.Context, repos domain.Repos, targetWorkspaceID string, now time.Time) error {
	workspaces, err := repos.OpenWebUIWorkspaces.List(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	for _, w := range workspaces {
		if w.ID == targetWorkspaceID {
			continue
		}
		if w.Enabled {
			if err := repos.OpenWebUIWorkspaces.SetEnabled(ctx, w.ID, false, now); err != nil {
				return fmt.Errorf("disable workspace: %w", err)
			}
		}
		models, err := repos.OpenWebUIModels.ListByWorkspace(ctx, w.ID)
		if err != nil {
			return fmt.Errorf("list workspace models: %w", err)
		}
		for _, m := range models {
			if !m.Active {
				continue
			}
			if err := repos.OpenWebUIModels.SetActive(ctx, m.ID, false, now); err != nil {
				return fmt.Errorf("deactivate model: %w", err)
			}
		}
	}
	return nil
}

// ResolveVirtualActor projects actorID onto its VirtualActor, or returns
// ErrNotVirtualActor when it does not have one to project.
//
// All three gates matter and are checked here rather than by the caller:
// the actor must be an Open WebUI model actor, its model must be active,
// and its workspace must be enabled. Disabling a workspace therefore
// stops its models being presented as remote users without touching a
// single entry — the entries stay, and their author falls back to the
// wire layer's plain projection.
func (r *Registry) ResolveVirtualActor(ctx context.Context, actorID string) (domain.VirtualActor, error) {
	if !r.cfg.Enabled {
		return domain.VirtualActor{}, ErrDisabled
	}

	actor, err := r.repos.Actors.Get(ctx, actorID)
	if err != nil {
		return domain.VirtualActor{}, err
	}
	if actor.Type != domain.ActorOpenWebUIModel {
		return domain.VirtualActor{}, ErrNotVirtualActor
	}

	model, err := r.repos.OpenWebUIModels.GetByActor(ctx, actorID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.VirtualActor{}, ErrNotVirtualActor
		}
		return domain.VirtualActor{}, err
	}
	if !model.Active {
		return domain.VirtualActor{}, ErrNotVirtualActor
	}

	workspace, err := r.repos.OpenWebUIWorkspaces.Get(ctx, model.WorkspaceID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.VirtualActor{}, ErrNotVirtualActor
		}
		return domain.VirtualActor{}, err
	}
	if !workspace.Enabled {
		return domain.VirtualActor{}, ErrNotVirtualActor
	}

	return domain.VirtualActor{
		ActorID:     actor.ID,
		Slug:        model.ActorSlug,
		Host:        workspace.PresentationHost,
		DisplayName: model.DisplayName,
		WorkspaceID: workspace.ID,
		ModelID:     model.ID,
	}, nil
}

// DefaultVirtualActor returns the enabled workspace's default model as a
// VirtualActor. Issue #53's bridge needs it to know who authors a
// generated reply; nothing in Issue #52 routes anything to it.
func (r *Registry) DefaultVirtualActor(ctx context.Context) (domain.VirtualActor, error) {
	if !r.cfg.Enabled {
		return domain.VirtualActor{}, ErrDisabled
	}
	workspace, err := r.repos.OpenWebUIWorkspaces.GetEnabled(ctx)
	if err != nil {
		return domain.VirtualActor{}, err
	}
	if workspace.DefaultModelID == nil {
		return domain.VirtualActor{}, ErrNotVirtualActor
	}
	model, err := r.repos.OpenWebUIModels.Get(ctx, *workspace.DefaultModelID)
	if err != nil {
		return domain.VirtualActor{}, err
	}
	return r.ResolveVirtualActor(ctx, model.ActorID)
}

// SetGenerationEnabled turns outbound generation on or off for the
// enabled workspace. Issue #53 is what makes the flag mean anything;
// until then it is a stored intent.
func (r *Registry) SetGenerationEnabled(ctx context.Context, actorID string, enabled bool) error {
	return r.ownerOnly(ctx, actorID, func(ctx context.Context, repos domain.Repos, workspace domain.OpenWebUIWorkspace, now time.Time) error {
		// Update deliberately cannot write the enable flags (see
		// OpenWebUIWorkspaceRepository.Update), so this goes through the
		// dedicated write for the workspace gate's sibling field.
		return repos.OpenWebUIWorkspaces.SetGenerationEnabled(ctx, workspace.ID, enabled, now)
	})
}

// SetCapabilityStatus records what has actually been observed about the
// instance's two provider operations (ADR-0005 D3). It is owner-only
// because the answer gates whether the feature may be used at all.
func (r *Registry) SetCapabilityStatus(ctx context.Context, actorID string, chatCreate, chatContinue domain.CapabilityStatus) error {
	return r.ownerOnly(ctx, actorID, func(ctx context.Context, repos domain.Repos, workspace domain.OpenWebUIWorkspace, now time.Time) error {
		return repos.OpenWebUIWorkspaces.SetCapabilityStatus(ctx, workspace.ID, chatCreate, chatContinue, now)
	})
}

// RenameModel changes the default model's display name and handle slug.
// Its local id and actor id are untouched, which is the point of having
// a rename path at all: the roadmap requires a model actor's local ID to
// survive exactly this.
func (r *Registry) RenameModel(ctx context.Context, actorID, displayName, slug string) error {
	return r.ownerOnly(ctx, actorID, func(ctx context.Context, repos domain.Repos, workspace domain.OpenWebUIWorkspace, now time.Time) error {
		if workspace.DefaultModelID == nil {
			return ErrNotVirtualActor
		}
		model, err := repos.OpenWebUIModels.Get(ctx, *workspace.DefaultModelID)
		if err != nil {
			return err
		}
		model.DisplayName = displayName
		model.ActorSlug = slug
		model.UpdatedAt = now
		return repos.OpenWebUIModels.Update(ctx, model)
	})
}

// ownerOnly is the shared guard for every change this package exposes.
// It re-checks the actor type against the database rather than trusting
// the caller, exactly as miauth.Service.UpdateOwnerDisplayName does, so
// no future caller — an HTTP handler, a CLI, a job — can reach a
// registry write with a presentation actor's id. The whole operation,
// guard included, runs in one transaction so the check cannot be made
// stale by a concurrent write between it and the change.
func (r *Registry) ownerOnly(
	ctx context.Context,
	actorID string,
	apply func(ctx context.Context, repos domain.Repos, workspace domain.OpenWebUIWorkspace, now time.Time) error,
) error {
	if !r.cfg.Enabled {
		return ErrDisabled
	}
	now := r.clock.Now().UTC()
	return r.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		actor, err := repos.Actors.Get(ctx, actorID)
		if err != nil {
			return err
		}
		if !actor.IsLoginable() {
			return ErrNotOwner
		}
		workspace, err := repos.OpenWebUIWorkspaces.GetEnabled(ctx)
		if err != nil {
			return err
		}
		return apply(ctx, repos, workspace, now)
	})
}
