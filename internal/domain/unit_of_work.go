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
	LocalMiAuth     LocalMiAuthSessionRepository
	APITokens       APITokenRepository
	UserTags        UserTagRepository
	Classifications LLMClassificationRepository
	Generations     LLMGenerationRepository
	Jobs            JobRepository
	ExternalSources ExternalSourceRepository
	ExternalItems   ExternalItemRepository
	Reactions       ReactionRepository
	Mentions        MentionRepository
	Notifications   NotificationRepository

	// The Open WebUI registry and conversation-link repositories (Issue
	// #52). They are always present; whether anything writes through
	// them is the OPENWEBUI_ENABLED feature flag's decision, made in
	// cmd/server, not here.
	OpenWebUIWorkspaces OpenWebUIWorkspaceRepository
	OpenWebUIModels     OpenWebUIModelRepository
	OpenWebUILinks      OpenWebUIConversationLinkRepository
	OpenWebUITurnLinks  OpenWebUITurnLinkRepository

	// Config and ConfigAudit back Issue #76's DB configuration overlay
	// (ADR-0006). Always present, like the Open WebUI repositories
	// above; nothing writes through them until miauthctl config or the
	// startup auto-seed path does.
	Config      ConfigRepository
	ConfigAudit ConfigAuditRepository
}

// UnitOfWork runs fn inside one atomic transaction, so writes made
// through several repositories in Repos (for example, an Entry and its
// durable Job intent) commit or roll back together. Implementations must
// roll back both on a returned error and on a panic from fn.
type UnitOfWork interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, repos Repos) error) error
}
