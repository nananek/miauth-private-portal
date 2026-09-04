package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/llmclassify"
	"github.com/nananek/miauth-private-portal/internal/llmreply"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-key", "test-model", 5*time.Second)
}

func TestClient_Complete_Success(t *testing.T) {
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got, want := r.URL.Path, "/chat/completions"; got != want {
			t.Errorf("path = %s, want %s", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer test-key"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})

	result, err := client.Complete(t.Context(), llmreply.CompletionRequest{
		Messages:        []llmreply.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}},
		MaxOutputTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Content != "hello there" {
		t.Errorf("Content = %q, want %q", result.Content, "hello there")
	}
	if result.PromptTokens == nil || *result.PromptTokens != 10 {
		t.Errorf("PromptTokens = %v, want 10", result.PromptTokens)
	}
	if result.CompletionTokens == nil || *result.CompletionTokens != 5 {
		t.Errorf("CompletionTokens = %v, want 5", result.CompletionTokens)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("request model = %v, want test-model", gotBody["model"])
	}
	if gotBody["max_tokens"] != float64(100) {
		t.Errorf("request max_tokens = %v, want 100", gotBody["max_tokens"])
	}
}

func TestClient_Complete_OmitsAuthorizationWhenAPIKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization header set = %q, want none", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "", "model", 5*time.Second)

	if _, err := client.Complete(t.Context(), llmreply.CompletionRequest{Messages: []llmreply.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestClient_Complete_ClassifiesErrors(t *testing.T) {
	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantCategory llmreply.Category
	}{
		{
			name: "unauthorized",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			wantCategory: llmreply.CategoryAuth,
		},
		{
			name: "forbidden",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantCategory: llmreply.CategoryAuth,
		},
		{
			name: "rate limited",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantCategory: llmreply.CategoryRateLimit,
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantCategory: llmreply.CategoryServerError,
		},
		{
			name: "bad gateway",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			},
			wantCategory: llmreply.CategoryServerError,
		},
		{
			name: "bad request",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			},
			wantCategory: llmreply.CategoryClientError,
		},
		{
			name: "not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantCategory: llmreply.CategoryClientError,
		},
		{
			name: "malformed json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{not-json`))
			},
			wantCategory: llmreply.CategoryMalformedResponse,
		},
		{
			name: "no choices",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[]}`))
			},
			wantCategory: llmreply.CategoryMalformedResponse,
		},
		{
			name: "empty content",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`))
			},
			wantCategory: llmreply.CategoryMalformedResponse,
		},
		{
			name: "content filter finish reason",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`))
			},
			wantCategory: llmreply.CategoryContentRefusal,
		},
		{
			name: "refusal field",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","refusal":"I cannot help with that"},"finish_reason":"stop"}]}`))
			},
			wantCategory: llmreply.CategoryContentRefusal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, tt.handler)
			_, err := client.Complete(t.Context(), llmreply.CompletionRequest{Messages: []llmreply.Message{{Role: "user", Content: "hi"}}})
			if err == nil {
				t.Fatal("Complete() error = nil, want an error")
			}
			if got := llmreply.ClassifyProviderError(err); got != tt.wantCategory {
				t.Errorf("ClassifyProviderError() = %q, want %q", got, tt.wantCategory)
			}
		})
	}
}

func TestClient_Complete_TimeoutIsClassifiedAsTimeout(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"late"},"finish_reason":"stop"}]}`))
	})
	client.httpClient.Timeout = 10 * time.Millisecond

	_, err := client.Complete(t.Context(), llmreply.CompletionRequest{Messages: []llmreply.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete() error = nil, want a timeout error")
	}
	if got := llmreply.ClassifyProviderError(err); got != llmreply.CategoryTimeout {
		t.Errorf("ClassifyProviderError() = %q, want %q", got, llmreply.CategoryTimeout)
	}
}

func TestClient_Complete_ContextCancelIsClassifiedAsTimeout(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := client.Complete(ctx, llmreply.CompletionRequest{Messages: []llmreply.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	if got := llmreply.ClassifyProviderError(err); got != llmreply.CategoryTimeout {
		t.Errorf("ClassifyProviderError() = %q, want %q", got, llmreply.CategoryTimeout)
	}
}

func TestClient_Complete_TransportErrorForUnreachableHost(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "key", "model", 2*time.Second)
	_, err := client.Complete(t.Context(), llmreply.CompletionRequest{Messages: []llmreply.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	got := llmreply.ClassifyProviderError(err)
	if got != llmreply.CategoryTransport && got != llmreply.CategoryTimeout {
		t.Errorf("ClassifyProviderError() = %q, want transport or timeout", got)
	}
}

// TestClient_CompleteForClassification_Success backs the doComplete
// sharing decision (internal/llmclassify's plan §3): the classification
// path must parse the same response envelope and report the same
// usage/content Complete does, just wrapped in llmclassify's own result
// type.
func TestClient_CompleteForClassification_Success(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"subject\":\"x\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8}}`))
	})
	adapter := NewClassificationClient(client)

	result, err := adapter.Complete(t.Context(), llmclassify.CompletionRequest{
		Messages:        []llmclassify.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}},
		MaxOutputTokens: 100,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Content != `{"subject":"x"}` {
		t.Errorf("Content = %q, want %q", result.Content, `{"subject":"x"}`)
	}
	if result.PromptTokens == nil || *result.PromptTokens != 20 {
		t.Errorf("PromptTokens = %v, want 20", result.PromptTokens)
	}
	if result.CompletionTokens == nil || *result.CompletionTokens != 8 {
		t.Errorf("CompletionTokens = %v, want 8", result.CompletionTokens)
	}
}

// TestClient_CompleteForClassification_ClassifiesErrors spot-checks that
// doComplete's shared status/malformed-response classification reaches
// llmclassify's own Category values, not just llmreply's.
func TestClient_CompleteForClassification_ClassifiesErrors(t *testing.T) {
	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantCategory llmclassify.Category
	}{
		{
			name:         "unauthorized",
			handler:      func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			wantCategory: llmclassify.CategoryAuth,
		},
		{
			name: "malformed json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{not-json`))
			},
			wantCategory: llmclassify.CategoryMalformedResponse,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := NewClassificationClient(newTestClient(t, tt.handler))
			_, err := adapter.Complete(t.Context(), llmclassify.CompletionRequest{Messages: []llmclassify.Message{{Role: "user", Content: "hi"}}})
			if err == nil {
				t.Fatal("Complete() error = nil, want an error")
			}
			if got := llmclassify.ClassifyProviderError(err); got != tt.wantCategory {
				t.Errorf("ClassifyProviderError() = %q, want %q", got, tt.wantCategory)
			}
		})
	}
}

func TestClient_Complete_ResponseBodyIsBounded(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"padding":"`))
		buf := make([]byte, maxResponseBytes*2)
		for i := range buf {
			buf[i] = 'a'
		}
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte(`"}`))
	})

	_, err := client.Complete(t.Context(), llmreply.CompletionRequest{Messages: []llmreply.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete() expected a decode error for an oversized, truncated body, got nil")
	}
	if got := llmreply.ClassifyProviderError(err); got != llmreply.CategoryMalformedResponse {
		t.Errorf("ClassifyProviderError() = %q, want %q", got, llmreply.CategoryMalformedResponse)
	}
}
