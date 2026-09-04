// Package openai implements an OpenAI-compatible chat-completions client
// for internal/llmreply.Provider (Issue #9). It is deliberately generic
// over baseURL/apiKey/model, following internal/provider/misskey's
// "narrow adapter behind a domain-owned interface" pattern (AGENTS.md:
// "put provider boundaries behind narrow interfaces"), so a future
// provider that also speaks the OpenAI-compatible chat-completions
// protocol (docs/roadmap/openwebui.md notes "Shared provider code with
// #9 may be reused") can reuse this client rather than duplicating it.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxResponseBytes bounds how much of the upstream response this client
// will read, so a misbehaving or oversized response cannot make this
// service buffer unbounded memory (AGENTS.md: "bound request sizes,
// timeouts, concurrency, and retry counts").
const maxResponseBytes = 4 << 20 // 4 MiB

// Category classifies a Complete failure so callers (internal/llmreply's
// job handler) can decide retryable vs. permanent and record
// LLMGeneration.ErrorCategory without ever needing the underlying error
// text, which may echo request content back from the upstream.
type Category string

const (
	// CategoryTransport is a network-level failure (DNS, connection
	// refused, connection reset) below the HTTP response layer.
	CategoryTransport Category = "transport"
	// CategoryTimeout is a request that exceeded its deadline.
	CategoryTimeout Category = "timeout"
	// CategoryAuth is a 401/403 response: the configured API key is
	// missing, invalid, or lacks access to the configured model.
	CategoryAuth Category = "auth"
	// CategoryRateLimit is a 429 response.
	CategoryRateLimit Category = "rate_limit"
	// CategoryServerError is a 5xx response.
	CategoryServerError Category = "server_error"
	// CategoryClientError is any other non-2xx response (a malformed
	// request, an unknown model, ...). Retrying the same request is not
	// expected to help.
	CategoryClientError Category = "client_error"
	// CategoryMalformedResponse is a 2xx response this client could not
	// decode into the documented shape (invalid JSON, no choices, an
	// empty message).
	CategoryMalformedResponse Category = "malformed_response"
	// CategoryContentRefusal is a 2xx response in which the model refused
	// to answer (finish_reason "content_filter" or a non-empty refusal
	// field). Retrying the identical request is not expected to help.
	CategoryContentRefusal Category = "content_refusal"
)

// Error wraps a classified Complete failure. Category is safe to log; the
// wrapped error text is not (AGENTS.md forbids logging LLM prompts or
// response bodies, and an upstream 4xx/5xx error body can echo request
// content back), so callers must log only Category, never Error() or
// Unwrap().
type Error struct {
	Category Category
	err      error
}

func (e *Error) Error() string { return fmt.Sprintf("openai: %s: %s", e.Category, e.err) }
func (e *Error) Unwrap() error { return e.err }

func classified(category Category, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Category: category, err: err}
}

// Message is one chat-completions message.
type Message struct {
	Role    string
	Content string
}

// CompletionRequest is one chat-completions call.
type CompletionRequest struct {
	Messages []Message
	// MaxOutputTokens bounds the completion length. Zero omits the
	// upstream max_tokens field, deferring to the provider's own default.
	MaxOutputTokens int
}

// CompletionResult is a successful generation. PromptTokens and
// CompletionTokens are nil when the upstream response omits usage, which
// some OpenAI-compatible servers do.
type CompletionResult struct {
	Content          string
	PromptTokens     *int
	CompletionTokens *int
}

// Client calls POST {baseURL}/chat/completions.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient builds a Client against baseURL (an OpenAI-compatible API
// base, commonly including a path such as "https://api.openai.com/v1"),
// bounding every request by timeout. An empty apiKey omits the
// Authorization header, for self-hosted providers that do not require
// one.
func NewClient(baseURL, apiKey, model string, timeout time.Duration) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: timeout},
	}
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionRequestBody struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type completionResponseBody struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete calls the chat-completions endpoint and returns the first
// choice. Every failure is classified through Category; see the Category
// constants for the retryable/permanent distinction internal/llmreply's
// job handler applies.
func (c *Client) Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	messages := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = wireMessage{Role: m.Role, Content: m.Content}
	}
	body, err := json.Marshal(completionRequestBody{
		Model:     c.model,
		Messages:  messages,
		MaxTokens: req.MaxOutputTokens,
	})
	if err != nil {
		return CompletionResult{}, classified(CategoryClientError, fmt.Errorf("openai: encode request: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompletionResult{}, classified(CategoryTransport, fmt.Errorf("openai: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return CompletionResult{}, classified(categorizeTransportError(ctx, err), fmt.Errorf("openai: request: %w", err))
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CompletionResult{}, classified(categorizeStatus(resp.StatusCode), fmt.Errorf("openai: status %d", resp.StatusCode))
	}

	var parsed completionResponseBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&parsed); err != nil {
		return CompletionResult{}, classified(CategoryMalformedResponse, fmt.Errorf("openai: decode response: %w", err))
	}
	if len(parsed.Choices) == 0 {
		return CompletionResult{}, classified(CategoryMalformedResponse, errors.New("openai: no choices in response"))
	}
	choice := parsed.Choices[0]
	if choice.FinishReason == "content_filter" || choice.Message.Refusal != "" {
		return CompletionResult{}, classified(CategoryContentRefusal, errors.New("openai: provider refused to generate content"))
	}
	if choice.Message.Content == "" {
		return CompletionResult{}, classified(CategoryMalformedResponse, errors.New("openai: empty content in response"))
	}

	return CompletionResult{
		Content:          choice.Message.Content,
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
	}, nil
}

// categorizeTransportError distinguishes a timeout (the caller-supplied
// ctx expiring, or http.Client.Timeout firing) from every other
// transport-level failure, so a caller can treat the two differently:
// a caller-driven cancellation is handled entirely by
// internal/jobs.Manager's own lease/shutdown logic, never a timeout the
// LLM job handler itself classifies for retry accounting, but both leave
// Go's http.Client returning the same net.Error shape.
func categorizeTransportError(ctx context.Context, err error) Category {
	if errors.Is(err, context.DeadlineExceeded) {
		return CategoryTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return CategoryTimeout
	}
	if ctx.Err() != nil {
		return CategoryTimeout
	}
	return CategoryTransport
}

func categorizeStatus(status int) Category {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return CategoryAuth
	case status == http.StatusTooManyRequests:
		return CategoryRateLimit
	case status >= 500:
		return CategoryServerError
	default:
		return CategoryClientError
	}
}

// drainAndClose gives net/http a chance to reuse a keep-alive connection
// after an early return (a non-2xx response or a decode error). The drain
// is bounded because the upstream response is untrusted.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes+1))
	_ = body.Close()
}
