package domain

import "context"

// Repos bundles every repository this service persists through. A
// UnitOfWork hands one Repos value to its callback, built against a
// single transaction, so a caller can compose several repositories'
// writes into one atomic commit without ever seeing a storage driver
// type.
type Repos struct {
	Actors      ActorRepository
	Threads     ThreadRepository
	Entries     EntryRepository
	LocalMiAuth LocalMiAuthSessionRepository
	APITokens   APITokenRepository
	// TokenScopeAudit backs Issue #133's api_token_scope_audit table:
	// miauth.Service.ReflectScopes' change history, written in the same
	// transaction as each APITokens.UpdateScopes call.
	TokenScopeAudit APITokenScopeAuditRepository
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
	// EntryFiles backs Issue #77 PR6's notes/create fileIds attachment
	// (the entry_files many-to-many join table).
	EntryFiles EntryFileRepository

	// The Open WebUI registry and conversation-link repositories (Issue
	// #52). They are always present; whether anything writes through
	// them is the OPENWEBUI_ENABLED feature flag's decision, made in
	// cmd/server, not here.
	OpenWebUIWorkspaces OpenWebUIWorkspaceRepository
	OpenWebUIModels     OpenWebUIModelRepository
	OpenWebUILinks      OpenWebUIConversationLinkRepository
	OpenWebUITurnLinks  OpenWebUITurnLinkRepository

	// Config and ConfigAudit back Issue #76's DB configuration overlay
	// (ADR-0007). Always present, like the Open WebUI repositories
	// above; nothing writes through them until miauthctl config or the
	// startup auto-seed path does.
	Config      ConfigRepository
	ConfigAudit ConfigAuditRepository

	// UserLists backs Issue #115's users/lists/* CRUD and
	// notes/user-list-timeline. Always present; whether anything writes
	// through it is internal/httpserver's route registration decision,
	// mirroring the Files/Folders precedent above.
	UserLists UserListRepository

	// WebAdminBootstrapTokens and WebAdminCredentials back Issue #136
	// Phase 1's admin Web UI bootstrap (ADR-0010): a `miauthctl
	// web-login issue`-minted single-use token and the WebAuthn
	// credential(s) it lets the Owner register. Always present, like
	// UserLists above; nothing writes through them until
	// internal/webadmin.Service does.
	WebAdminBootstrapTokens WebAdminBootstrapTokenRepository
	WebAdminCredentials     WebAdminCredentialRepository
	// WebAdminSessions backs Issue #136 Phase 2's login ceremonies and
	// the sessions they produce (ADR-0010). Always present, like
	// WebAdminBootstrapTokens/WebAdminCredentials above.
	WebAdminSessions WebAdminSessionRepository
	// WebAdminActionAudit backs Issue #136 Phase 3's admin action audit
	// trail (ADR-0010 Decision 8): one row per approve/reject/revoke/
	// reflect-scopes action performed through the Web UI. Always
	// present, like the other WebAdmin* repositories above.
	WebAdminActionAudit WebAdminActionAuditRepository
}

// UnitOfWork runs fn inside one atomic transaction, so writes made
// through several repositories in Repos (for example, an Entry and its
// durable Job intent) commit or roll back together. Implementations must
// roll back both on a returned error and on a panic from fn.
type UnitOfWork interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, repos Repos) error) error
}
