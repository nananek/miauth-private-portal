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

	"github.com/nananek/miauth-private-portal/internal/config"
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
		ToolTurnTimeout:             5 * time.Second,
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
// with the request's WebSearchEnabled and ToolIDs left at their zero
// values, the completions request body must carry neither a "features"
// nor a "tool_ids" key at all, not merely false/empty values — the exact
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
		t.Error(`completions request has a "features" key, want it entirely absent when req.WebSearchEnabled is false`)
	}
	if _, ok := sawKeys["tool_ids"]; ok {
		t.Error(`completions request has a "tool_ids" key, want it entirely absent when ToolIDs is empty`)
	}
}

// TestClient_ContinueTurn_WebSearchAndToolIDsConfigured_SendsBoth backs
// plan §1.3, generalized by Issue #75 PR5/AC#11: a request with
// WebSearchEnabled set gets features.web_search=true on that call, and
// whatever ToolIDs a caller resolved for this turn's model is sent
// verbatim as tool_ids.
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
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	req.ToolIDs = []string{"web_search", "server:mcp:example"}
	req.WebSearchEnabled = true
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

// TestClient_ContinueTurn_ToolIDsConfigured_SendsStreamTrueNoParams and
// TestClient_ContinueTurn_WebSearchConfigured_SendsStreamTrueNoParams back
// ADR-0005 D27 (Issue #123): whenever either opt-in flag is in use, the
// completions request is sent with stream:true and no params key at all,
// so Open WebUI's native tool-calling loop actually runs — replacing
// Issue #74's params.function_calling="legacy" fix (D17), which D27
// retires entirely (its buffered stream:false shape is what Issue #93/
// #120 showed cannot itself run a tool over HTTP without D24's separate,
// now-also-retired stateless mode).
func TestClient_ContinueTurn_ToolIDsConfigured_SendsStreamTrueNoParams(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawBody struct {
		Stream bool            `json:"stream"`
		Params json.RawMessage `json:"params"`
	}
	sawKeys := map[string]json.RawMessage{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &sawBody); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			if err := json.Unmarshal(body, &sawKeys); err != nil {
				t.Fatalf("decode completions request keys: %v", err)
			}
			writeJSON(t, w, []byte(`null`), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	req.ToolIDs = []string{"calculator"}
	if _, err := client.ContinueTurn(t.Context(), req); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if !sawBody.Stream {
		t.Error("completions request stream = false, want true for a tool-carrying turn")
	}
	if _, ok := sawKeys["params"]; ok {
		t.Errorf(`completions request has a "params" key = %s, want it entirely absent`, sawBody.Params)
	}
}

func TestClient_ContinueTurn_WebSearchConfigured_SendsStreamTrueNoParams(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawBody struct {
		Stream bool `json:"stream"`
	}
	sawKeys := map[string]json.RawMessage{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &sawBody); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			if err := json.Unmarshal(body, &sawKeys); err != nil {
				t.Fatalf("decode completions request keys: %v", err)
			}
			writeJSON(t, w, []byte(`null`), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	req.WebSearchEnabled = true
	if _, err := client.ContinueTurn(t.Context(), req); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if !sawBody.Stream {
		t.Error("completions request stream = false, want true for a tool-carrying turn")
	}
	if _, ok := sawKeys["params"]; ok {
		t.Error(`completions request has a "params" key, want it entirely absent`)
	}
}

// TestClient_ContinueTurn_NativeMode_PollsUntilDoneAcrossMultipleChecks is
// awaitTurnDone's core assertion (ADR-0005 D27, Issue #123): a native
// turn's completion is not known after one GET the way a legacy turn's
// is — it must be polled — so this pins that a not-yet-done response does
// not fail or return early, and a later done:true response is what
// success is built from.
func TestClient_ContinueTurn_NativeMode_PollsUntilDoneAcrossMultipleChecks(t *testing.T) {
	const assistantID = "assistant-1"
	notDone := chatGetBody(t, assistantID, map[string]any{"done": false, "content": ""}, assistantID)
	done := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "the answer"}, assistantID)

	getCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case http.MethodGet:
			getCalls++
			if getCalls < 2 {
				writeJSON(t, w, notDone, http.StatusOK)
				return
			}
			writeJSON(t, w, done, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) { cfg.ToolTurnTimeout = 4 * time.Second })

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	req.ToolIDs = []string{"calculator"}
	result, err := client.ContinueTurn(t.Context(), req)
	if err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if result.Content != "the answer" {
		t.Errorf("Content = %q, want %q", result.Content, "the answer")
	}
	if getCalls < 2 {
		t.Errorf("GET calls = %d, want at least 2 (awaitTurnDone must have polled rather than trusting the first not-done response)", getCalls)
	}
}

// TestClient_ContinueTurn_NativeMode_PollTimeoutBecomesAmbiguous confirms
// ADR-0005 D25's withdrawal (D27): a native turn that never reaches
// done/error within ToolTurnTimeout ends CategoryAmbiguous — D6's
// ordinary "resolve it later from a GET" case, the same as any other
// chat-managed turn whose completion is unconfirmed — never a distinct
// timeout error and never a fabricated success.
func TestClient_ContinueTurn_NativeMode_PollTimeoutBecomesAmbiguous(t *testing.T) {
	const assistantID = "assistant-1"
	notDone := chatGetBody(t, assistantID, map[string]any{"done": false, "content": ""}, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, notDone, http.StatusOK)
		}
	}))
	defer server.Close()
	// Shorter than nativeTurnPollInterval, so awaitTurnDone's poll budget
	// is exhausted on the very first tick's select — this must resolve
	// quickly rather than requiring a real multi-second wait to observe.
	client := newTestClient(t, server, func(cfg *Config) { cfg.ToolTurnTimeout = 20 * time.Millisecond })

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	req.WebSearchEnabled = true
	_, err := client.ContinueTurn(t.Context(), req)
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryAmbiguous {
		t.Errorf("Category = %q, want %q", pe.Category, openwebui.CategoryAmbiguous)
	}
}

// TestClient_ContinueTurn_NativeMode_HardLookupFailureStopsPollingImmediately
// confirms awaitTurnDone does not spend its polling budget re-asking a
// question a rejected credential can never answer differently.
func TestClient_ContinueTurn_NativeMode_HardLookupFailureStopsPollingImmediately(t *testing.T) {
	getCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case http.MethodGet:
			getCalls++
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	// Long enough that reaching the deadline (rather than the hard
	// failure returning immediately) would make this test visibly slow —
	// a regression here would show up as a multi-second test, not just a
	// wrong category.
	client := newTestClient(t, server, func(cfg *Config) { cfg.ToolTurnTimeout = 4 * time.Second })

	req := minimalContinueTurnReq("chat-1")
	req.ToolIDs = []string{"calculator"}
	_, err := client.ContinueTurn(t.Context(), req)
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryAuthFailed {
		t.Errorf("Category = %q, want %q", pe.Category, openwebui.CategoryAuthFailed)
	}
	if getCalls != 1 {
		t.Errorf("GET calls = %d, want exactly 1 (a rejected credential must not be retried)", getCalls)
	}
}

// TestClient_ContinueTurn_NativeMode_CompletionsPOSTUsesToolTurnTimeout
// covers Issue #126: a real-instance capture found the initiating
// completions POST for a native (tool-carrying) turn blocks until Open
// WebUI's native tool-calling loop fully finishes, not the "returns
// immediately" behavior ADR-0005 D27 assumed. Config.Timeout (5s from
// newTestClient) is deliberately shorter than how long the completions
// handler below sleeps, so this fails unless runTurn's native branch
// bounds that one call by ToolTurnTimeout instead.
func TestClient_ContinueTurn_NativeMode_CompletionsPOSTUsesToolTurnTimeout(t *testing.T) {
	const assistantID = "assistant-1"
	done := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "the answer"}, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			time.Sleep(300 * time.Millisecond)
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, done, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) {
		cfg.Timeout = 50 * time.Millisecond
		cfg.ToolTurnTimeout = 4 * time.Second
	})

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	req.ToolIDs = []string{"calculator"}
	result, err := client.ContinueTurn(t.Context(), req)
	if err != nil {
		t.Fatalf("ContinueTurn: %v (want the completions POST to survive the 300ms handler delay under ToolTurnTimeout, not fail against the 50ms Timeout)", err)
	}
	if result.Content != "the answer" {
		t.Errorf("Content = %q, want %q", result.Content, "the answer")
	}
}

// TestClient_ContinueTurn_PlainMode_CompletionsPOSTStillUsesShortTimeout
// is the regression guard for Issue #126's fix: a plain turn (neither
// tool_ids nor web_search resolved) must keep using the ordinary,
// shorter Config.Timeout for its completions POST — the fix is confined
// to runTurn's native branch, not every call through c.post.
func TestClient_ContinueTurn_PlainMode_CompletionsPOSTStillUsesShortTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			time.Sleep(150 * time.Millisecond)
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case http.MethodGet:
			done := chatGetBody(t, "assistant-1", map[string]any{"done": true, "content": "ok"}, "assistant-1")
			writeJSON(t, w, done, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, func(cfg *Config) {
		cfg.Timeout = 30 * time.Millisecond
		cfg.ToolTurnTimeout = 4 * time.Second
	})

	req := minimalContinueTurnReq("chat-1")
	_, err := client.ContinueTurn(t.Context(), req)
	pe := requireProviderError(t, err)
	if pe.Category != openwebui.CategoryTimeout {
		t.Errorf("Category = %q, want %q (a plain turn's completions POST must still be bounded by the short Timeout, not ToolTurnTimeout)", pe.Category, openwebui.CategoryTimeout)
	}
}

// --- Issues #81/#84: citation sources and chat title ---
//
// UNVERIFIED ASSUMPTION (2026-09-08, no real-instance access — ADR-0005
// D22): the multi-source shapes these tests exercise are hand-authored
// synthetic fixtures (completions_response_sources_tool.json,
// completions_response_sources_websearch.json), not a real capture. The
// real 2026-09-07 capture behind Issue #81 observed exactly one source;
// these tests pin normalizeSources' own documented 1:1-array-order
// mapping, not a confirmed real-instance contract for more than one
// source. See openwebui.Source's own doc comment.
//
// NOT NATIVE-MODE COVERAGE (ADR-0005 D27, Issue #123, self-review
// finding, 2026-09-08): none of the tests below set ToolIDs or
// WebSearchEnabled on their request, so each exercises runTurn's plain
// (Stream: false) branch with a hand-authored, sources-bearing POST
// response body. That combination — a real sources[] array on a
// completions response — can no longer occur for an actual native
// (tool-carrying) turn post-D27: that response is always empty (see
// completionsResponseBody.Sources' own doc comment). These tests still
// correctly pin normalizeSources'/decodeSources' own decoding logic, but
// they do not demonstrate that TurnResult.Sources is ever non-nil for a
// turn that really called a tool or searched the web under the current
// design — see ADR-0005 D27's "self-review gap" paragraph.

// TestClient_ContinueTurn_NormalizesToolSourceFromFixture backs the
// tool-execution sources[] shape: source.name becomes DisplayName, the
// first metadata entry's parameters become Arguments (stringified), and
// Kind is SourceKindTool.
func TestClient_ContinueTurn_NormalizesToolSourceFromFixture(t *testing.T) {
	const assistantID = "assistant-1"
	turnResp := loadFixture(t, "completions_response_sources_tool.json")
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, turnResp, http.StatusOK)
		case http.MethodGet:
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
	if len(result.Sources) != 1 {
		t.Fatalf("Sources = %+v, want exactly 1", result.Sources)
	}
	src := result.Sources[0]
	if src.Kind != openwebui.SourceKindTool {
		t.Errorf("Kind = %q, want %q", src.Kind, openwebui.SourceKindTool)
	}
	if src.DisplayName != "get_weather" {
		t.Errorf("DisplayName = %q, want %q", src.DisplayName, "get_weather")
	}
	if src.URL != nil {
		t.Errorf("URL = %v, want nil for a tool source", src.URL)
	}
	if src.Arguments["city"] != "Tokyo" {
		t.Errorf("Arguments[city] = %q, want %q", src.Arguments["city"], "Tokyo")
	}
}

// TestClient_ContinueTurn_NormalizesWebSearchSourceFromFixture backs the
// web-search sources[] shape: the first metadata entry's own "source"
// field becomes URL, and — the documented under-representation risk
// (openwebui.Source's own doc comment) — a second metadata entry (a
// further result chunk) is not represented at all.
func TestClient_ContinueTurn_NormalizesWebSearchSourceFromFixture(t *testing.T) {
	const assistantID = "assistant-1"
	turnResp := loadFixture(t, "completions_response_sources_websearch.json")
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, turnResp, http.StatusOK)
		case http.MethodGet:
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
	if len(result.Sources) != 1 {
		t.Fatalf("Sources = %+v, want exactly 1 (one sources[] element, regardless of its 2 metadata chunks)", result.Sources)
	}
	src := result.Sources[0]
	if src.Kind != openwebui.SourceKindWebSearch {
		t.Errorf("Kind = %q, want %q", src.Kind, openwebui.SourceKindWebSearch)
	}
	if src.URL == nil || *src.URL != "https://go.dev/doc/go1.24" {
		t.Errorf("URL = %v, want the first metadata entry's source, not the second chunk's go.dev/issue/66821", src.URL)
	}
	if src.Arguments != nil {
		t.Errorf("Arguments = %v, want nil for a web-search source", src.Arguments)
	}
}

// TestClient_ContinueTurn_SourcesDocumentNeverCaptured is the
// security-regression pin for ADR-0005 D22's core guarantee: both
// fixtures' document[] entries carry a distinctive marker string, and
// none of it may reach TurnResult in any field — not Content, not any
// Source field — because wireSource/wireSourceMetadata have no field
// that could ever decode it in the first place.
func TestClient_ContinueTurn_SourcesDocumentNeverCaptured(t *testing.T) {
	const documentMarker = "temperature\": 22"
	const assistantID = "assistant-1"
	for _, fixture := range []string{
		"completions_response_sources_tool.json",
		"completions_response_sources_websearch.json",
	} {
		t.Run(fixture, func(t *testing.T) {
			turnResp := loadFixture(t, fixture)
			if !strings.Contains(string(turnResp), "document") {
				t.Fatalf("fixture %s has no document[] field to test against", fixture)
			}
			getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					writeJSON(t, w, turnResp, http.StatusOK)
				case http.MethodGet:
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
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal TurnResult: %v", err)
			}
			for _, marker := range []string{"document", "page-text", "crypto/rand.Read never returns"} {
				if strings.Contains(string(encoded), marker) {
					t.Errorf("TurnResult = %s, must never contain document[]-derived text (%q)", encoded, marker)
				}
			}
		})
	}
}

// TestClient_ContinueTurn_NormalizeSourcesBoundsFieldLength is the
// security-regression pin for the other half of D22's defense-in-depth:
// an oversized DisplayName/URL/Arguments value (untrusted tool/provider
// output) is truncated to maxSourceFieldLen, never copied through
// unbounded.
func TestClient_ContinueTurn_NormalizeSourcesBoundsFieldLength(t *testing.T) {
	longName := strings.Repeat("a", maxSourceFieldLen+50)
	longURL := "https://example.com/" + strings.Repeat("b", maxSourceFieldLen+50)
	longArg := strings.Repeat("c", maxSourceFieldLen+50)

	turnRespBody, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}, "finish_reason": "stop"}},
		"sources": []any{
			map[string]any{
				"source":      map[string]any{"name": longName},
				"tool_result": true,
				"metadata":    []any{map[string]any{"parameters": map[string]any{"query": longArg}}},
			},
			map[string]any{
				"source":   map[string]any{"name": "web_search"},
				"metadata": []any{map[string]any{"source": longURL}},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal synthetic completions response: %v", err)
	}

	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, turnRespBody, http.StatusOK)
		case http.MethodGet:
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
	if len(result.Sources) != 2 {
		t.Fatalf("Sources = %+v, want 2", result.Sources)
	}
	if n := len(result.Sources[0].DisplayName); n != maxSourceFieldLen {
		t.Errorf("Sources[0].DisplayName length = %d, want %d", n, maxSourceFieldLen)
	}
	if n := len(result.Sources[0].Arguments["query"]); n != maxSourceFieldLen {
		t.Errorf("Sources[0].Arguments[query] length = %d, want %d", n, maxSourceFieldLen)
	}
	if result.Sources[1].URL == nil || len(*result.Sources[1].URL) != maxSourceFieldLen {
		t.Errorf("Sources[1].URL length = %v, want %d", result.Sources[1].URL, maxSourceFieldLen)
	}
}

// TestClient_ContinueTurn_MalformedSourcesNeverFailsTheTurn is the
// robustness half of Issue #81's own test requirement (plan §7.8): a
// sources[] shape this adapter cannot parse (schema drift, or simply a
// provider bug) must degrade to "no citations for this reply," never to
// a failed turn — the turn's actual content already decoded fine, and
// citations are enrichment, not core to success. Before decodeSources
// existed, completionsResponseBody decoded Sources as a typed
// []wireSource field directly, so a single malformed element failed the
// entire response decode (contract_failed) even though Choices/content
// were perfectly fine; this pins the fix.
func TestClient_ContinueTurn_MalformedSourcesNeverFailsTheTurn(t *testing.T) {
	turnRespBody, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}, "finish_reason": "stop"}},
		// "source" is a string here, not the expected object — a shape
		// this adapter has never observed for real.
		"sources": []any{map[string]any{"source": "not-an-object", "tool_result": true}},
	})
	if err != nil {
		t.Fatalf("marshal synthetic completions response: %v", err)
	}

	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, turnRespBody, http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, getResp, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	result, err := client.ContinueTurn(t.Context(), req)
	if err != nil {
		t.Fatalf("ContinueTurn returned an error for a malformed sources[] shape, want the turn to still succeed: %v", err)
	}
	if result.Content != "ok" {
		t.Errorf("Content = %q, want %q", result.Content, "ok")
	}
	if result.Sources != nil {
		t.Errorf("Sources = %+v, want nil for an unparseable shape", result.Sources)
	}
}

// --- ADR-0005 D28 (Issue #127): output[] citation recovery ---

// TestClient_LookupTurnOutcome_NormalizesToolSourceFromOutput backs
// ADR-0005 D28: a native-mode assistant message's output[] carries a
// function_call/function_call_output pair sharing one call_id, and
// LookupTurnOutcome normalizes it into exactly the shape a legacy-path
// tool source already has (DisplayName from the call's own name,
// Arguments from its own arguments) — the reasoning and message items
// alongside it, present the way a real capture would have them, decode
// but contribute nothing.
func TestClient_LookupTurnOutcome_NormalizesToolSourceFromOutput(t *testing.T) {
	const assistantID = "assistant-1"
	message := map[string]any{
		"done":    true,
		"content": "the weather in Tokyo is sunny",
		"output": []any{
			map[string]any{"type": "reasoning", "content": []any{}, "encrypted_content": "opaque"},
			map[string]any{
				"type": "function_call", "name": "search_notes_search_get",
				"call_id": "call-1", "arguments": map[string]any{"query": "weather", "max_results": 20},
				"status": "completed",
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call-1",
				"output": []any{map[string]any{"type": "input_text", "text": `{"hits":[]}`}}, "status": "completed",
			},
			map[string]any{"type": "message", "role": "assistant", "content": []any{}, "status": "completed"},
		},
	}
	getResp := chatGetBody(t, assistantID, message, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", assistantID)
	if err != nil {
		t.Fatalf("LookupTurnOutcome: %v", err)
	}
	if len(outcome.Sources) != 1 {
		t.Fatalf("Sources = %+v, want exactly 1", outcome.Sources)
	}
	src := outcome.Sources[0]
	if src.Kind != openwebui.SourceKindTool {
		t.Errorf("Kind = %q, want %q", src.Kind, openwebui.SourceKindTool)
	}
	if src.DisplayName != "search_notes_search_get" {
		t.Errorf("DisplayName = %q, want %q", src.DisplayName, "search_notes_search_get")
	}
	if src.Arguments["query"] != "weather" {
		t.Errorf("Arguments[query] = %q, want %q", src.Arguments["query"], "weather")
	}
}

// TestClient_LookupTurnOutcome_OutputArgumentsAsJSONString backs
// functionCallArguments' other tolerated shape: OpenAI's own Responses
// API documents function_call.arguments as a JSON-encoded string, not a
// bare object — Issue #127's own write-up rendered it unquoted, which may
// just be the issue text's own formatting rather than the literal wire
// shape, so both are accepted.
func TestClient_LookupTurnOutcome_OutputArgumentsAsJSONString(t *testing.T) {
	const assistantID = "assistant-1"
	message := map[string]any{
		"done": true, "content": "ok",
		"output": []any{
			map[string]any{"type": "function_call", "name": "get_weather", "call_id": "call-1", "arguments": `{"city":"Tokyo"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "irrelevant"},
		},
	}
	getResp := chatGetBody(t, assistantID, message, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", assistantID)
	if err != nil {
		t.Fatalf("LookupTurnOutcome: %v", err)
	}
	if len(outcome.Sources) != 1 || outcome.Sources[0].Arguments["city"] != "Tokyo" {
		t.Fatalf("Sources = %+v, want one entry with Arguments[city] = Tokyo", outcome.Sources)
	}
}

// TestClient_LookupTurnOutcome_OutputFunctionCallWithoutMatchingOutput_NotSurfaced
// backs the "a tool round that actually completed" gate (ADR-0005 D28): a
// function_call with no function_call_output sharing its call_id — still
// in flight, or abandoned mid-turn — is never surfaced as a citation.
func TestClient_LookupTurnOutcome_OutputFunctionCallWithoutMatchingOutput_NotSurfaced(t *testing.T) {
	const assistantID = "assistant-1"
	message := map[string]any{
		"done": true, "content": "ok",
		"output": []any{
			map[string]any{"type": "function_call", "name": "get_weather", "call_id": "call-1", "arguments": map[string]any{}},
		},
	}
	getResp := chatGetBody(t, assistantID, message, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", assistantID)
	if err != nil {
		t.Fatalf("LookupTurnOutcome: %v", err)
	}
	if outcome.Sources != nil {
		t.Errorf("Sources = %+v, want nil for an unmatched function_call", outcome.Sources)
	}
}

// TestClient_LookupTurnOutcome_OutputMultipleRoundsPaired backs
// normalizeOutputSources' call_id pairing across more than one round —
// UNVERIFIED against a real multi-round native turn (ADR-0005 D28), but
// the pairing logic itself must not silently collapse to array position
// or lose a round.
func TestClient_LookupTurnOutcome_OutputMultipleRoundsPaired(t *testing.T) {
	const assistantID = "assistant-1"
	message := map[string]any{
		"done": true, "content": "ok",
		"output": []any{
			map[string]any{"type": "function_call", "name": "search", "call_id": "call-1", "arguments": map[string]any{"q": "a"}},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "x"},
			map[string]any{"type": "function_call", "name": "fetch", "call_id": "call-2", "arguments": map[string]any{"q": "b"}},
			map[string]any{"type": "function_call_output", "call_id": "call-2", "output": "y"},
		},
	}
	getResp := chatGetBody(t, assistantID, message, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", assistantID)
	if err != nil {
		t.Fatalf("LookupTurnOutcome: %v", err)
	}
	if len(outcome.Sources) != 2 {
		t.Fatalf("Sources = %+v, want exactly 2", outcome.Sources)
	}
	if outcome.Sources[0].DisplayName != "search" || outcome.Sources[1].DisplayName != "fetch" {
		t.Errorf("Sources = %+v, want [search, fetch] in order", outcome.Sources)
	}
}

// TestClient_LookupTurnOutcome_OutputNeverCapturesRawToolResultOrEncryptedContent
// is the security-regression pin for ADR-0005 D28's own extension of
// D22's "document[] is never decoded" guarantee to output[]: neither
// function_call_output's own raw result text nor a reasoning item's
// encrypted_content may reach TurnOutcome in any field, mirroring
// TestClient_ContinueTurn_SourcesDocumentNeverCaptured for sources[].
func TestClient_LookupTurnOutcome_OutputNeverCapturesRawToolResultOrEncryptedContent(t *testing.T) {
	const assistantID = "assistant-1"
	const toolResultMarker = "super-secret-tool-result-text"
	const encryptedMarker = "super-secret-encrypted-reasoning"
	message := map[string]any{
		"done": true, "content": "ok",
		"output": []any{
			map[string]any{"type": "reasoning", "encrypted_content": encryptedMarker},
			map[string]any{"type": "function_call", "name": "get_weather", "call_id": "call-1", "arguments": map[string]any{}},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": toolResultMarker},
		},
	}
	getResp := chatGetBody(t, assistantID, message, assistantID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", assistantID)
	if err != nil {
		t.Fatalf("LookupTurnOutcome: %v", err)
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("marshal TurnOutcome: %v", err)
	}
	for _, marker := range []string{toolResultMarker, encryptedMarker} {
		if strings.Contains(string(encoded), marker) {
			t.Errorf("TurnOutcome = %s, must never contain output[]-derived raw text (%q)", encoded, marker)
		}
	}
}

// TestClient_ContinueTurn_NativeModeRecoversSourcesFromOutput is the
// end-to-end pin for ADR-0005 D28's actual fix: a native turn's
// completions POST body is always empty (D27), so runTurn's own
// parsed.Sources is always nil for it — this confirms runTurn falls back
// to the GET confirmation's own output[]-derived Sources instead, rather
// than leaving citations lost the way D27's self-review gap originally
// found.
func TestClient_ContinueTurn_NativeModeRecoversSourcesFromOutput(t *testing.T) {
	const assistantID = "assistant-1"
	done := chatGetBody(t, assistantID, map[string]any{
		"done": true, "content": "the answer",
		"output": []any{
			map[string]any{"type": "function_call", "name": "get_weather", "call_id": "call-1", "arguments": map[string]any{"city": "Tokyo"}},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "irrelevant"},
		},
	}, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, []byte("null"), http.StatusOK)
		case http.MethodGet:
			writeJSON(t, w, done, http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalContinueTurnReq("chat-1")
	req.IDs.AssistantMessageID = assistantID
	req.ToolIDs = []string{"calculator"}
	result, err := client.ContinueTurn(t.Context(), req)
	if err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("Sources = %+v, want exactly 1 recovered from output[]", result.Sources)
	}
	if result.Sources[0].DisplayName != "get_weather" {
		t.Errorf("DisplayName = %q, want %q", result.Sources[0].DisplayName, "get_weather")
	}
}

// TestClient_ContinueTurn_LegacySourcesTakePriorityOverOutput confirms
// the output[] fallback runs only when the completions response's own
// sources[] came back empty: a legacy-path turn whose POST body already
// carries sources[] (D22) is unaffected by whatever output[] the GET path
// also happens to carry.
func TestClient_ContinueTurn_LegacySourcesTakePriorityOverOutput(t *testing.T) {
	const assistantID = "assistant-1"
	turnResp := loadFixture(t, "completions_response_sources_tool.json")
	getResp := chatGetBody(t, assistantID, map[string]any{
		"done": true, "content": "ok",
		"output": []any{
			map[string]any{"type": "function_call", "name": "other_tool", "call_id": "call-1", "arguments": map[string]any{}},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "x"},
		},
	}, assistantID)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, turnResp, http.StatusOK)
		case http.MethodGet:
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
	if len(result.Sources) != 1 || result.Sources[0].DisplayName != "get_weather" {
		t.Fatalf("Sources = %+v, want the legacy fixture's get_weather source, not output[]'s other_tool", result.Sources)
	}
}

// --- Issue #84: chat title ---

// TestClient_StartChat_EnableTitleGenerationSendsTitleGenerationTrue
// backs StartChatRequest.EnableTitleGeneration: only when it is set does
// the first turn's completions request carry
// background_tasks.title_generation: true.
func TestClient_StartChat_EnableTitleGenerationSendsTitleGenerationTrue(t *testing.T) {
	const chatID = "chat-1"
	createResp, err := json.Marshal(map[string]any{"id": chatID})
	if err != nil {
		t.Fatalf("marshal chats/new response: %v", err)
	}
	getResp := chatGetBody(t, "assistant-1", map[string]any{"done": true, "content": "ok"}, "assistant-1")

	var sawBackgroundTasks struct {
		TitleGeneration bool `json:"title_generation"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/chats/new", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, createResp, http.StatusOK)
	})
	mux.HandleFunc("POST /api/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BackgroundTasks struct {
				TitleGeneration bool `json:"title_generation"`
			} `json:"background_tasks"`
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("decode completions request: %v", err)
		}
		sawBackgroundTasks.TitleGeneration = body.BackgroundTasks.TitleGeneration
		writeJSON(t, w, []byte(`{}`), http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/chats/"+chatID, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, getResp, http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, server, nil)

	req := minimalStartChatReq()
	req.EnableTitleGeneration = true
	if _, err := client.StartChat(t.Context(), req); err != nil {
		t.Fatalf("StartChat: %v", err)
	}
	if !sawBackgroundTasks.TitleGeneration {
		t.Error("completions request background_tasks.title_generation = false, want true when EnableTitleGeneration is set")
	}
}

// TestClient_ContinueTurn_NeverSendsTitleGeneration backs
// ContinueTurn's own doc comment: title generation is never requested on
// a continuation, since ContinueTurnRequest has no field for it at all —
// this only re-confirms the request body's own zero value, since there
// is no way to even ask ContinueTurn for the opposite.
func TestClient_ContinueTurn_NeverSendsTitleGeneration(t *testing.T) {
	const assistantID = "assistant-1"
	getResp := chatGetBody(t, assistantID, map[string]any{"done": true, "content": "ok"}, assistantID)

	var sawBackgroundTasks struct {
		TitleGeneration bool `json:"title_generation"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			data, _ := io.ReadAll(r.Body)
			var body struct {
				BackgroundTasks struct {
					TitleGeneration bool `json:"title_generation"`
				} `json:"background_tasks"`
			}
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatalf("decode completions request: %v", err)
			}
			sawBackgroundTasks.TitleGeneration = body.BackgroundTasks.TitleGeneration
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
	if sawBackgroundTasks.TitleGeneration {
		t.Error("continuation's background_tasks.title_generation = true, want always false")
	}
}

// TestClient_LookupTurnOutcome_TitleFiltersPrecreatedPlaceholder backs
// resolvedTitle: the "bridge-precreated" placeholder createChat itself
// sets is never surfaced as a real title, but any other non-empty value
// is — including, per the documented 要実機確認 gap, a value that is
// merely the raw first user message rather than an actually-summarized
// title (this adapter cannot tell the two apart; see ADR-0005 D23).
func TestClient_LookupTurnOutcome_TitleFiltersPrecreatedPlaceholder(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		wantTitle *string
	}{
		{name: "placeholder is filtered", title: "bridge-precreated", wantTitle: nil},
		{name: "empty is filtered", title: "", wantTitle: nil},
		{name: "a real title passes through", title: "Weekend trip planning", wantTitle: strPtrForTest("Weekend trip planning")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{
				"title": tt.title,
				"chat":  map[string]any{"history": map[string]any{"messages": map[string]any{}, "currentId": nil}},
			})
			if err != nil {
				t.Fatalf("marshal chat response: %v", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, data, http.StatusOK)
			}))
			defer server.Close()
			client := newTestClient(t, server, nil)

			outcome, err := client.LookupTurnOutcome(t.Context(), "chat-1", "assistant-1")
			if err != nil {
				t.Fatalf("LookupTurnOutcome: %v", err)
			}
			if (outcome.Title == nil) != (tt.wantTitle == nil) {
				t.Fatalf("Title = %v, want %v", outcome.Title, tt.wantTitle)
			}
			if tt.wantTitle != nil && *outcome.Title != *tt.wantTitle {
				t.Errorf("Title = %q, want %q", *outcome.Title, *tt.wantTitle)
			}
		})
	}
}

func strPtrForTest(s string) *string { return &s }

// --- Issue #74/#75: tool/model resolution ---

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
// openwebui.RemoteModel carrying id, name, the arena signal, and its own
// info.meta.toolIds/defaultFeatureIds — including the built-in
// arena-model entry, which ListModels does not filter (that is
// Registry's eligibleRemoteModels' job, not this adapter's).
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
// decode-failure classification every other call against this endpoint
// uses.
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
		BaseURL:         "http://evil.example",
		AllowedOrigins:  []string{"https://openwebui.example.net"},
		APIKey:          testAPIKey,
		Timeout:         time.Second,
		ToolTurnTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("NewClient with a base URL outside the allowlist did not error")
	}
}

// TestNewClient_RejectsNonPositiveTimeouts is Issue #116's own regression
// case: a zero-value Timeout or ToolTurnTimeout makes context.WithTimeout
// return an already-expired context, so every call this Client makes (a
// native turn's awaitTurnDone poll within microseconds, never reaching
// the network) would fail instantly and silently instead of NewClient
// refusing to build the Client at all.
func TestNewClient_RejectsNonPositiveTimeouts(t *testing.T) {
	base := Config{
		BaseURL:         "https://openwebui.example.net",
		AllowedOrigins:  []string{"https://openwebui.example.net"},
		APIKey:          testAPIKey,
		Timeout:         time.Second,
		ToolTurnTimeout: time.Second,
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"zero Timeout", func(c *Config) { c.Timeout = 0 }, "Timeout"},
		{"negative Timeout", func(c *Config) { c.Timeout = -time.Second }, "Timeout"},
		{"zero ToolTurnTimeout", func(c *Config) { c.ToolTurnTimeout = 0 }, "ToolTurnTimeout"},
		{"negative ToolTurnTimeout", func(c *Config) { c.ToolTurnTimeout = -time.Second }, "ToolTurnTimeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			_, err := NewClient(cfg)
			if err == nil {
				t.Fatalf("NewClient(%+v) did not error", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewClient(%+v) error = %q, want it to mention %q", cfg, err, tt.wantErr)
			}
		})
	}
}

// TestConfigFrom_MapsEveryField is Issue #116's own regression case: the
// bug was that three separate call sites (cmd/server's catalog and turn
// clients, cmd/openwebuictl's confirm subcommand) each built a Config
// inline from config.OpenWebUIConfig, and two of them silently dropped
// ToolTurnTimeout. Routing every call site through ConfigFrom instead
// makes this the one place such a mapping mistake can happen — and this
// test the one place it gets caught, for every field, not just the one
// Issue #116 already found.
func TestConfigFrom_MapsEveryField(t *testing.T) {
	oc := config.OpenWebUIConfig{
		BaseURL:          "https://openwebui.example.net",
		AllowedOrigins:   []string{"https://openwebui.example.net", "https://other.example.net"},
		APIKey:           testAPIKey,
		Timeout:          7 * time.Second,
		ToolTurnTimeout:  11 * time.Minute,
		MaxResponseBytes: 1 << 21,
		MaxRequestBytes:  1 << 19,
	}
	got := ConfigFrom(oc)
	want := Config{
		BaseURL:          oc.BaseURL,
		AllowedOrigins:   oc.AllowedOrigins,
		APIKey:           oc.APIKey,
		Timeout:          oc.Timeout,
		ToolTurnTimeout:  oc.ToolTurnTimeout,
		MaxResponseBytes: oc.MaxResponseBytes,
		MaxRequestBytes:  oc.MaxRequestBytes,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ConfigFrom(%+v) = %+v, want %+v", oc, got, want)
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
