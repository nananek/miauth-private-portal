package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// This file holds the Open WebUI registry and conversation-link model
// (Issue #52, docs/roadmap/openwebui.md's OWUI-P section, ADR-0005).
//
// Two rules from those documents shape almost every type here:
//
//   - Local identifiers are the only source of truth (ADR-0005 D2). Every
//     Remote* field below is an opaque, nullable correlation string. None
//     of them is ever used for identity, ordering, authorization, or
//     pagination, and none reaches an Aria payload.
//   - The provider is treated as a store that will faithfully persist
//     whatever it is told, including nonsense (ADR-0005 D4). Every
//     eligibility rule is therefore decided locally and fail-closed,
//     which is what the LinkState machine below exists for.
//
// Nothing in this file talks to Open WebUI. It describes what this
// service records about it.

// ErrInvalidLinkTransition is returned by LinkTransition for any
// (state, event) pair the conversation-link state machine does not
// allow. It is deliberately one sentinel rather than one per pair:
// callers fail closed on all of them identically.
var ErrInvalidLinkTransition = errors.New("invalid conversation link transition")

// CapabilityStatus records whether a capability of the configured Open
// WebUI instance has actually been observed to work, as opposed to
// assumed. It starts unverified and is only ever advanced by evidence
// (ADR-0005 D14's re-verification on upgrade).
type CapabilityStatus string

const (
	// CapabilityUnverified means the capability has not been exercised
	// against this instance yet. It is the default: this service never
	// assumes an endpoint works because a version number suggests it
	// should.
	CapabilityUnverified CapabilityStatus = "unverified"
	// CapabilityVerified means the capability was observed working
	// against this instance.
	CapabilityVerified CapabilityStatus = "verified"
	// CapabilityUnsupported means the capability was observed not to
	// work. The roadmap's fallback for that is to disable the feature,
	// not to improvise around it.
	CapabilityUnsupported CapabilityStatus = "unsupported"
)

// OpenWebUIWorkspace is the roadmap's WorkspaceDefinition: one Open WebUI
// instance (BaseURL) plus one account (SecretRef). ADR-0005 D9 pins that
// naming — Open WebUI itself has no "workspace" API object, and the
// product's own Workspace screen is unrelated model/prompt management.
//
// This deployment has at most one enabled workspace.
type OpenWebUIWorkspace struct {
	// ID is local, opaque, and immutable for the workspace's lifetime,
	// including across BaseURL or credential changes.
	ID string
	// Name is the owner-configured display name.
	Name string
	// BaseURL is an HTTPS origin only — no userinfo, path, query, or
	// fragment — and must be a member of the configured origin
	// allowlist (ADR-0005 D11). This package does not validate it; the
	// configuration layer does, before a workspace is ever created.
	BaseURL string
	// SecretRef names the configuration key holding the provider API
	// key. It is a key *name*, never a credential: ADR-0005 D10 keeps
	// every secret in configuration, so no value derived from one is
	// persisted, logged, or projected.
	SecretRef string
	// PresentationHost is the fixed, deployment-provisioned host in a
	// VirtualActor's @slug@host handle. It is never inferred from
	// BaseURL, and it is presentation only: this service does not
	// discover, resolve, or federate with it.
	PresentationHost string
	// DefaultModelID names a model in this same workspace, or is nil
	// before one has been registered. The database enforces the
	// same-workspace part with a composite foreign key; nothing in Go
	// needs to re-check it.
	DefaultModelID *string
	// Enabled gates the whole workspace. A disabled workspace's models
	// are not projected and cannot be used to create anything.
	Enabled bool
	// GenerationEnabled gates outbound generation specifically, which
	// Issue #53 implements. It stays false in Issue #52: there is no
	// bridge for it to turn on yet.
	GenerationEnabled bool
	// ChatCreateStatus and ChatContinueStatus record whether this
	// instance has been observed to support creating a chat and
	// continuing a turn (ADR-0005 D3's two provider operations).
	ChatCreateStatus   CapabilityStatus
	ChatContinueStatus CapabilityStatus
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// OpenWebUIModelCapabilities is the verified capability set of one model.
// Its JSON form is what the registry stores; see
// ParseOpenWebUIModelCapabilities for the decoding rules.
type OpenWebUIModelCapabilities struct {
	ChatCreate   bool `json:"chat_create"`
	ChatContinue bool `json:"chat_continue"`
}

// ParseOpenWebUIModelCapabilities decodes a stored capabilities
// document. Three inputs all mean "nothing verified yet" rather than an
// error, because all three are states the column legitimately reaches:
// the empty string (a row written before this document had a shape),
// "null", and "{}". Unknown members are ignored, so a document written
// by a newer build downgrades to the capabilities this build understands
// instead of failing to load the row at all.
//
// Malformed JSON is still an error: that is corruption, not a version
// skew, and silently reading it as "no capabilities" would present a
// model as unusable for a reason no one could see.
func ParseOpenWebUIModelCapabilities(raw string) (OpenWebUIModelCapabilities, error) {
	if raw == "" {
		return OpenWebUIModelCapabilities{}, nil
	}
	var c OpenWebUIModelCapabilities
	// A JSON "null" leaves c at its zero value and returns no error,
	// which is exactly the wanted behaviour.
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return OpenWebUIModelCapabilities{}, err
	}
	return c, nil
}

// Encode renders the capabilities for storage.
func (c OpenWebUIModelCapabilities) Encode() string {
	// json.Marshal cannot fail here: the value is a struct of bools with
	// no custom marshaller, so there is no unsupported type, cyclic
	// value, or failing MarshalJSON for it to report.
	encoded, _ := json.Marshal(c)
	return string(encoded)
}

// OpenWebUIModel is the roadmap's ModelDefinition: one model offered by
// one workspace, projected to Aria through ActorID's VirtualActor.
type OpenWebUIModel struct {
	// ID and WorkspaceID are local, opaque, and immutable.
	ID          string
	WorkspaceID string
	// ExternalModelID is the provider's own model id (ADR-0005 D9: a
	// GET /api/models data[].id, or a workspace custom-model id). It is
	// opaque and immutable — never regenerated from DisplayName, never
	// parsed, never compared for ordering.
	ExternalModelID string
	// DisplayName and ActorSlug are presentation, and both may change
	// without disturbing ID or ActorID. The roadmap requires exactly
	// that: a model actor's stable local ID must survive display-name or
	// handle changes.
	DisplayName string
	ActorSlug   string
	// ActorID names this model's ActorOpenWebUIModel row. It is fixed at
	// creation: re-registering the same ExternalModelID reuses the actor
	// rather than minting a second one, so entries already authored by
	// it keep resolving.
	ActorID string
	// Active excludes a model from projection and from any new use
	// without deleting the rows entries still reference.
	Active       bool
	Capabilities OpenWebUIModelCapabilities
	// ExternalUpdatedAt is the provider's own last-modified value when
	// it reports one. It is metadata: nothing local orders or expires by
	// it.
	ExternalUpdatedAt *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// VirtualActor is the Aria-facing projection of one model: the presented
// identity behind an ActorOpenWebUIModel row. It is assembled from a
// model and its workspace rather than stored, so a change to either
// shows up without a migration.
//
// It is a presentation actor, never a login-capable user — see
// ActorOpenWebUIModel and internal/miauth's exclusion tests.
type VirtualActor struct {
	ActorID     string
	Slug        string
	Host        string
	DisplayName string
	WorkspaceID string
	ModelID     string
}

// Handle returns the @slug@host form Aria displays. Host is a fixed
// presentation value, so this is a label, not an address: nothing
// resolves, discovers, or delivers to it.
func (v VirtualActor) Handle() string { return "@" + v.Slug + "@" + v.Host }

// LinkState is a conversation link's persisted state. The roadmap's
// state machine has one further state, `unlinked`, which is deliberately
// not a constant here: it means no link row exists for the branch at
// all. Claiming a branch is the INSERT that creates the row in
// LinkCreationPending, so "unlinked" is never a value anything reads,
// writes, or transitions out of.
type LinkState string

const (
	// LinkCreationPending is a claimed branch whose single initial
	// StartChat has been assigned to one durable job and has not yet
	// produced a definitive result. It is not retryable or re-claimable:
	// the claim is the authorization, and it is spent once.
	LinkCreationPending LinkState = "creation_pending"
	// LinkReady is a branch with a confirmed, persisted remote chat. It
	// is the only state a continuation may be sent from.
	LinkReady LinkState = "ready"
	// LinkAmbiguous is a branch whose remote outcome is unknown — a lost
	// or uncertain response, or a lease that expired before a definitive
	// result. It is frozen: recovery is an explicit owner action, never
	// another automatic provider call, because retrying could silently
	// create a second remote chat.
	LinkAmbiguous LinkState = "ambiguous"
	// LinkFailed is a definitive creation failure. The remote outcome is
	// known: no chat exists.
	LinkFailed LinkState = "failed"
	// LinkDead is a terminal owner decision to abandon the branch.
	LinkDead LinkState = "dead"
)

// LinkEvent names one thing that can happen to a conversation link.
// Events are distinguished by their authority as well as their outcome:
// LinkEventConfirmed and LinkEventOwnerConfirmed both reach LinkReady,
// but only the second may be applied to a frozen ambiguous link, and
// only a person may cause it.
type LinkEvent string

const (
	// LinkEventConfirmed reports that the claimed StartChat created and
	// persisted a chat, with a stable remote chat id in hand.
	LinkEventConfirmed LinkEvent = "confirmed"
	// LinkEventDefinitiveFailure reports a creation failure whose remote
	// outcome is known: no chat was created.
	LinkEventDefinitiveFailure LinkEvent = "definitive_failure"
	// LinkEventUncertainCreation reports a lost or uncertain creation
	// response, including a lease that expired before any definitive
	// result.
	LinkEventUncertainCreation LinkEvent = "uncertain_creation"
	// LinkEventUncertainContinuation reports a continuation on an
	// already-ready link whose remote outcome is unknown. A definitive
	// turn failure is not this event: that leaves the link ready and
	// marks only the turn failed.
	LinkEventUncertainContinuation LinkEvent = "uncertain_continuation"
	// LinkEventOwnerConfirmed is an explicit owner recovery action that
	// verified the provider outcome and identified the same chat.
	LinkEventOwnerConfirmed LinkEvent = "owner_confirmed"
	// LinkEventOwnerAbandoned is an explicit owner decision to abandon
	// the branch rather than adopt an uncertain remote chat.
	LinkEventOwnerAbandoned LinkEvent = "owner_abandoned"
)

// linkTransitions is the whole state machine, written out so that
// "allowed" is a lookup rather than a chain of conditions. Every pair
// absent from it is rejected; see docs/roadmap/openwebui.md's diagram,
// which this mirrors exactly.
var linkTransitions = map[LinkState]map[LinkEvent]LinkState{
	LinkCreationPending: {
		LinkEventConfirmed:         LinkReady,
		LinkEventDefinitiveFailure: LinkFailed,
		LinkEventUncertainCreation: LinkAmbiguous,
	},
	LinkReady: {
		LinkEventUncertainContinuation: LinkAmbiguous,
	},
	LinkAmbiguous: {
		LinkEventOwnerConfirmed: LinkReady,
		LinkEventOwnerAbandoned: LinkDead,
	},
}

// LinkTransition returns the state reached by applying ev to from, or
// ErrInvalidLinkTransition if the machine does not allow that pair. It
// is a pure function: callers decide the event, this decides whether it
// is legal, and the repository's compare-and-set writes enforce the same
// rule again against concurrent writers.
func LinkTransition(from LinkState, ev LinkEvent) (LinkState, error) {
	to, ok := linkTransitions[from][ev]
	if !ok {
		return "", ErrInvalidLinkTransition
	}
	return to, nil
}

// OpenWebUIConversationLink binds one local branch — a (ThreadID,
// BranchID) pair — to at most one remote chat (ADR-0005 D5). A reply to
// an earlier node starts a new branch with its own link, rather than
// reusing the remote chat with a second parent chain.
type OpenWebUIConversationLink struct {
	ID string
	// ThreadID and BranchID are the local identity of the branch.
	// BranchID is a locally minted opaque ID, not anything the provider
	// supplied.
	ThreadID string
	BranchID string
	// WorkspaceID is how a thread maps to a workspace: there is no
	// column on threads for it. ModelID is the workspace's default model
	// at claim time, recorded so a later default change cannot rewrite
	// which model a branch was started with.
	WorkspaceID string
	ModelID     string
	State       LinkState
	// ClaimJobID names the one durable job that holds this link's single
	// initial StartChat authorization. AllowsInitialStartChat compares
	// against it: no other job, and no later job, may make that call.
	ClaimJobID *string
	// RemoteChatID and RemoteCurrentID are opaque provider correlation
	// values, nil until confirmed. RemoteCurrentID is recorded only
	// after the provider reports the turn done (ADR-0005 D3): the server
	// advances it onto failed messages too, so recording it
	// unconditionally would parent the next turn on an error node.
	RemoteChatID    *string
	RemoteCurrentID *string
	// FailureCategory is a local classification (auth_failed,
	// contract_failed, ...), never provider error text. ADR-0005 D6
	// requires that text be discarded: the observed instance echoes
	// upstream credentials verbatim into it.
	FailureCategory  *string
	ClaimedAt        time.Time
	ReadyAt          *time.Time
	LastTransitionAt time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// AllowsInitialStartChat reports whether jobID may issue this link's one
// initial StartChat. Only the job that claimed the branch may, and only
// while the link is still pending: the claim is a single-use
// authorization, so a second job, a later job, or the same job after any
// transition all get false. This is the local guard that stops a retry
// from silently creating a second remote chat.
func (l OpenWebUIConversationLink) AllowsInitialStartChat(jobID string) bool {
	if l.State != LinkCreationPending || l.ClaimJobID == nil {
		return false
	}
	return *l.ClaimJobID == jobID
}

// AllowsContinue reports whether a continuation may be sent for this
// link. Only a ready link — confirmed chat persistence and a stable
// remote chat id — may receive one.
func (l OpenWebUIConversationLink) AllowsContinue() bool { return l.State == LinkReady }

// AllowsAutoRetry reports whether the link itself may be retried
// automatically. No state permits it. A pending link's call is
// single-use, an ambiguous link is frozen until a person resolves it,
// and failed and dead are terminal. Whether an individual continuation
// on a ready link may be retried is a decision about that turn, not
// about the link, and is made by Issue #53's turn handling.
func (l OpenWebUIConversationLink) AllowsAutoRetry() bool { return false }

// IsTerminal reports whether the link can no longer change state.
func (l OpenWebUIConversationLink) IsTerminal() bool {
	return l.State == LinkFailed || l.State == LinkDead
}

// TurnProviderStatus is one turn's outcome. Unlike LinkState it is a
// record rather than a machine: the categories mirror ADR-0005 D6's
// observation-to-outcome table, which classifies from the chat's stored
// state rather than from an HTTP status.
type TurnProviderStatus string

const (
	// TurnPending is a turn that has been recorded locally and not yet
	// resolved.
	TurnPending TurnProviderStatus = "pending"
	// TurnSucceeded is a turn the provider confirmed done, with content.
	TurnSucceeded TurnProviderStatus = "succeeded"
	// TurnFailed is a definitive failure whose remote outcome is known.
	// It does not by itself disturb a ready link.
	TurnFailed TurnProviderStatus = "failed"
	// TurnAmbiguous is a turn whose remote outcome is unknown.
	TurnAmbiguous TurnProviderStatus = "ambiguous"
	// TurnContractFailed is a response this service could not decode as
	// the pinned contract — the target changed shape, so nothing about
	// the outcome may be assumed.
	TurnContractFailed TurnProviderStatus = "contract_failed"
	// TurnAuthFailed is a rejected credential. It is never retried
	// blindly.
	TurnAuthFailed TurnProviderStatus = "auth_failed"
	// TurnCancelled is a turn abandoned locally before a result.
	TurnCancelled TurnProviderStatus = "cancelled"
)

// OpenWebUITurnLink records one owner message and the assistant reply it
// asked for, plus the opaque provider correlation values for both.
//
// Its identity is entirely local: LocalMessageID with Revision is the
// logical turn key, RequestID is the local correlation key, and the
// Remote* fields are nullable metadata that no lookup, ordering, or
// authorization decision may consult.
type OpenWebUITurnLink struct {
	ID     string
	LinkID string
	// BranchID repeats the link's branch so a turn can be read without
	// its link when only the branch matters.
	BranchID string
	// LocalMessageID is the owner's entry; LocalParentID is that entry's
	// reply_to_id, nil at the root. Aria's reply_to_id is the only
	// source of truth for parentage (ADR-0005 D2), so these mirror the
	// entries table rather than deriving anything from remote parent
	// ids.
	LocalMessageID string
	LocalParentID  *string
	// AssistantEntryID is the VirtualActor-authored reply entry, set
	// once the turn completes.
	AssistantEntryID *string
	// RequestID is a locally generated correlation key, unique across
	// all turns. The provider has no idempotency mechanism (ADR-0005
	// D7), so single-flight is entirely this value's job.
	RequestID string
	// Revision counts re-asks of the same local message; Attempt counts
	// provider attempts within one revision.
	Revision int
	Attempt  int
	Status   TurnProviderStatus
	// The Remote* fields are opaque provider correlation values, all
	// nullable. RemoteMessageID and RemoteAssistantMessageID are the
	// client-generated UUIDs from ADR-0005 D3, which the server stores
	// verbatim; the completion response's own chatcmpl id belongs to the
	// upstream model provider and is deliberately not recorded.
	RemoteChatID             *string
	RemoteMessageID          *string
	RemoteAssistantMessageID *string
	RemoteParentID           *string
	RemoteCurrentID          *string
	// TombstonedAt marks a turn superseded by a later revision. The
	// entries themselves are unaffected: hiding a note is ADR-0004's
	// separate concern.
	TombstonedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// OpenWebUITurnCorrelation carries the opaque provider ids recorded for
// one turn. It is a parameter object rather than five positional
// arguments because every field is a nullable opaque string, which is
// exactly the shape an argument list silently gets wrong.
type OpenWebUITurnCorrelation struct {
	RemoteChatID             *string
	RemoteMessageID          *string
	RemoteAssistantMessageID *string
	RemoteParentID           *string
	RemoteCurrentID          *string
}

// OpenWebUIWorkspaceRepository persists Open WebUI workspace registry
// rows.
type OpenWebUIWorkspaceRepository interface {
	Create(ctx context.Context, w OpenWebUIWorkspace) error
	Get(ctx context.Context, id string) (OpenWebUIWorkspace, error)
	// GetByBaseURL finds a workspace by its unique base URL, which is
	// how startup reconciliation recognises an already-registered
	// instance without inventing a second row for it.
	GetByBaseURL(ctx context.Context, baseURL string) (OpenWebUIWorkspace, error)
	// GetEnabled returns the single enabled workspace. It returns
	// ErrNotFound when none is enabled — the ordinary state with the
	// feature flag off — and an error if more than one is, which is a
	// broken invariant rather than a case to pick a winner from.
	GetEnabled(ctx context.Context) (OpenWebUIWorkspace, error)
	// Update writes the mutable registry fields: name, base URL, secret
	// ref, presentation host, and both capability statuses. ID,
	// timestamps, and the enable flags are not touched; SetEnabled and
	// SetDefaultModel own those.
	Update(ctx context.Context, w OpenWebUIWorkspace) error
	// SetDefaultModel points the workspace at one of its own models. The
	// same-workspace rule is a composite foreign key in the database, so
	// a model from another workspace is rejected by the write itself.
	SetDefaultModel(ctx context.Context, workspaceID, modelID string, at time.Time) error
	// SetEnabled flips the workspace-wide gate. Callers that also need
	// the workspace's models deactivated run both writes inside one
	// UnitOfWork transaction, which is the roadmap's "workspace disable
	// and related actor behavior must be one transaction".
	SetEnabled(ctx context.Context, workspaceID string, enabled bool, at time.Time) error
	// SetCapabilityStatus records evidence about one provider operation.
	SetCapabilityStatus(ctx context.Context, workspaceID string, chatCreate, chatContinue CapabilityStatus, at time.Time) error
}

// OpenWebUIModelRepository persists Open WebUI model registry rows.
type OpenWebUIModelRepository interface {
	Create(ctx context.Context, m OpenWebUIModel) error
	Get(ctx context.Context, id string) (OpenWebUIModel, error)
	// GetByActor resolves the model behind an ActorOpenWebUIModel row,
	// which is what the Aria projection needs to turn an author actor
	// into a VirtualActor.
	GetByActor(ctx context.Context, actorID string) (OpenWebUIModel, error)
	// GetByExternalID finds a model by the provider's own opaque id
	// within one workspace.
	GetByExternalID(ctx context.Context, workspaceID, externalModelID string) (OpenWebUIModel, error)
	// ListByWorkspace returns a workspace's models in a stable
	// (created_at, id) order.
	ListByWorkspace(ctx context.Context, workspaceID string) ([]OpenWebUIModel, error)
	// Update writes only the mutable fields: display name, actor slug,
	// capabilities, and the provider's external timestamp. WorkspaceID,
	// ExternalModelID and ActorID are immutable — the roadmap's stable
	// actor ID requirement is that a rename cannot move a model onto a
	// different actor row.
	Update(ctx context.Context, m OpenWebUIModel) error
	SetActive(ctx context.Context, modelID string, active bool, at time.Time) error
}

// OpenWebUIConversationLinkRepository persists branch-to-remote-chat
// links. Its four Mark* methods are compare-and-set writes whose WHERE
// clause encodes the state machine's allowed sources, so an illegal
// transition returns ErrConflict even if two callers race; LinkTransition
// answers the same question ahead of time, without a write.
type OpenWebUIConversationLinkRepository interface {
	// Claim inserts a creation_pending link for a branch. The unique
	// (thread_id, branch_id) constraint makes it the atomic claim: a
	// second attempt on the same branch returns ErrConflict rather than
	// producing a second link that could create a second remote chat.
	Claim(ctx context.Context, l OpenWebUIConversationLink) error
	Get(ctx context.Context, id string) (OpenWebUIConversationLink, error)
	GetByThreadBranch(ctx context.Context, threadID, branchID string) (OpenWebUIConversationLink, error)
	// GetByRemoteChat resolves a link from a provider chat id. It exists
	// for owner-driven recovery of an ambiguous link, the one workflow
	// that legitimately starts from a remote value; it is not a lookup
	// any request path uses.
	GetByRemoteChat(ctx context.Context, workspaceID, remoteChatID string) (OpenWebUIConversationLink, error)
	// ListByThread returns a thread's links in a stable (created_at, id)
	// order.
	ListByThread(ctx context.Context, threadID string) ([]OpenWebUIConversationLink, error)
	// MarkReady records a confirmed remote chat. It applies to a
	// creation_pending link (LinkEventConfirmed) or an ambiguous one an
	// owner has resolved (LinkEventOwnerConfirmed), and returns
	// ErrConflict from any other state.
	MarkReady(ctx context.Context, id, remoteChatID string, remoteCurrentID *string, at time.Time) error
	// MarkAmbiguous freezes a link whose remote outcome is unknown, from
	// either creation_pending or ready.
	MarkAmbiguous(ctx context.Context, id string, at time.Time) error
	// MarkFailed records a definitive creation failure. Only a
	// creation_pending link can reach it: after a chat exists, a failure
	// is the turn's, not the link's.
	MarkFailed(ctx context.Context, id, failureCategory string, at time.Time) error
	// MarkDead records an owner's decision to abandon an ambiguous
	// branch. Only an ambiguous link can reach it; nothing automatic
	// ever does.
	MarkDead(ctx context.Context, id, failureCategory string, at time.Time) error
	// SetRemoteCurrent updates a ready link's opaque current-message
	// pointer. Callers record it only once the provider reports the turn
	// done (ADR-0005 D3).
	SetRemoteCurrent(ctx context.Context, id string, remoteCurrentID *string, at time.Time) error
}

// OpenWebUITurnLinkRepository persists per-turn correlation rows.
type OpenWebUITurnLinkRepository interface {
	// Create inserts a turn. The unique (link_id, local_message_id,
	// revision) constraint is the logical turn key: re-recording the
	// same revision of the same message returns ErrConflict instead of
	// producing a second turn for it.
	Create(ctx context.Context, t OpenWebUITurnLink) error
	Get(ctx context.Context, id string) (OpenWebUITurnLink, error)
	// GetByRequestID resolves a turn from its local correlation key.
	GetByRequestID(ctx context.Context, requestID string) (OpenWebUITurnLink, error)
	// ListByLink returns a link's turns in a stable (created_at, id)
	// order — never in any order derived from a remote id.
	ListByLink(ctx context.Context, linkID string) ([]OpenWebUITurnLink, error)
	// SetRemoteCorrelation records the opaque provider ids for a turn.
	SetRemoteCorrelation(ctx context.Context, id string, corr OpenWebUITurnCorrelation, at time.Time) error
	SetProviderStatus(ctx context.Context, id string, status TurnProviderStatus, attempt int, at time.Time) error
	// SetAssistantEntry attaches the VirtualActor-authored reply entry
	// once the turn has produced one.
	SetAssistantEntry(ctx context.Context, id, assistantEntryID string, at time.Time) error
	// Tombstone marks a turn superseded by a later revision. It does not
	// delete the row: the correlation is still the record of what was
	// sent.
	Tombstone(ctx context.Context, id string, at time.Time) error
}
