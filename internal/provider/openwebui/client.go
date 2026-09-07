// Package openwebui implements internal/openwebui.Provider against a
// real Open WebUI instance, per docs/compat/openwebui-0.11.3.md and
// ADR-0005. It is a narrow adapter behind a use-case-owned interface
// (AGENTS.md: "put provider boundaries behind narrow interfaces"), the
// same split internal/provider/openai keeps from internal/llmreply: the
// port names no endpoint, and only this package knows the three
// Open WebUI calls (POST /api/v1/chats/new, POST /api/chat/completions,
// GET /api/v1/chats/{id}) behind them.
package openwebui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

var _ openwebui.Provider = (*Client)(nil)

// remoteIDPattern is what a chat id must match before this client will
// embed it in a request path (LookupTurnOutcome). Open WebUI's own ids
// are UUIDs; this is deliberately a little looser than a strict UUID
// regexp — the point is not to validate the provider's id format, only
// to refuse anything that could act as a path separator or traversal
// segment (a chat id is opaque correlation data, never trusted input —
// ADR-0005 D2 — but it still ends up in a URL this client builds).
var remoteIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// Config builds a Client. BaseURL, AllowedOrigins, and APIKey come from
// internal/config.OpenWebUIConfig's already-validated fields; cmd/server
// is the only production caller and passes them through unmodified
// (ADR-0005 D10: this adapter reads the key from a typed Config, never
// os.Getenv).
type Config struct {
	// BaseURL is the workspace's configured origin — no path beyond the
	// endpoint paths this client appends.
	BaseURL string
	// AllowedOrigins must contain BaseURL exactly. This re-checks what
	// internal/config.Validate already enforces (defense in depth): a
	// Client is never constructed with a base URL its own configuration
	// does not allow.
	AllowedOrigins []string
	APIKey         string
	// Timeout bounds every individual HTTP call this client makes.
	Timeout time.Duration
	// MaxResponseBytes and MaxRequestBytes are this client's own
	// client-side bounds (ADR-0005 D11: no server-side limit could be
	// observed, so the adapter imposes one rather than relying on the
	// target's).
	MaxResponseBytes int64
	MaxRequestBytes  int64
	// AllowIPForTesting overrides the default public-unicast-only IP
	// policy so a test can point a Client at an httptest.Server. A
	// production deployment never sets it (mirrors
	// internal/ingest/safehttp.Config's field of the same name).
	AllowIPForTesting func(net.IP) bool
	// AllowInsecureHTTPForTesting permits an "http" BaseURL. A
	// production deployment never sets it: OPENWEBUI_BASE_URL is
	// validated as an HTTPS origin before a Client is ever built.
	AllowInsecureHTTPForTesting bool
}

// Client calls the three endpoints docs/compat/openwebui-0.11.3.md
// classifies: POST /api/v1/chats/new, POST /api/chat/completions,
// and GET /api/v1/chats/{id}.
type Client struct {
	baseURL          string
	apiKey           string
	timeout          time.Duration
	maxResponseBytes int64
	maxRequestBytes  int64
	httpClient       *safehttp.Client
}

// NewClient builds a Client against cfg. It errors if BaseURL is not a
// member of AllowedOrigins: every request this client ever makes starts
// from BaseURL, so that membership check is the one place the whole
// allowlist policy is enforced for this client's lifetime.
func NewClient(cfg Config) (*Client, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	allowed := false
	for _, origin := range cfg.AllowedOrigins {
		if origin == baseURL {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("openwebui: base URL %q is not in the configured allowed-origin list", baseURL)
	}

	return &Client{
		baseURL:          baseURL,
		apiKey:           cfg.APIKey,
		timeout:          cfg.Timeout,
		maxResponseBytes: cfg.MaxResponseBytes,
		maxRequestBytes:  cfg.MaxRequestBytes,
		httpClient: safehttp.NewClient(safehttp.Config{
			// ADR-0005 D11: redirects disabled, simpler than
			// revalidating every hop against the allowlist.
			MaxRedirects:      0,
			AllowInsecureHTTP: cfg.AllowInsecureHTTPForTesting,
			AllowIPForTesting: cfg.AllowIPForTesting,
		}),
	}, nil
}

// wireMessage is the shape of one entry in a completions request's
// "messages" array — the same {role, content} pair regardless of
// whether it comes from context history or the new turn.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// --- POST /api/v1/chats/new ---

// precreatedChatTitle is the fixed title createChat sends when it
// precreates a chat (ADR-0005 D3): the value LookupTurnOutcome must see
// move past before Issue #84's title feature treats a chat's own title
// as meaningful. Open WebUI is known (Issue #84's own write-up) to
// overwrite this with the raw first user message even without
// background_tasks.title_generation ever being requested, so this
// filter alone does not guarantee a *summarized* title — only that a
// placeholder this adapter itself invented is never shown as one.
const precreatedChatTitle = "bridge-precreated"

type chatCreateRequestBody struct {
	Chat chatCreateChatBody `json:"chat"`
}

type chatCreateChatBody struct {
	ID        string                `json:"id"`
	Title     string                `json:"title"`
	Models    []string              `json:"models"`
	Params    map[string]any        `json:"params"`
	History   chatCreateHistoryBody `json:"history"`
	Messages  []any                 `json:"messages"`
	Tags      []string              `json:"tags"`
	Timestamp int64                 `json:"timestamp"`
}

type chatCreateHistoryBody struct {
	Messages  map[string]any `json:"messages"`
	CurrentID *string        `json:"currentId"`
}

type chatCreateResponseBody struct {
	ID string `json:"id"`
}

// createChat calls POST /api/v1/chats/new with an empty history, per
// docs/compat/openwebui-0.11.3.md's ChatForm example
// (fixtures/openwebui/chats_new_empty_request.json): sending chat.id:""
// is what makes the server assign a fresh id, which is why the chat can
// be pre-created before any generation runs at all (ADR-0005 D3).
func (c *Client) createChat(ctx context.Context, modelID string, sentAt time.Time) (string, error) {
	reqBody := chatCreateRequestBody{Chat: chatCreateChatBody{
		ID:        "",
		Title:     precreatedChatTitle,
		Models:    []string{modelID},
		Params:    map[string]any{},
		History:   chatCreateHistoryBody{Messages: map[string]any{}, CurrentID: nil},
		Messages:  []any{},
		Tags:      []string{},
		Timestamp: sentAt.UnixMilli(),
	}}

	data, err := c.post(ctx, openwebui.PhaseCreate, "/api/v1/chats/new", reqBody)
	if err != nil {
		return "", err
	}

	// The declared 200 schema is ChatResponse | null (compat: "a null
	// body is a documented possibility on this endpoint"); a null body
	// means the chat may or may not have been created and there is no
	// id to check by — ADR-0005 D7's rule against ever re-creating a
	// chat is exactly why this case is reported as unknown rather than
	// as a definite failure.
	if isJSONNull(data) {
		return "", openwebui.NewProviderError(openwebui.CategoryAmbiguous, openwebui.PhaseCreate,
			errors.New("chat creation returned null"))
	}

	var parsed chatCreateResponseBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", openwebui.NewProviderError(openwebui.CategoryContractFailed, openwebui.PhaseCreate,
			fmt.Errorf("decode chat creation response: %w", err))
	}
	if parsed.ID == "" {
		return "", openwebui.NewProviderError(openwebui.CategoryContractFailed, openwebui.PhaseCreate,
			errors.New("chat creation response has no id"))
	}
	return parsed.ID, nil
}

// --- POST /api/chat/completions ---

type completionsRequestBody struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
	ChatID string `json:"chat_id"`
	// ParentID has no "omitempty": ADR-0005 D4 requires the parent_id
	// *key* to always be present, even when its value is null.
	// Omitting it while still naming chat_id and an existing
	// message id is not a safe "generate only" call — it overwrites
	// that message's content and clears its parent link.
	ParentID        *string             `json:"parent_id"`
	ID              string              `json:"id"`
	UserMessage     userMessageBody     `json:"user_message"`
	Messages        []wireMessage       `json:"messages"`
	BackgroundTasks backgroundTasksBody `json:"background_tasks"`
	// Features and ToolIDs are Issue #72's opt-in: both omitted (nil/
	// empty) by default, which is byte-for-byte the same request shape
	// this client already sent — Open WebUI's own `features.pop(...) or
	// {}` and `tool_ids = form_data.pop('tool_ids', None)` treat an
	// absent key exactly like an empty/false one, so there is no
	// behavior change until an operator opts in via config.
	Features *featuresBody `json:"features,omitempty"`
	ToolIDs  []string      `json:"tool_ids,omitempty"`
	// Params is Issue #74's fix: set only when this call also carries
	// Features or ToolIDs (never on a plain turn — no behavior change
	// for a caller that uses neither). Open WebUI's non_streaming_chat_
	// response_handler never processes a native tool_calls response (it
	// only reads choices[0].message.content), so a buffered stream:false
	// call whose model decides to call a tool natively hangs the
	// assistant message at done:false forever. function_calling=legacy
	// routes web search and tool execution through a pre-completion
	// injection step instead (Open WebUI's own internal task calls),
	// which the buffered handler processes normally — see ADR-0005's
	// tool/web-search addendum.
	Params *paramsBody `json:"params,omitempty"`
}

// paramsBody is the subset of Open WebUI's per-request `params` object
// this adapter sets: only FunctionCalling, only the literal "legacy"
// value, only when Issue #72's opt-in tool/web-search flags are in use.
type paramsBody struct {
	FunctionCalling string `json:"function_calling"`
}

// featuresBody is the subset of Open WebUI's `features` request object
// this adapter sets. Open WebUI recognizes several more keys (voice,
// memory, image_generation, code_interpreter) — Issue #72's scope is
// web_search only; add more only when a future issue's own request
// traces the same client-caller behavior for one of them.
type featuresBody struct {
	WebSearch bool `json:"web_search"`
}

type userMessageBody struct {
	ID          string   `json:"id"`
	ParentID    *string  `json:"parentId"`
	ChildrenIDs []string `json:"childrenIds"`
	Role        string   `json:"role"`
	Content     string   `json:"content"`
	Timestamp   int64    `json:"timestamp"`
}

type backgroundTasksBody struct {
	TitleGeneration    bool `json:"title_generation"`
	TagsGeneration     bool `json:"tags_generation"`
	FollowUpGeneration bool `json:"follow_up_generation"`
}

type completionsResponseBody struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Sources is Issue #81's addition: present whenever a tool call or
	// web search actually ran (function_calling=legacy's own injection
	// step — see paramsBody's doc comment). Left as json.RawMessage here
	// — decoded into []wireSource separately, by decodeSources — so that
	// an unexpected sources[] shape (a provider schema drift this
	// adapter has not observed) degrades to "no citations for this
	// reply" rather than failing the whole completions decode and, with
	// it, a turn that otherwise succeeded: sources is enrichment, never
	// load-bearing for turn success. There is deliberately no field
	// anywhere in this decode path (typed or otherwise) for a source's
	// own document[] member — ADR-0005 D22 requires that raw,
	// potentially large tool/page text never be decoded into memory by
	// this adapter at all, and omitting the field entirely from
	// wireSource is what makes encoding/json's default
	// unknown-field-skipping behavior enforce that for free.
	Sources json.RawMessage `json:"sources"`
}

// decodeSources decodes raw (completionsResponseBody.Sources) into
// wireSource entries, or nil — never an error — if raw is empty or does
// not match the expected shape: a malformed/unknown sources[] entry must
// never fail a turn that otherwise completed successfully (see Sources'
// own doc comment). ok is false only to let a caller that wants to know
// decide whether to log the anomaly; it is never treated as this
// function's own failure.
func decodeSources(raw json.RawMessage) (sources []wireSource, ok bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if err := json.Unmarshal(raw, &sources); err != nil {
		return nil, false
	}
	return sources, true
}

// wireSource is one raw sources[] entry (Issue #81's real-instance
// check, 2026-09-07): ToolResult true distinguishes a tool-execution
// source (source.name, metadata[].parameters as the call's arguments)
// from a web-search one (source.{name,type,urls,queries}, metadata[].
// {title,description,source} per result chunk). See normalizeSources
// for how this collapses to one openwebui.Source, and openwebui.Source's
// own doc comment for the multi-source ordering assumption this adapter
// makes without real-instance confirmation.
type wireSource struct {
	Source struct {
		Name string `json:"name"`
	} `json:"source"`
	ToolResult bool                 `json:"tool_result"`
	Metadata   []wireSourceMetadata `json:"metadata"`
}

// wireSourceMetadata is one metadata[] entry. Parameters (tool call
// arguments) and Source (a web-search result chunk's own URL) are the
// only members normalizeSources reads; Title/Description exist here only
// so a caller inspecting the raw fixture shape can see what this adapter
// deliberately leaves unused rather than persisting a page's summary
// text.
type wireSourceMetadata struct {
	Parameters map[string]any `json:"parameters"`
	Source     string         `json:"source"`
}

// maxSourceFieldLen bounds every string this adapter copies out of a
// sources[] entry into an openwebui.Source, the same defense-in-depth a
// provider display name already gets (ADR-0005 D9): this is untrusted
// provider/tool output, never assumed to be reasonably sized.
const maxSourceFieldLen = 200

func boundedString(s string) string {
	if len(s) <= maxSourceFieldLen {
		return s
	}
	return s[:maxSourceFieldLen]
}

// normalizeSources turns raw sources[] entries into openwebui.Source
// records, one per array element, in order — see openwebui.Source's own
// doc comment for why that 1:1 mapping is an unverified assumption for
// more than one entry. document[] is never read at all (wireSource has
// no field for it); a tool-execution source's Arguments come from its
// first metadata entry's parameters, a web-search source's URL from its
// first metadata entry's source field — later metadata entries (further
// result chunks) are not represented, the concrete shape of this
// function's own under-representation risk.
func normalizeSources(raw []wireSource) []openwebui.Source {
	if len(raw) == 0 {
		return nil
	}
	sources := make([]openwebui.Source, 0, len(raw))
	for _, s := range raw {
		kind := openwebui.SourceKindWebSearch
		if s.ToolResult {
			kind = openwebui.SourceKindTool
		}
		src := openwebui.Source{Kind: kind, DisplayName: boundedString(s.Source.Name)}
		if len(s.Metadata) > 0 {
			m := s.Metadata[0]
			if s.ToolResult {
				src.Arguments = stringifyArguments(m.Parameters)
			} else if m.Source != "" {
				url := boundedString(m.Source)
				src.URL = &url
			}
		}
		sources = append(sources, src)
	}
	return sources
}

// stringifyArguments renders a tool call's raw JSON arguments as
// display strings, bounded the same way every other source field is:
// this is untrusted tool-controlled input reaching an owner-facing
// reply and a log line (ADR-0005 D22), never re-parsed as JSON by
// anything downstream.
func stringifyArguments(params map[string]any) map[string]string {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[boundedString(k)] = boundedString(fmt.Sprint(v))
	}
	return out
}

// runTurn sends one completions call and then confirms its outcome with
// LookupTurnOutcome — ADR-0005 D6's rule that a chat-managed turn's
// result is never trusted from the completions response alone, only
// from what GET /api/v1/chats/{id} reports. StartChat and ContinueTurn
// both funnel through this: the only difference between them is
// parentID (nil for the former, the previous assistant message id for
// the latter).
func (c *Client) runTurn(ctx context.Context, remoteChatID string, parentID *string, modelID string, messages []openwebui.Message, newTurn openwebui.Message, ids openwebui.TurnIDs, sentAt time.Time, toolIDs []string, webSearchEnabled, enableTitleGeneration bool) (openwebui.TurnResult, error) {
	wireMessages := make([]wireMessage, 0, len(messages)+1)
	for _, m := range messages {
		wireMessages = append(wireMessages, wireMessage{Role: m.Role, Content: m.Content})
	}
	wireMessages = append(wireMessages, wireMessage{Role: newTurn.Role, Content: newTurn.Content})

	reqBody := completionsRequestBody{
		Model:    modelID,
		Stream:   false,
		ChatID:   remoteChatID,
		ParentID: parentID,
		ID:       ids.AssistantMessageID,
		UserMessage: userMessageBody{
			ID:          ids.UserMessageID,
			ParentID:    parentID,
			ChildrenIDs: []string{ids.AssistantMessageID},
			Role:        "user",
			Content:     newTurn.Content,
			Timestamp:   sentAt.Unix(),
		},
		Messages:        wireMessages,
		BackgroundTasks: backgroundTasksBody{TitleGeneration: enableTitleGeneration},
	}
	if webSearchEnabled {
		reqBody.Features = &featuresBody{WebSearch: true}
	}
	if len(toolIDs) > 0 {
		reqBody.ToolIDs = toolIDs
	}
	if reqBody.Features != nil || len(reqBody.ToolIDs) > 0 {
		reqBody.Params = &paramsBody{FunctionCalling: "legacy"}
	}

	data, err := c.post(ctx, openwebui.PhaseTurn, "/api/chat/completions", reqBody)
	if err != nil {
		return openwebui.TurnResult{}, err
	}

	// Unmarshaling a top-level JSON null into a non-pointer struct is a
	// documented no-op in encoding/json (parsed stays its zero value),
	// so the chat-managed failure signal (HTTP 200, body null — compat
	// "a failure looks like success") and a genuine completion both
	// reach the GET-based confirmation below unmarshaled the same way.
	// Only a body that is neither an object nor null — a JSON string,
	// per the legacy-path malformed fixture — fails to decode at all,
	// which is schema drift this adapter refuses to guess at (D6: "body
	// is a JSON string or otherwise undecodable -> contract_failed").
	var parsed completionsResponseBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		return openwebui.TurnResult{}, openwebui.NewProviderError(openwebui.CategoryContractFailed, openwebui.PhaseTurn,
			fmt.Errorf("decode completion response: %w", err))
	}
	var finishReason *string
	if len(parsed.Choices) > 0 && parsed.Choices[0].FinishReason != "" {
		fr := parsed.Choices[0].FinishReason
		finishReason = &fr
	}

	outcome, err := c.LookupTurnOutcome(ctx, remoteChatID, ids.AssistantMessageID)
	if err != nil {
		return openwebui.TurnResult{}, err
	}
	switch {
	case outcome.HasError:
		return openwebui.TurnResult{}, openwebui.NewProviderError(openwebui.CategoryTurnFailed, openwebui.PhaseTurn,
			errors.New("assistant message reports an error"))
	case !outcome.Found || !outcome.Done:
		return openwebui.TurnResult{}, openwebui.NewProviderError(openwebui.CategoryAmbiguous, openwebui.PhaseTurn,
			errors.New("turn outcome not yet confirmed"))
	case outcome.Content == "":
		return openwebui.TurnResult{}, openwebui.NewProviderError(openwebui.CategoryContractFailed, openwebui.PhaseTurn,
			errors.New("done turn has empty content"))
	default:
		// A malformed/unrecognized sources[] shape (decodeSources' ok
		// == false) yields nil sources for this reply, never a turn
		// failure: the turn's actual content already decoded fine, and
		// citations are enrichment, not core to success.
		rawSources, _ := decodeSources(parsed.Sources)
		return openwebui.TurnResult{
			Content:          outcome.Content,
			RemoteCurrentID:  outcome.RemoteCurrentID,
			PromptTokens:     outcome.PromptTokens,
			CompletionTokens: outcome.CompletionTokens,
			FinishReason:     finishReason,
			Title:            outcome.Title,
			Sources:          normalizeSources(rawSources),
		}, nil
	}
}

// StartChat implements openwebui.Provider.
func (c *Client) StartChat(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
	remoteChatID, err := c.createChat(ctx, req.ModelID, req.SentAt)
	if err != nil {
		return openwebui.TurnResult{}, err
	}
	if req.OnChatCreated != nil {
		if err := req.OnChatCreated(ctx, remoteChatID); err != nil {
			return openwebui.TurnResult{}, err
		}
	}
	return c.runTurn(ctx, remoteChatID, nil, req.ModelID, req.Messages, req.NewTurn, req.IDs, req.SentAt, req.ToolIDs, req.WebSearchEnabled, req.EnableTitleGeneration)
}

// ContinueTurn implements openwebui.Provider. Title generation is never
// requested on a continuation: it is meaningful only once, for a chat's
// first turn (ContinueTurnRequest has no EnableTitleGeneration field at
// all — see its doc comment).
func (c *Client) ContinueTurn(ctx context.Context, req openwebui.ContinueTurnRequest) (openwebui.TurnResult, error) {
	return c.runTurn(ctx, req.RemoteChatID, req.IDs.ParentAssistantID, req.ModelID, req.Messages, req.NewTurn, req.IDs, req.SentAt, req.ToolIDs, req.WebSearchEnabled, false)
}

// --- GET /api/models ---

type modelsResponseBody struct {
	Data []modelResponseEntry `json:"data"`
}

type modelResponseEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arena is the built-in "arena-model" entry's own signal (compat: "A
	// built-in arena-model entry is present alongside real models",
	// fixtures/openwebui/models_response.json's second entry) — checked
	// alongside the literal id in ListModels' eligibility filter as
	// defense in depth, since GET /api/models's field list is the
	// compat document's own "weakest-evidenced part".
	Arena bool `json:"arena"`
	Info  *struct {
		Meta struct {
			ToolIDs           []string `json:"toolIds"`
			DefaultFeatureIDs []string `json:"defaultFeatureIds"`
		} `json:"meta"`
	} `json:"info"`
}

// ListModels implements openwebui.CatalogProvider (Issue #75 PR1) by
// calling GET /api/models and translating every entry the configured
// account can see, including each one's own info.meta.toolIds/
// defaultFeatureIds (Issue #74's original per-model tool defaults,
// generalized by Issue #75 PR5 to every model rather than one resolved
// at boot) — everything Registry.SyncCatalog needs to reconcile the
// registry and resolve per-model tool_ids in the same GET /api/models
// round, rather than a second call per model.
//
// It applies no eligibility filtering of its own (blank/duplicate ids,
// the built-in arena-model entry): that is Registry.SyncCatalog's
// decision, made from the openwebui.RemoteModel values this returns
// verbatim.
func (c *Client) ListModels(ctx context.Context) ([]openwebui.RemoteModel, error) {
	data, err := c.get(ctx, openwebui.PhaseTurn, "/api/models")
	if err != nil {
		return nil, err
	}

	var parsed modelsResponseBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, openwebui.NewProviderError(openwebui.CategoryContractFailed, openwebui.PhaseTurn,
			fmt.Errorf("decode models response: %w", err))
	}

	models := make([]openwebui.RemoteModel, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		rm := openwebui.RemoteModel{ID: m.ID, Name: m.Name, IsArena: m.Arena}
		if m.Info != nil {
			rm.ToolIDs = m.Info.Meta.ToolIDs
			rm.DefaultFeatureIDs = m.Info.Meta.DefaultFeatureIDs
		}
		models = append(models, rm)
	}
	return models, nil
}

// --- GET /api/v1/tools/ ---

type toolUserResponseEntry struct {
	ID string `json:"id"`
}

// ListAccessibleTools implements openwebui.CatalogProvider by calling
// GET /api/v1/tools/ (Issue #74) and returning every tool id this
// adapter's own account may invoke — the same access-filtered list Open
// WebUI itself resolves tool_ids against. Registry.SyncCatalog calls it
// once per sync round (Issue #75 PR5) to filter every model's own
// toolIds fail-closed, the same rule Issue #74 established for a single
// resolved-at-boot model.
func (c *Client) ListAccessibleTools(ctx context.Context) ([]string, error) {
	data, err := c.get(ctx, openwebui.PhaseTurn, "/api/v1/tools/")
	if err != nil {
		return nil, err
	}

	var parsed []toolUserResponseEntry
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, openwebui.NewProviderError(openwebui.CategoryContractFailed, openwebui.PhaseTurn,
			fmt.Errorf("decode tools response: %w", err))
	}

	ids := make([]string, 0, len(parsed))
	for _, t := range parsed {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// --- GET /api/v1/chats/{id} ---

type chatResponseBody struct {
	Chat struct {
		History struct {
			Messages  map[string]chatMessageBody `json:"messages"`
			CurrentID *string                    `json:"currentId"`
		} `json:"history"`
	} `json:"chat"`
	CurrentMessageID *string `json:"current_message_id"`
	// Title is Issue #84's addition: the chat's own current title,
	// top-level on this response the same way current_message_id is.
	// resolvedTitle is what actually decides whether it means anything
	// yet.
	Title string `json:"title"`
}

// resolvedTitle reports c's title once it has moved past the
// "bridge-precreated" placeholder createChat sets, or "" (meaning "not
// yet meaningful") otherwise — the one gate LookupTurnOutcome applies
// before ever surfacing a title to a caller.
func (c chatResponseBody) resolvedTitle() string {
	if c.Title == "" || c.Title == precreatedChatTitle {
		return ""
	}
	return boundedString(c.Title)
}

func (c chatResponseBody) currentID() *string {
	if c.Chat.History.CurrentID != nil {
		return c.Chat.History.CurrentID
	}
	return c.CurrentMessageID
}

type chatMessageBody struct {
	Done    bool   `json:"done"`
	Content string `json:"content"`
	// Error is decoded no further than "present or absent": ADR-0005 D6
	// requires provider error text be discarded rather than surfaced,
	// and the observed instance's error.content carries the upstream
	// credential pseudo-secret verbatim.
	Error json.RawMessage `json:"error"`
	Usage struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	} `json:"usage"`
}

// LookupTurnOutcome implements openwebui.Provider by calling
// GET /api/v1/chats/{id} and reading back only the one assistant
// message named, plus the chat's current-message pointer — never any
// other message, the title, or anything else the chat carries (the
// port's own doc comment on this method, and ADR-0005 D1's addendum,
// both hold this line: it is not GetChat).
func (c *Client) LookupTurnOutcome(ctx context.Context, remoteChatID, assistantMessageID string) (openwebui.TurnOutcome, error) {
	if !remoteIDPattern.MatchString(remoteChatID) {
		return openwebui.TurnOutcome{}, openwebui.NewProviderError(openwebui.CategoryPolicyViolation, openwebui.PhaseLookup,
			errors.New("remote chat id has an unexpected shape"))
	}

	data, err := c.get(ctx, openwebui.PhaseLookup, "/api/v1/chats/"+url.PathEscape(remoteChatID))
	if err != nil {
		return openwebui.TurnOutcome{}, err
	}

	// Unlike the completions response, a null GET body is not treated
	// as "proceed and see" — compat: "GET ... declares ChatResponse |
	// null", and there is no fallback confirmation this adapter can
	// make of a null chat. It is reported as unknown, not as "message
	// not found" (which means something more specific: the chat exists
	// but does not yet, or does not ever, carry this assistant id).
	if isJSONNull(data) {
		return openwebui.TurnOutcome{}, openwebui.NewProviderError(openwebui.CategoryAmbiguous, openwebui.PhaseLookup,
			errors.New("chat lookup returned null"))
	}

	var chat chatResponseBody
	if err := json.Unmarshal(data, &chat); err != nil {
		return openwebui.TurnOutcome{}, openwebui.NewProviderError(openwebui.CategoryContractFailed, openwebui.PhaseLookup,
			fmt.Errorf("decode chat response: %w", err))
	}

	var title *string
	if t := chat.resolvedTitle(); t != "" {
		title = &t
	}

	msg, ok := chat.Chat.History.Messages[assistantMessageID]
	if !ok {
		return openwebui.TurnOutcome{Found: false, RemoteCurrentID: chat.currentID(), Title: title}, nil
	}
	return openwebui.TurnOutcome{
		Found:            true,
		Done:             msg.Done,
		HasError:         msg.Error != nil,
		Content:          msg.Content,
		RemoteCurrentID:  chat.currentID(),
		PromptTokens:     msg.Usage.PromptTokens,
		CompletionTokens: msg.Usage.CompletionTokens,
		Title:            title,
	}, nil
}

// --- shared HTTP plumbing ---

// post marshals payload, refuses to send it unsent when it exceeds
// maxRequestBytes (returning ErrRequestTooLarge wrapped as
// CategoryClientRejected — roadmap: "must not silently send an
// incomplete context", so this client never truncates instead), and
// performs the call.
func (c *Client) post(ctx context.Context, phase openwebui.Phase, path string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, openwebui.NewProviderError(openwebui.CategoryContractFailed, phase, fmt.Errorf("encode request: %w", err))
	}
	if int64(len(body)) > c.maxRequestBytes {
		return nil, openwebui.NewProviderError(openwebui.CategoryClientRejected, phase, openwebui.ErrRequestTooLarge)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, openwebui.NewProviderError(openwebui.CategoryTransport, phase, fmt.Errorf("build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	return c.do(reqCtx, phase, httpReq)
}

func (c *Client) get(ctx context.Context, phase openwebui.Phase, path string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, openwebui.NewProviderError(openwebui.CategoryTransport, phase, fmt.Errorf("build request: %w", err))
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	return c.do(reqCtx, phase, httpReq)
}

func (c *Client) do(ctx context.Context, phase openwebui.Phase, httpReq *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, classifyDoError(ctx, phase, err)
	}
	defer drainAndClose(resp.Body, c.maxResponseBytes)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, openwebui.NewProviderError(categorizeStatus(resp.StatusCode), phase, fmt.Errorf("status %d", resp.StatusCode))
	}

	data, err := safehttp.ReadLimited(resp.Body, c.maxResponseBytes)
	if err != nil {
		return nil, openwebui.NewProviderError(openwebui.CategoryContractFailed, phase, err)
	}
	return data, nil
}

// classifyDoError distinguishes a policy violation
// (safehttp.ErrPolicyViolation: a disallowed redirect, scheme, or
// SSRF-guarded address) and a timeout from every other transport-level
// failure, mirroring internal/provider/openai's and
// internal/ingest/rss's identically-shaped helpers.
func classifyDoError(ctx context.Context, phase openwebui.Phase, err error) error {
	if errors.Is(err, safehttp.ErrPolicyViolation) {
		return openwebui.NewProviderError(openwebui.CategoryPolicyViolation, phase, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return openwebui.NewProviderError(openwebui.CategoryTimeout, phase, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return openwebui.NewProviderError(openwebui.CategoryTimeout, phase, err)
	}
	if ctx.Err() != nil {
		return openwebui.NewProviderError(openwebui.CategoryTimeout, phase, err)
	}
	return openwebui.NewProviderError(openwebui.CategoryTransport, phase, err)
}

// categorizeStatus is ADR-0005 D6's non-2xx side: an upstream failure
// arriving as an actual HTTP error status, as opposed to the chat-
// managed path's "failure looks like success" case that createChat and
// runTurn each detect from a decoded body instead.
func categorizeStatus(status int) openwebui.Category {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return openwebui.CategoryAuthFailed
	case status == http.StatusTooManyRequests:
		return openwebui.CategoryRateLimited
	case status >= 500:
		return openwebui.CategoryServerError
	default:
		return openwebui.CategoryClientRejected
	}
}

// isJSONNull reports whether data is exactly the JSON literal null,
// ignoring surrounding whitespace. Both POST /api/v1/chats/new and
// GET /api/v1/chats/{id} declare ChatResponse | null as their 200
// schema (compat), and this is the one place that literal must be told
// apart from a decodable object.
func isJSONNull(data []byte) bool {
	return string(bytes.TrimSpace(data)) == "null"
}

// drainAndClose gives net/http a chance to reuse a keep-alive connection
// after an early return. The drain is bounded because the upstream
// response is untrusted, mirroring internal/provider/openai's and
// internal/ingest/rss's identically-named helpers.
func drainAndClose(body io.ReadCloser, maxBytes int64) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxBytes+1))
	_ = body.Close()
}
