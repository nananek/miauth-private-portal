// This file is Issue #53's (OWUI-B) outbound provider port: the shape
// internal/provider/openwebui's HTTP adapter implements against a real
// Open WebUI instance, and the shape a future durable job handler
// (Issue #53's later PRs) and its tests call. Like internal/llmreply's
// Provider, it depends on nothing but the standard library — no
// net/http, no domain, no endpoint name — so a fake can stand in for
// tests without pulling in any of that.
//
// ADR-0005 D1 keeps the port's two operations, StartChat and
// ContinueTurn, and its 2026-09-06 addendum for Issue #53 adds the two
// members below that the bridge could not be built without:
// OnChatCreated (a callback StartChat's implementation invokes once the
// chat id is confirmed, before any generation is requested) and
// LookupTurnOutcome (the "result lookup" a durable job needs before it
// may ever retry an uncertain continuation — see ADR-0005 D7's
// addendum). Neither promotes an Open WebUI endpoint name into this
// port: an adapter for a different chat-persistence backend could
// implement Provider without either concept mapping to a literal
// endpoint.
package openwebui

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// Message is one chat-style turn in the sequence a Provider call sends.
// Role is "user" or "assistant" — never "system": ADR-0005 D4 requires
// the locally assembled sequence to carry no system prompt, since Open
// WebUI forwards it to the upstream model exactly as sent, with nothing
// added or substituted.
type Message struct {
	Role    string
	Content string
}

// TurnIDs are the client-generated correlation ids for one turn: the
// user message id, the assistant message id, and — for a continuation
// — the previous assistant message id this turn attaches to as its
// remote parent. Compat's "Opaque ID semantics" is what makes this
// possible: because these ids are minted locally rather than returned
// by the provider, a caller can persist them before ever making the
// call, so a lost response never leaves the local side without an id
// to reconcile against.
type TurnIDs struct {
	UserMessageID      string
	AssistantMessageID string
	ParentAssistantID  *string
}

// StartChatRequest is the initial turn on a brand-new remote chat.
type StartChatRequest struct {
	ModelID string
	// Messages is the root-to-parent context this turn continues, in
	// order; NewTurn is the message being asked now. Neither carries a
	// local entry id, a Misskey field, or a system prompt (ADR-0005
	// D2, D4).
	Messages []Message
	NewTurn  Message
	IDs      TurnIDs
	// ToolIDs is sent verbatim as tool_ids on this one call — Issue #75
	// resolves it per model (the model's own info.meta.toolIds, GET
	// /api/models, filtered against what this credential may actually
	// invoke), unlike Issue #72's original single deployment-wide
	// OPENWEBUI_TOOL_IDS override, which Issue #75 retires. Nil/empty
	// sends no "tool_ids" key at all.
	ToolIDs []string
	// WebSearchEnabled sets features.web_search on this one call.
	// TurnJob.resolveWebSearchEnabled computes it per model (ADR-0005
	// D21, Issue #75 AC#11): an explicit OPENWEBUI_WEB_SEARCH_ENABLED
	// overrides every model uniformly; left unset, it follows the
	// selected model's own most recently synced defaultFeatureIds
	// instead. Moved here from a Client-construction-time Config field
	// (Issue #72's original shape) the same way ToolIDs already moved
	// per-call under Issue #75.
	WebSearchEnabled bool
	// EnableTitleGeneration sets background_tasks.title_generation on
	// this chat's first completions call (Issue #84, ADR-0005 D23).
	// TurnJob sets it from whether OPENWEBUI_VIEWER_BASE_URL is
	// configured — the title and the viewer link are always enabled or
	// disabled together (see TurnJobConfig.ViewerBaseURL), so there is
	// no separate config key for this alone.
	//
	// UNVERIFIED (no real-instance access — see ADR-0005 D23's own note):
	// whether Open WebUI resolves title generation synchronously (the
	// title is already updated by the time this same turn's outcome
	// lookup runs) or asynchronously (visible only on some later turn, if
	// ever) was never confirmed against a real instance. This field only
	// asks for it; TurnOutcome.Title reports whatever, if anything, the
	// very next lookup actually observes — a title that arrives late
	// simply is not captured for this reply, which degrades the feature
	// to "no title shown," never to an incorrect one and never to a
	// failed turn.
	EnableTitleGeneration bool
	// CorrelationID is a local request id for logging only. It is never
	// sent to the provider and is not a provider idempotency key — Open
	// WebUI has none (ADR-0005 D7).
	CorrelationID string
	SentAt        time.Time
	// OnChatCreated is invoked once chat creation has returned a
	// server-assigned chat id and before the first completion is
	// requested (ADR-0005 D1's addendum). A caller uses it to persist
	// the id durably at the earliest possible point. A non-nil error
	// aborts the call before any generation is attempted — returned
	// as-is, not wrapped in a *ProviderError, so a caller can tell "the
	// hook itself failed" (its own concern to classify and retry) apart
	// from a provider-classified failure.
	OnChatCreated func(ctx context.Context, remoteChatID string) error
}

// ContinueTurnRequest is a turn on an already-confirmed remote chat.
// IDs.ParentAssistantID must name the previous assistant message:
// ADR-0005 D4 requires both the request's own parent id and the new
// user message's parent id to name it, or the turn lands as a
// disconnected pair instead of extending the chain.
type ContinueTurnRequest struct {
	RemoteChatID  string
	ModelID       string
	Messages      []Message
	NewTurn       Message
	IDs           TurnIDs
	CorrelationID string
	SentAt        time.Time
	// ToolIDs mirrors StartChatRequest.ToolIDs — see its doc comment.
	ToolIDs []string
	// WebSearchEnabled mirrors StartChatRequest.WebSearchEnabled — see
	// its doc comment.
	WebSearchEnabled bool
}

// SourceKind names the two sources[] shapes Issue #81's real-instance
// check (2026-09-07) found — see Source's own doc comment.
const (
	SourceKindTool      = "tool"
	SourceKindWebSearch = "web_search"
)

// Source is this port's own normalized shape of one Issue #81 sources[]
// entry, decoded from a completions response — never document[]'s raw
// tool/page text, which ADR-0005 D22 requires never be stored, logged,
// or forwarded to Aria at all. Only a short display name, an optional
// URL, and (for a tool call) its arguments survive.
//
// UNVERIFIED ASSUMPTION (2026-09-08, no real-instance access — see
// ADR-0005 D22): the real-instance check behind this issue observed
// exactly one sources[] entry. This adapter assumes each array element
// maps 1:1 to one Source here, in array order, matching the "[n]"
// citation markers the model's own answer text embeds — the only
// mapping actually exercised. If a real multi-source response instead
// nests several results under one sources[] element (for example
// multiple web-search result chunks grouped under a single entry, rather
// than one entry per chunk), this normalization under-represents it:
// only that group's first URL/description is kept, and a footnote's
// number could disagree with the "[n]" markers the reply text actually
// shows. That failure mode is cosmetic only — a wrong or missing
// footnote — never a data leak (document[] is never captured regardless
// of this assumption) and never a turn failure (a sources-parsing
// anomaly never fails the turn; see internal/provider/openwebui/
// client.go's runTurn). Re-verify against a real multi-source turn
// before removing this note.
type Source struct {
	// Kind is SourceKindTool or SourceKindWebSearch.
	Kind        string
	DisplayName string
	URL         *string
	Arguments   map[string]string
}

// TurnResult is a turn's confirmed successful outcome. RemoteCurrentID,
// PromptTokens, CompletionTokens, and FinishReason are accounting and
// correlation metadata only — nothing about them is authoritative for
// anything but bookkeeping.
type TurnResult struct {
	Content          string
	RemoteCurrentID  *string
	PromptTokens     *int
	CompletionTokens *int
	FinishReason     *string
	// Title is the chat's own generated title, observed on the same GET
	// /api/v1/chats/{id} lookup runTurn already makes for confirmation —
	// nil unless EnableTitleGeneration was set on this chat's first turn
	// and the placeholder title had already been replaced by the time of
	// that lookup (Issue #84; see EnableTitleGeneration's own doc
	// comment on the synchronous/asynchronous uncertainty this implies).
	Title *string
	// Sources is Issue #81's normalized citation list for this turn,
	// nil when the completions response carried none.
	Sources []Source
}

// TurnOutcome is what LookupTurnOutcome reads back about one assistant
// message: whether it exists, whether it is done, whether it carries an
// error, its content once done, and the chat's current-message pointer.
// Found false means the assistant message this turn asked for is not
// (yet, or ever) present in the chat's history — distinct from the
// chat lookup itself failing, which LookupTurnOutcome reports as an
// error instead.
type TurnOutcome struct {
	Found            bool
	Done             bool
	HasError         bool
	Content          string
	RemoteCurrentID  *string
	PromptTokens     *int
	CompletionTokens *int
	// Title mirrors TurnResult.Title (Issue #84): the chat's own title,
	// read from the same GET /api/v1/chats/{id} body this lookup already
	// decodes, once it has moved past the "bridge-precreated"
	// placeholder. Unlike Sources, a lookup can report this for a
	// continuation or a retried turn too, since the chat title is
	// chat-wide state, not turn-specific.
	//
	// There is deliberately no Sources field here: D22 records that
	// sources[] is captured only from the completions response body
	// itself, never re-derived from this GET path, because whether GET
	// /api/v1/chats/{id} even carries sources for a completed turn was
	// never confirmed. A turn recovered through this lookup path (Issue
	// #53's uncertain-outcome retry) therefore never gets sources
	// attached, even if the original completions call that produced it
	// would have. That gap is cosmetic (a possibly-missing footnote
	// list on an already-rare recovery path), not a correctness or
	// security concern.
	Title *string
}

// Provider is the outbound boundary a durable job handler (Issue #53's
// later PRs) uses to talk to a chat-persistence backend.
// internal/provider/openwebui implements it against a real Open WebUI
// instance; tests use a fake.
type Provider interface {
	// StartChat creates a new remote chat and runs its first turn.
	StartChat(ctx context.Context, req StartChatRequest) (TurnResult, error)
	// ContinueTurn runs a turn on an already-confirmed remote chat.
	ContinueTurn(ctx context.Context, req ContinueTurnRequest) (TurnResult, error)
	// LookupTurnOutcome reads back the one assistant message a prior
	// StartChat or ContinueTurn call named, and nothing else: not the
	// rest of the chat's history, not its title, not any other message.
	// It exists so an uncertain completion can be resolved without
	// resending it (ADR-0005 D7's addendum) — a durable job calls it
	// before ever retrying a continuation whose previous attempt's
	// outcome is unknown.
	LookupTurnOutcome(ctx context.Context, remoteChatID, assistantMessageID string) (TurnOutcome, error)
}

// Phase names which of Provider's three calls a ProviderError came
// from, so a caller (and a log line) can tell "the chat was never
// created" apart from "an established chat's turn failed" apart from
// "a result lookup itself failed" without parsing Error()'s text.
type Phase string

const (
	PhaseCreate Phase = "create"
	PhaseTurn   Phase = "turn"
	PhaseLookup Phase = "lookup"
)

// Category classifies a Provider failure. It is this port's own
// vocabulary — not domain.FailureCategory* — because a Provider call's
// classification and a turn's eventual recorded failure_category are
// different questions: a bounded-retry job handler decides the latter
// from this Category *and* which Phase produced it *and* how many
// attempts have already been made (ADR-0005 D7's asymmetry: a
// `creation_pending` StartChat is never automatically replayed, while
// a `ready` link's continuation may be retried within a bounded
// count), so the two vocabularies are deliberately not merged into
// one. CategoryAmbiguous
// has no domain.FailureCategory counterpart at all: it means "this call's
// outcome could not be determined", never a category recorded on its own.
type Category string

const (
	// CategoryAuthFailed is a rejected credential (401/403).
	CategoryAuthFailed Category = "auth_failed"
	// CategoryClientRejected is a definitive 4xx for the request itself
	// (not credentials) — an unknown model, a malformed request, or a
	// request this client refused to send because it exceeded
	// Config.MaxRequestBytes (see ErrRequestTooLarge).
	CategoryClientRejected Category = "client_rejected"
	// CategoryContractFailed is a response this adapter could not
	// decode as the pinned contract (docs/compat/openwebui-0.11.3.md) —
	// the target changed shape, so nothing about the outcome may be
	// assumed.
	CategoryContractFailed Category = "contract_failed"
	// CategoryTurnFailed is a chat lookup that found the assistant
	// message carrying an error object.
	CategoryTurnFailed Category = "turn_failed"
	// CategoryRateLimited is a 429 response.
	CategoryRateLimited Category = "rate_limited"
	// CategoryServerError is a 5xx response.
	CategoryServerError Category = "server_error"
	// CategoryTransport is a network-level failure below the HTTP
	// response layer (connection refused, reset, DNS, ...).
	CategoryTransport Category = "transport"
	// CategoryTimeout is a call that exceeded its configured deadline.
	CategoryTimeout Category = "timeout"
	// CategoryPolicyViolation is a request this client refused to send
	// or follow at all: a disallowed redirect, a non-allowlisted
	// origin, an SSRF-guarded address, or a chat id this client will
	// not embed in a request path.
	CategoryPolicyViolation Category = "policy_violation"
	// CategoryAmbiguous is a call whose outcome genuinely could not be
	// determined: a chat-creation or chat-lookup response that was
	// itself the literal JSON null the target's own schema documents as
	// a possible 200 body, or (from runTurn) a turn whose lookup found
	// the assistant message not yet done and not erroring either — for a
	// native (tool-carrying) turn, this is also what a poll budget
	// exhausted before the assistant message resolved reports (ADR-0005
	// D27's awaitTurnDone). It is never adopted as evidence of failure —
	// only of "unknown".
	CategoryAmbiguous Category = "ambiguous"
)

// ProviderError wraps a classified Provider failure. Error() renders a
// fixed "openwebui: <phase>: <category>" string — never the request
// body, a response body, a header, or a URL. This is not incidental
// caution: the observed Open WebUI instance echoes the upstream
// credential pseudo-secret verbatim into its own error text (ADR-0005
// D6), so anything from a response body reaching Error()'s return value
// would leak it into any log line or error response that prints an
// error's text.
type ProviderError struct {
	Category Category
	Phase    Phase
	err      error
}

// NewProviderError classifies err under category and phase. A nil err
// returns nil so a caller can write "return result, NewProviderError(...)"
// without manufacturing a failure when there wasn't one.
func NewProviderError(category Category, phase Phase, err error) error {
	if err == nil {
		return nil
	}
	return &ProviderError{Category: category, Phase: phase, err: err}
}

func (e *ProviderError) Error() string {
	return "openwebui: " + string(e.Phase) + ": " + string(e.Category)
}
func (e *ProviderError) Unwrap() error { return e.err }

// ErrRequestTooLarge is the sentinel wrapped inside a *ProviderError
// with CategoryClientRejected when an assembled request body exceeds
// Config.MaxRequestBytes. It exists as a distinct sentinel — rather
// than relying on Category alone — because a future durable job handler
// needs to tell this specific, request-shape-known-in-advance case
// apart from an ordinary provider-side rejection, so it can record
// domain.FailureCategoryRequestTooLarge instead of the generic
// client_rejected that Category alone would imply.
var ErrRequestTooLarge = errors.New("openwebui: request exceeds the configured size bound")

// newRemoteMessageID mints a client-generated correlation id in the
// same RFC 4122 version-4 form Open WebUI's own ids take
// (docs/compat/openwebui-0.11.3.md's "Opaque ID semantics": "client-
// generated UUIDs"). A caller mints one of these for each side of a
// turn (TurnIDs.UserMessageID, TurnIDs.AssistantMessageID) and persists
// them before making the Provider call that will use them, never
// after.
func newRemoteMessageID() string {
	buf := make([]byte, 16)
	// crypto/rand.Read never returns an error on Go 1.24+ (it crashes
	// the process instead — go.dev/issue/66821), so there is no error
	// path here to check.
	_, _ = rand.Read(buf)
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10 (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
