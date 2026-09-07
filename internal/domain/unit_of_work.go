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

	// Files and Folders back internal/drive.Service (Issue #77 PR3): the
	// files table's row-level metadata store, and PR3's new folders
	// table. Always present; whether anything writes through them is
	// internal/httpserver's Drive route registration decision (mirrors
	// PR1's "table ships now, repository does not" precedent — PR3 is
	// the repository).
	Files   FileRepository
	Folders FolderRepository

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
