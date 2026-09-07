package openwebui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// testAPIKey is this test file's stand-in credential. Every redaction
// test asserts it never appears in a returned error or value, the same
// way the fixtures' sk-mock-upstream-secret is asserted absent.
const testAPIKey = "sk-test-api-key"

func fixturesDir() string {
	return filepath.Join("..", "..", "..", "docs", "compat", "fixtures", "openwebui")
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir(), name))
	if err != nil {
		t.Fatalf("load fixture %s: %v", name, err)
	}
	return data
}

// allowAnyIP is every test's Config.AllowIPForTesting override except
// TestClient_ContinueTurn_PrivateIPIsPolicyViolation, which omits it to
// prove the production default actually rejects a loopback address.
func allowAnyIP(net.IP) bool { return true }

// newTestClient builds a Client against server, permitting the loopback
// address and plain-http scheme httptest.Server uses — neither of which
// a production deployment allows (OPENWEBUI_BASE_URL is validated as
// HTTPS, and safehttp's default IP policy excludes loopback).
func newTestClient(t *testing.T, server *httptest.Server, overrides func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL:                     server.URL,
		AllowedOrigins:              []string{server.URL},
		APIKey:                      testAPIKey,
		Timeout:                     5 * time.Second,
		MaxResponseBytes:            1 << 20,
		MaxRequestBytes:             1 << 20,
		AllowIPForTesting:           allowAnyIP,
		AllowInsecureHTTPForTesting: true,
	}
	if overrides != nil {
		overrides(&cfg)
	}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// jsonEqual reports whether a and b decode to the same JSON value,
// independent of key order or exact byte formatting — what a fixture
// comparison needs, since json.Marshal's key order need not match a
// fixture file's.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode first JSON value: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode second JSON value: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}

// chatGetBody builds a synthetic GET /api/v1/chats/{id} response body
// carrying exactly one message under assistantID, so a test can control
// done/error/content precisely without depending on a fixture's own
// captured ids.
func chatGetBody(t *testing.T, assistantID string, message any, currentID string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"chat": map[string]any{
			"history": map[string]any{
				"messages":  map[string]any{assistantID: message},
				"currentId": currentID,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal chat GET body: %v", err)
	}
	return data
}

func requireProviderError(t *testing.T, err error) *openwebui.ProviderError {
	t.Helper()
	var pe *openwebui.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *openwebui.ProviderError", err)
	}
	return pe
}

func writeJSON(t *testing.T, w http.ResponseWriter, body []byte, status int) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

// --- linear happy path, against the pinned capture fixtures ---

// TestClient_StartChat_LinearAgainstFixtures backs plan §7.2's "linear"
// case: StartChat must send exactly the captured chats/new and
// completions request bodies (same client-generated ids as the
// fixtures), call OnChatCreated with the server-assigned id before
// generating, and confirm the outcome via GET rather than trusting the
// completions response's own content.
func TestClient_StartChat_LinearAgainstFixtures(t *testing.T) {
	const (
		chatID      = "76f89a52-6ff6-423f-9a4a-adaa9370b687"
		userMsgID   = "7e9cee65-cc92-45ef-911f-16ab2bbca306"
		assistantID = "bf927bbf-3055-4844-bda2-d12ff775f618"
	)
	wantCreateReq := loadFixture(t, "chats_new_empty_request.json")
	createResp := loadFixture(t, "chats_new_empty_response.json")
	wantTurnReq := loadFixture(t, "completions_start_request.json")
	turnResp := loadFixture(t, "completions_start_response.json")
	getResp := loadFixture(t, "chat_after_start.json")

	var sawAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/chats/new", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if !jsonEqual(t, body, wantCreateReq) {
			t.Errorf("chats/new request body = %s, want (JSON-equivalent to) %s", body, wantCreateReq)
		}
		writeJSON(t, w, createResp, http.StatusOK)
	})
	mux.HandleFunc("POST /api/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !jsonEqual(t, body, wantTurnReq) {
			t.Errorf("completions request body = %s, want (JSON-equivalent to) %s", body, wantTurnReq)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode completions request: %v", err)
		}
		if _, ok := raw["parent_id"]; !ok {
			t.Error("completions request has no parent_id key (ADR-0005 D4)")
		}
		writeJSON(t, w, turnResp, http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/chats/"+chatID, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, server, nil)

	var onChatCreatedID string
	result, err := client.StartChat(t.Context(), openwebui.StartChatRequest{
		ModelID: "mock-model",
		NewTurn: openwebui.Message{Role: "user", Content: "precreated turn 1"},
		IDs:     openwebui.TurnIDs{UserMessageID: userMsgID, AssistantMessageID: assistantID},
		SentAt:  time.Unix(1788672375, 0).UTC(),
		OnChatCreated: func(ctx context.Context, remoteChatID string) error {
			onChatCreatedID = remoteChatID
			return nil
		},
	})
	if err != nil {
		t.Fatalf("StartChat: %v", err)
	}
	if onChatCreatedID != chatID {
		t.Errorf("OnChatCreated remoteChatID = %q, want %q", onChatCreatedID, chatID)
	}
	if sawAuth != "Bearer "+testAPIKey {
		t.Errorf("Authorization header = %q, want %q", sawAuth, "Bearer "+testAPIKey)
	}

	const wantContent = "mock reply #14 (received 1 messages; last user: precreated turn 1)"
	if result.Content != wantContent {
		t.Errorf("Content = %q, want %q", result.Content, wantContent)
	}
	if result.RemoteCurrentID == nil || *result.RemoteCurrentID != assistantID {
		t.Errorf("RemoteCurrentID = %v, want %q", result.RemoteCurrentID, assistantID)
	}
	if result.PromptTokens == nil || *result.PromptTokens != 10 {
		t.Errorf("PromptTokens = %v, want 10", result.PromptTokens)
	}
	if result.CompletionTokens == nil || *result.CompletionTokens != 12 {
		t.Errorf("CompletionTokens = %v, want 12", result.CompletionTokens)
	}
	if result.FinishReason == nil || *result.FinishReason != "stop" {
		t.Errorf("FinishReason = %v, want \"stop\"", result.FinishReason)
	}
}

// TestClient_ContinueTurn_LinearAgainstFixtures backs compat (b): both
// parent_id and user_message.parentId must be set to the previous
// assistant message id, the exact pitfall the pinned
// duplicate_delivery_chat.json capture shows an omission of.
func TestClient_ContinueTurn_LinearAgainstFixtures(t *testing.T) {
	const (
		chatID       = "76f89a52-6ff6-423f-9a4a-adaa9370b687"
		prevAssistID = "bf927bbf-3055-4844-bda2-d12ff775f618"
		userMsgID    = "8332223a-7b0f-4d29-b38c-4ab9fd159daf"
		assistantID  = "5c49e5b8-e204-4696-90e9-00133d00a843"
	)
	wantTurnReq := loadFixture(t, "completions_continue_request.json")
	turnResp := loadFixture(t, "completions_continue_response.json")
	getResp := loadFixture(t, "chat_after_continue.json")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !jsonEqual(t, body, wantTurnReq) {
			t.Errorf("completions request body = %s, want (JSON-equivalent to) %s", body, wantTurnReq)
		}
		var raw struct {
			ParentID    *string `json:"parent_id"`
			UserMessage struct {
				ParentID *string `json:"parentId"`
			} `json:"user_message"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode completions request: %v", err)
		}
		if raw.ParentID == nil || *raw.ParentID != prevAssistID {
			t.Errorf("parent_id = %v, want %q", raw.ParentID, prevAssistID)
		}
		if raw.UserMessage.ParentID == nil || *raw.UserMessage.ParentID != prevAssistID {
			t.Errorf("user_message.parentId = %v, want %q (compat (b): omitting this orphans the pair)", raw.UserMessage.ParentID, prevAssistID)
		}
		writeJSON(t, w, turnResp, http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/chats/"+chatID, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, server, nil)

	parent := prevAssistID
	result, err := client.ContinueTurn(t.Context(), openwebui.ContinueTurnRequest{
		RemoteChatID: chatID,
		ModelID:      "mock-model",
		Messages: []openwebui.Message{
			{Role: "user", Content: "precreated turn 1"},
			{Role: "assistant", Content: "(prev reply)"},
		},
		NewTurn: openwebui.Message{Role: "user", Content: "precreated turn 2"},
		IDs: openwebui.TurnIDs{
			UserMessageID:      userMsgID,
			AssistantMessageID: assistantID,
			ParentAssistantID:  &parent,
		},
		SentAt: time.Unix(1788672375, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	const wantContent = "mock reply #15 (received 3 messages; last user: precreated turn 2)"
	if result.Content != wantContent {
		t.Errorf("Content = %q, want %q", result.Content, wantContent)
	}
	if result.RemoteCurrentID == nil || *result.RemoteCurrentID != assistantID {
		t.Errorf("RemoteCurrentID = %v, want %q", result.RemoteCurrentID, assistantID)
	}
}

// --- Issue #72: opt-in web search / tool_ids ---

// TestClient_ContinueTurn_WebSearchAndToolIDsDefaultOff_OmitsBothKeys backs
// plan §7's "existing fixture-matching tests keep passing" requirement:
// with Config.WebSearchEnabled/ToolIDs left at their zero values, the
// completions request body must carry neither a "features" nor a
// "tool_ids" key at all, not merely false/empty values — the exact
// request shape this client sent before Issue #72.
func TestClient_ContinueTurn_WebSearchAndToolIDsDefaultOff_OmitsBothKeys(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawKeys map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &sawKeys); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			writeJSON(t, w, []byte(`{}`), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	if _, err := client.ContinueTurn(t.Context(), req); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if _, ok := sawKeys["features"]; ok {
		t.Error(`completions request has a "features" key, want it entirely absent when WebSearchEnabled is false`)
	}
	if _, ok := sawKeys["tool_ids"]; ok {
		t.Error(`completions request has a "tool_ids" key, want it entirely absent when ToolIDs is empty`)
	}
}

// TestClient_ContinueTurn_WebSearchAndToolIDsConfigured_SendsBoth backs
// plan §1.3: an operator who sets WebSearchEnabled/ToolIDs gets
// features.web_search=true and tool_ids sent verbatim on every
// completions call.
func TestClient_ContinueTurn_WebSearchAndToolIDsConfigured_SendsBoth(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawBody struct {
		Features *struct {
			WebSearch bool `json:"web_search"`
		} `json:"features"`
		ToolIDs []string `json:"tool_ids"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &sawBody); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			writeJSON(t, w, []byte(`{}`), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) {
		cfg.WebSearchEnabled = true
		cfg.ToolIDs = []string{"web_search", "server:mcp:example"}
	})

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	if _, err := client.ContinueTurn(t.Context(), req); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if sawBody.Features == nil || !sawBody.Features.WebSearch {
		t.Errorf("completions request features = %+v, want {web_search: true}", sawBody.Features)
	}
	wantToolIDs := []string{"web_search", "server:mcp:example"}
	if !reflect.DeepEqual(sawBody.ToolIDs, wantToolIDs) {
		t.Errorf("completions request tool_ids = %v, want %v", sawBody.ToolIDs, wantToolIDs)
	}
}

// --- Issue #74: params.function_calling=legacy ---

// TestClient_ContinueTurn_NeitherFeatureNorToolIDs_OmitsParams backs
// Issue #74's "no behavior change for a plain turn" requirement:
// without WebSearchEnabled/ToolIDs configured, the completions request
// carries no "params" key at all — byte-for-byte the same shape as
// before Issue #74, matching TestClient_..._DefaultOff_OmitsBothKeys's
// "the key itself is absent, not merely empty" style.
func TestClient_ContinueTurn_NeitherFeatureNorToolIDs_OmitsParams(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawKeys map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &sawKeys); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			writeJSON(t, w, []byte(`{}`), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	if _, err := client.ContinueTurn(t.Context(), req); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if _, ok := sawKeys["params"]; ok {
		t.Error(`completions request has a "params" key, want it entirely absent when neither WebSearchEnabled nor ToolIDs is set`)
	}
}

// TestClient_ContinueTurn_ToolIDsConfigured_SendsLegacyFunctionCalling
// and TestClient_ContinueTurn_WebSearchConfigured_SendsLegacyFunctionCalling
// back Issue #74's fix: whenever either opt-in flag is in use, the
// completions request also carries params.function_calling="legacy" -
// Open WebUI's non_streaming_chat_response_handler never processes a
// native tool_calls response (ADR-0005's tool/web-search addendum), so
// this buffered stream:false adapter would otherwise hang the assistant
// message at done:false forever whenever the model decided to call a
// tool (Issue #74's exact report).
func TestClient_ContinueTurn_ToolIDsConfigured_SendsLegacyFunctionCalling(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawBody struct {
		Params *struct {
			FunctionCalling string `json:"function_calling"`
		} `json:"params"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &sawBody); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			writeJSON(t, w, []byte(`{}`), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) {
		cfg.ToolIDs = []string{"calculator"}
	})

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	if _, err := client.ContinueTurn(t.Context(), req); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if sawBody.Params == nil || sawBody.Params.FunctionCalling != "legacy" {
		t.Errorf("completions request params = %+v, want {function_calling: legacy}", sawBody.Params)
	}
}

func TestClient_ContinueTurn_WebSearchConfigured_SendsLegacyFunctionCalling(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawBody struct {
		Params *struct {
			FunctionCalling string `json:"function_calling"`
		} `json:"params"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &sawBody); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			writeJSON(t, w, []byte(`{}`), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) {
		cfg.WebSearchEnabled = true
	})

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	if _, err := client.ContinueTurn(t.Context(), req); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if sawBody.Params == nil || sawBody.Params.FunctionCalling != "legacy" {
		t.Errorf("completions request params = %+v, want {function_calling: legacy}", sawBody.Params)
	}
}

// --- Issue #74: startup-time tool/model resolution ---

// TestClient_GetModelTools_ReturnsToolIDsAndDefaultFeatureIDs backs
// Phase 1: GetModelTools reads modelID's own info.meta.toolIds/
// defaultFeatureIds out of GET /api/models's response (fixture recorded
// against the pinned target — docs/compat/openwebui-0.11.3.md).
func TestClient_GetModelTools_ReturnsToolIDsAndDefaultFeatureIDs(t *testing.T) {
	modelsResp := loadFixture(t, "models_response.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/models" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, modelsResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	toolIDs, defaultFeatureIDs, err := client.GetModelTools(t.Context(), "mock-model")
	if err != nil {
		t.Fatalf("GetModelTools: %v", err)
	}
	if !reflect.DeepEqual(toolIDs, []string{"calculator"}) {
		t.Errorf("toolIDs = %v, want [calculator]", toolIDs)
	}
	if !reflect.DeepEqual(defaultFeatureIDs, []string{"web_search"}) {
		t.Errorf("defaultFeatureIDs = %v, want [web_search]", defaultFeatureIDs)
	}
}

// TestClient_GetModelTools_UnknownModelReturnsNil backs the "not this
// call's concern" contract: a model id absent from the response (an
// access-filtered or unknown model) returns nil, nil, nil rather than
// an error - the resolution caller decides what an unknown model means.
func TestClient_GetModelTools_UnknownModelReturnsNil(t *testing.T) {
	modelsResp := loadFixture(t, "models_response.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, modelsResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	toolIDs, defaultFeatureIDs, err := client.GetModelTools(t.Context(), "no-such-model")
	if err != nil {
		t.Fatalf("GetModelTools: %v", err)
	}
	if toolIDs != nil || defaultFeatureIDs != nil {
		t.Errorf("toolIDs=%v defaultFeatureIDs=%v, want both nil for an unknown model", toolIDs, defaultFeatureIDs)
	}
}

// TestClient_ListAccessibleTools_ReturnsEveryToolID backs Phase 1:
// ListAccessibleTools reads every tool's id out of GET /api/v1/tools/'s
// response (fixture recorded against the pinned target).
func TestClient_ListAccessibleTools_ReturnsEveryToolID(t *testing.T) {
	toolsResp := loadFixture(t, "tools_response.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/tools/" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, toolsResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	ids, err := client.ListAccessibleTools(t.Context())
	if err != nil {
		t.Fatalf("ListAccessibleTools: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"calculator"}) {
		t.Errorf("ids = %v, want [calculator]", ids)
	}
}

// --- Issue #75 PR1: catalog sync ---

// TestClient_ListModels_TranslatesEveryEntry backs Registry.SyncCatalog's
// one call to the provider: every data[] entry becomes an
// openwebui.RemoteModel carrying id, name, the arena signal, and the same
// info.meta.toolIds/defaultFeatureIds GetModelTools reads for a single
// model — including the built-in arena-model entry, which ListModels
// does not filter (that is Registry's eligibleRemoteModels' job, not
// this adapter's).
func TestClient_ListModels_TranslatesEveryEntry(t *testing.T) {
	modelsResp := loadFixture(t, "models_response.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/models" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, modelsResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	got, err := client.ListModels(t.Context())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []openwebui.RemoteModel{
		{ID: "mock-model", Name: "mock-model", ToolIDs: []string{"calculator"}, DefaultFeatureIDs: []string{"web_search"}},
		{ID: "arena-model", Name: "Arena Model", IsArena: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListModels = %+v, want %+v", got, want)
	}
}

// TestClient_ListModels_EmptyDataIsNotAnError backs the "successful empty
// snapshot" case Registry.SyncCatalog treats as a genuine zero-model
// response rather than an outage.
func TestClient_ListModels_EmptyDataIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []byte(`{"data":[]}`), http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	got, err := client.ListModels(t.Context())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListModels = %+v, want empty", got)
	}
}

// TestClient_ListModels_MalformedBodyIsContractFailed pins the same
// decode-failure classification GetModelTools uses for this endpoint.
func TestClient_ListModels_MalformedBodyIsContractFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []byte(`"not an object"`), http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.ListModels(t.Context())
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryContractFailed {
		t.Errorf("Category = %q, want %q", pe.Category, openwebui.CategoryContractFailed)
	}
}

// --- error classification ---

func TestClient_StartChat_ChatsNewReturns401_AuthFailed(t *testing.T) {
	body := loadFixture(t, "error_401_invalid_token.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, body, http.StatusUnauthorized)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.StartChat(t.Context(), minimalStartChatReq())
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryAuthFailed || pe.Phase != openwebui.PhaseCreate {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryAuthFailed, openwebui.PhaseCreate)
	}
}

func TestClient_ContinueTurn_CompletionsReturns400ModelNotFound_ClientRejected(t *testing.T) {
	body := loadFixture(t, "error_400_model_not_found.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, body, http.StatusBadRequest)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryClientRejected || pe.Phase != openwebui.PhaseTurn {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryClientRejected, openwebui.PhaseTurn)
	}
}

func TestClient_ContinueTurn_CompletionsReturns429_RateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []byte(`{"detail":"synthetic rate limit"}`), http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryRateLimited || pe.Phase != openwebui.PhaseTurn {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryRateLimited, openwebui.PhaseTurn)
	}
}

func TestClient_ContinueTurn_CompletionsReturns500_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []byte(`{"detail":"synthetic server error"}`), http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryServerError || pe.Phase != openwebui.PhaseTurn {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryServerError, openwebui.PhaseTurn)
	}
}

// TestClient_ContinueTurn_ChatManagedErrorViaGet_TurnFailed backs ADR-0005
// D6's central case: a chat-managed failure is HTTP 200 with a null
// body, readable only by fetching the chat and finding the assistant
// message's error. It also backs the redaction rule: the pinned
// fixture's error.content carries the pseudo-secret
// sk-mock-upstream-secret verbatim, and neither the returned error nor
// the API key must ever surface it.
func TestClient_ContinueTurn_ChatManagedErrorViaGet_TurnFailed(t *testing.T) {
	const assistantID = "assistant-1"
	var failedMessage map[string]any
	if err := json.Unmarshal(loadFixture(t, "error_chat_managed_message_state.json"), &failedMessage); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	getResp := chatGetBody(t, assistantID, failedMessage, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/chat/completions":
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case r.Method == http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	_, err := client.ContinueTurn(t.Context(), req)
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryTurnFailed || pe.Phase != openwebui.PhaseTurn {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryTurnFailed, openwebui.PhaseTurn)
	}

	const secret = "sk-mock-upstream-secret"
	if strings.Contains(err.Error(), secret) {
		t.Errorf("err.Error() = %q leaks the upstream error text", err.Error())
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("err.Error() = %q leaks the API key", err.Error())
	}
}

// TestClient_ContinueTurn_ChatManagedPendingViaGet_Ambiguous is the
// still-generating case: done:false with no error is not a failure, it
// is unknown.
func TestClient_ContinueTurn_ChatManagedPendingViaGet_Ambiguous(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{
		"done": false, "content": "",
	}, "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case r.Method == http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	_, err := client.ContinueTurn(t.Context(), req)
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryAmbiguous || pe.Phase != openwebui.PhaseTurn {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryAmbiguous, openwebui.PhaseTurn)
	}
}

// TestClient_ContinueTurn_ChatManagedSuccessViaGet_Succeeds covers the
// "200 null then GET shows done:true" success path, and a completions
// response carrying no recognizable fields at all (schema drift), which
// must not by itself break decoding since D3 never trusts that body's
// content anyway.
func TestClient_ContinueTurn_ChatManagedSuccessViaGet_Succeeds(t *testing.T) {
	const assistantID = "assistant-1"
	promptTokens, completionTokens := 3, 4
	getResp := chatGetBody(t, assistantID, map[string]any{
		"done": true, "content": "hello from the chat",
		"usage": map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens},
	}, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			// Schema drift: a decodable object with none of the fields
			// this adapter reads. It must not fail the call — D3 never
			// uses this body's content, only GET's.
			writeJSON(t, w, []byte(`{"unexpected_field":123}`), http.StatusOK)
		case r.Method == http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	result, err := client.ContinueTurn(t.Context(), req)
	if err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if result.Content != "hello from the chat" {
		t.Errorf("Content = %q, want %q", result.Content, "hello from the chat")
	}
	if result.FinishReason != nil {
		t.Errorf("FinishReason = %v, want nil (no choices in the completions response)", result.FinishReason)
	}
	if result.PromptTokens == nil || *result.PromptTokens != promptTokens {
		t.Errorf("PromptTokens = %v, want %d", result.PromptTokens, promptTokens)
	}
}

// TestClient_ContinueTurn_CompletionsBodyIsJSONString_ContractFailed backs
// D6's "body is a JSON string or otherwise undecodable -> contract_failed":
// a bare JSON string is schema drift this adapter refuses to guess at,
// and it must not call GET afterward.
func TestClient_ContinueTurn_CompletionsBodyIsJSONString_ContractFailed(t *testing.T) {
	body := loadFixture(t, "error_legacy_upstream_malformed.json")
	getHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			writeJSON(t, w, body, http.StatusOK)
		case r.Method == http.MethodGet:
			getHits++
			writeJSON(t, w, []byte("null"), http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryContractFailed || pe.Phase != openwebui.PhaseTurn {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryContractFailed, openwebui.PhaseTurn)
	}
	if getHits != 0 {
		t.Errorf("GET was called %d times, want 0 (a malformed completions body must not trigger a lookup)", getHits)
	}
}

func TestClient_ContinueTurn_GetReturnsNull_Ambiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case r.Method == http.MethodGet:
			writeJSON(t, w, []byte("null"), http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryAmbiguous || pe.Phase != openwebui.PhaseLookup {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryAmbiguous, openwebui.PhaseLookup)
	}
}

// TestClient_LookupTurnOutcome_MessageAbsent_FoundFalse backs the
// "message not yet (or never) persisted" case a job's own retry
// handling (Issue #53's later PRs) depends on being distinguishable
// from every other outcome.
func TestClient_LookupTurnOutcome_MessageAbsent_FoundFalse(t *testing.T) {
	getResp := chatGetBody(t, "some-other-message", map[string]any{"done": true, "content": "x"}, "some-other-message")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", "missing-assistant-id")
	if err != nil {
		t.Fatalf("LookupTurnOutcome: %v", err)
	}
	if outcome.Found {
		t.Errorf("Found = true, want false")
	}
}

// --- boundaries ---

func TestClient_ContinueTurn_TimeoutIsClassifiedAsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(t, w, []byte("null"), http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) { cfg.Timeout = 20 * time.Millisecond })

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryTimeout {
		t.Errorf("Category = %q, want %q", pe.Category, openwebui.CategoryTimeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false")
	}
}

func TestClient_ContinueTurn_ContextCancelIsClassifiedAsTimeout(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		writeJSON(t, w, []byte("null"), http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	_, err := client.ContinueTurn(ctx, minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryTimeout {
		t.Errorf("Category = %q, want %q", pe.Category, openwebui.CategoryTimeout)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false")
	}
}

func TestClient_LookupTurnOutcome_ResponseTooLarge_ContractFailed(t *testing.T) {
	big := chatGetBody(t, "assistant-1", map[string]any{
		"done": true, "content": strings.Repeat("x", 4096),
	}, "assistant-1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, big, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) { cfg.MaxResponseBytes = 16 })

	_, err := client.LookupTurnOutcome(t.Context(), "chat-1", "assistant-1")
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryContractFailed || pe.Phase != openwebui.PhaseLookup {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryContractFailed, openwebui.PhaseLookup)
	}
}

// TestClient_ContinueTurn_RequestTooLarge_NotSent backs the roadmap's
// "must not silently send an incomplete context": exceeding
// MaxRequestBytes must fail the call closed without truncating
// anything, and without ever reaching the network.
func TestClient_ContinueTurn_RequestTooLarge_NotSent(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeJSON(t, w, []byte("null"), http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) { cfg.MaxRequestBytes = 8 })

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	if !errors.Is(err, openwebui.ErrRequestTooLarge) {
		t.Errorf("errors.Is(err, ErrRequestTooLarge) = false, err = %v", err)
	}
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryClientRejected {
		t.Errorf("Category = %q, want %q", pe.Category, openwebui.CategoryClientRejected)
	}
	if hits != 0 {
		t.Errorf("server was hit %d times, want 0 (an over-size request must never be sent)", hits)
	}
}

// TestClient_StartChat_RedirectIsPolicyViolation_NoSecondRequest backs
// ADR-0005 D11: redirects are disabled outright, so a redirect target
// is never even contacted.
func TestClient_StartChat_RedirectIsPolicyViolation_NoSecondRequest(t *testing.T) {
	targetHits := 0
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/chats/new", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/redirected", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/redirected", func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		writeJSON(t, w, []byte("null"), http.StatusOK)
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, server, nil)

	_, err := client.StartChat(t.Context(), minimalStartChatReq())
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryPolicyViolation || pe.Phase != openwebui.PhaseCreate {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, openwebui.CategoryPolicyViolation, openwebui.PhaseCreate)
	}
	if targetHits != 0 {
		t.Errorf("redirect target was hit %d times, want 0", targetHits)
	}
}

func TestNewClient_RejectsBaseURLNotInAllowlist(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL:        "http://evil.example",
		AllowedOrigins: []string{"https://openwebui.example.net"},
		APIKey:         testAPIKey,
		Timeout:        time.Second,
	})
	if err == nil {
		t.Fatal("NewClient with a base URL outside the allowlist did not error")
	}
}

// TestClient_ContinueTurn_PrivateIPIsPolicyViolation deliberately omits
// Config.AllowIPForTesting, proving the production default
// (internal/ingest/safehttp's public-unicast-only policy) rejects the
// loopback address httptest.Server binds to.
func TestClient_ContinueTurn_PrivateIPIsPolicyViolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must never be reached: the dial itself should be refused")
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) { cfg.AllowIPForTesting = nil })

	_, err := client.ContinueTurn(t.Context(), minimalContinueTurnReq("chat-1"))
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryPolicyViolation {
		t.Errorf("Category = %q, want %q", pe.Category, openwebui.CategoryPolicyViolation)
	}
}

// TestClient_LookupTurnOutcome_RejectsPathTraversalChatID backs the URL-
// building rule: the only caller-supplied string embedded in a request
// path is the chat id, and this client refuses to embed one that could
// act as a path separator or traversal segment.
func TestClient_LookupTurnOutcome_RejectsPathTraversalChatID(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeJSON(t, w, []byte("null"), http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	for _, badID := range []string{"../etc/passwd", "chat/1", "chat%2F1"} {
		_, err := client.LookupTurnOutcome(t.Context(), badID, "assistant-1")
		pe := requireProviderError(t, err)
		if pe.Category != openwebui.CategoryPolicyViolation || pe.Phase != openwebui.PhaseLookup {
			t.Errorf("chat id %q: pe = %+v, want Category=%q Phase=%q", badID, pe, openwebui.CategoryPolicyViolation, openwebui.PhaseLookup)
		}
	}
	if hits != 0 {
		t.Errorf("server was hit %d times, want 0 (a rejected chat id must never reach the network)", hits)
	}
}

// TestClient_LookupTurnOutcome_NeverExposesErrorContent is a standing
// regression guard for the redaction rule ADR-0005 D6 requires: even
// when the assistant message carries an error object with upstream
// text, TurnOutcome (and everything reachable from it) must contain
// nothing from that object.
func TestClient_LookupTurnOutcome_NeverExposesErrorContent(t *testing.T) {
	const assistantID = "assistant-1"
	var failedMessage map[string]any
	if err := json.Unmarshal(loadFixture(t, "error_chat_managed_message_state.json"), &failedMessage); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	getResp := chatGetBody(t, assistantID, failedMessage, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", assistantID)
	if err != nil {
		t.Fatalf("LookupTurnOutcome: %v", err)
	}
	if !outcome.HasError {
		t.Fatalf("HasError = false, want true")
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("marshal TurnOutcome: %v", err)
	}
	if strings.Contains(string(encoded), "sk-mock-upstream-secret") {
		t.Errorf("TurnOutcome %s leaks the upstream error text", encoded)
	}
}

// --- shared request builders ---

func minimalStartChatReq() openwebui.StartChatRequest {
	return openwebui.StartChatRequest{
		ModelID: "mock-model",
		NewTurn: openwebui.Message{Role: "user", Content: "hello"},
		IDs:     openwebui.TurnIDs{UserMessageID: "user-1", AssistantMessageID: "assistant-1"},
		SentAt:  time.Now(),
	}
}

func minimalContinueTurnReq(chatID string) openwebui.ContinueTurnRequest {
	parent := "parent-1"
	return openwebui.ContinueTurnRequest{
		RemoteChatID: chatID,
		ModelID:      "mock-model",
		NewTurn:      openwebui.Message{Role: "user", Content: "hello"},
		IDs: openwebui.TurnIDs{
			UserMessageID:      "user-1",
			AssistantMessageID: "assistant-1",
			ParentAssistantID:  &parent,
		},
		SentAt: time.Now(),
	}
}
