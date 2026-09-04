package domain

import "context"

// Repos bundles every repository this service persists through. A
// UnitOfWork hands one Repos value to its callback, built against a
// single transaction, so a caller can compose several repositories'
// writes into one atomic commit without ever seeing a storage driver
// type.
type Repos struct {
	Actors          ActorRepository
	Threads         ThreadRepository
	Entries         EntryRepository
	BootstrapGates  BootstrapGateRepository
	LocalMiAuth     LocalMiAuthSessionRepository
	UpstreamMiAuth  UpstreamMiAuthSessionRepository
	APITokens       APITokenRepository
	OwnerBindings   OwnerBindingRepository
	UpstreamTokens  UpstreamTokenRepository
	UserTags        UserTagRepository
	Classifications LLMClassificationRepository
	Generations     LLMGenerationRepository
	Jobs            JobRepository
	ExternalSources ExternalSourceRepository
	ExternalItems   ExternalItemRepository
}

// UnitOfWork runs fn inside one atomic transaction, so writes made
// through several repositories in Repos (for example, an Entry and its
// durable Job intent) commit or roll back together. Implementations must
// roll back both on a returned error and on a panic from fn.
type UnitOfWork interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, repos Repos) error) error
}
