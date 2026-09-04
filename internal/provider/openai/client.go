// Package openai implements internal/llmreply.Provider against a real
// OpenAI-compatible chat-completions endpoint. It is the outbound
// counterpart to internal/llmreply, exactly as internal/provider/misskey
// is to internal/miauth: a narrow adapter behind a use-case-owned
// interface (AGENTS.md: "put provider boundaries behind narrow
// interfaces"), so internal/llmreply never depends on net/http or a
// specific provider. Client is deliberately generic over
// baseURL/apiKey/model so a future provider that also speaks the
// OpenAI-compatible chat-completions protocol (docs/roadmap/openwebui.md
// notes "Shared provider code with #9 may be reused") can reuse it
// rather than duplicating this adapter.
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

	"github.com/nananek/miauth-private-portal/internal/llmclassify"
	"github.com/nananek/miauth-private-portal/internal/llmreply"
)

var _ llmreply.Provider = (*Client)(nil)

// maxResponseBytes bounds how much of the upstream response this client
// will read, so a misbehaving or oversized response cannot make this
// service buffer unbounded memory (AGENTS.md: "bound request sizes,
// timeouts, concurrency, and retry counts").
const maxResponseBytes = 4 << 20 // 4 MiB

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

// category is this client's own provider-neutral failure classification.
// Complete and CompleteForClassification each convert it into their own
// package's Category type (a plain string-to-string conversion, since
// both mirror the same eight values) right at the boundary where they
// wrap it into that package's ProviderError, so doComplete itself never
// depends on either use-case package's Category type.
type category string

const (
	categoryTransport         category = "transport"
	categoryTimeout           category = "timeout"
	categoryAuth              category = "auth"
	categoryRateLimit         category = "rate_limit"
	categoryServerError       category = "server_error"
	categoryClientError       category = "client_error"
	categoryMalformedResponse category = "malformed_response"
	categoryContentRefusal    category = "content_refusal"
)

// Complete implements llmreply.Provider. Every failure is wrapped with
// llmreply.NewProviderError; see its Category constants for the
// retryable/permanent distinction internal/llmreply's job handler
// applies. Neither the request body nor any upstream error/response body
// is ever included in a returned error's text beyond what
// llmreply.ProviderError.Error() itself composes from the Category alone
// (AGENTS.md: never log LLM prompts or response bodies — an upstream
// error body can echo request content back).
func (c *Client) Complete(ctx context.Context, req llmreply.CompletionRequest) (llmreply.CompletionResult, error) {
	messages := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = wireMessage{Role: m.Role, Content: m.Content}
	}
	content, promptTokens, completionTokens, cat, err := c.doComplete(ctx, messages, req.MaxOutputTokens)
	if err != nil {
		return llmreply.CompletionResult{}, llmreply.NewProviderError(llmreply.Category(cat), err)
	}
	return llmreply.CompletionResult{Content: content, PromptTokens: promptTokens, CompletionTokens: completionTokens}, nil
}

// ClassificationClient adapts a Client to llmclassify.Provider by calling
// CompleteForClassification. A separate type is needed here (rather than
// a direct llmclassify.Provider assertion on *Client itself) because Go
// does not allow one type to declare two methods named "Complete" with
// different signatures: Client.Complete already implements
// llmreply.Provider's shape. cmd/server constructs this alongside Client
// when LLM_CLASSIFICATION_ENABLED is set.
type ClassificationClient struct{ client *Client }

// NewClassificationClient wraps c for use as an llmclassify.Provider.
func NewClassificationClient(c *Client) ClassificationClient {
	return ClassificationClient{client: c}
}

var _ llmclassify.Provider = ClassificationClient{}

// Complete implements llmclassify.Provider by delegating to the wrapped
// Client's CompleteForClassification.
func (c ClassificationClient) Complete(ctx context.Context, req llmclassify.CompletionRequest) (llmclassify.CompletionResult, error) {
	return c.client.CompleteForClassification(ctx, req)
}

// CompleteForClassification implements llmclassify.Provider's actual
// wire call, sharing this client's wire handling (request assembly, HTTP
// call, response envelope parsing, status/timeout classification) with
// Complete through doComplete. See Complete's doc comment for the same
// never-log-prompts-or-bodies rule, which applies identically here.
func (c *Client) CompleteForClassification(ctx context.Context, req llmclassify.CompletionRequest) (llmclassify.CompletionResult, error) {
	messages := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = wireMessage{Role: m.Role, Content: m.Content}
	}
	content, promptTokens, completionTokens, cat, err := c.doComplete(ctx, messages, req.MaxOutputTokens)
	if err != nil {
		return llmclassify.CompletionResult{}, llmclassify.NewProviderError(llmclassify.Category(cat), err)
	}
	return llmclassify.CompletionResult{Content: content, PromptTokens: promptTokens, CompletionTokens: completionTokens}, nil
}

// doComplete is the shared implementation behind Complete and
// CompleteForClassification: it builds and sends one chat-completions
// request and parses the response envelope, returning a plain category
// (empty on success) instead of either use-case package's ProviderError
// type, which its two callers wrap right at their own boundary.
func (c *Client) doComplete(ctx context.Context, messages []wireMessage, maxOutputTokens int) (content string, promptTokens, completionTokens *int, cat category, err error) {
	body, err := json.Marshal(completionRequestBody{
		Model:     c.model,
		Messages:  messages,
		MaxTokens: maxOutputTokens,
	})
	if err != nil {
		return "", nil, nil, categoryClientError, errors.New("encode request")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, nil, categoryTransport, errors.New("build request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", nil, nil, categorizeTransportError(ctx, err), errors.New("request failed")
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, nil, categorizeStatus(resp.StatusCode), fmt.Errorf("status %d", resp.StatusCode)
	}

	var parsed completionResponseBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&parsed); err != nil {
		return "", nil, nil, categoryMalformedResponse, errors.New("decode response")
	}
	if len(parsed.Choices) == 0 {
		return "", nil, nil, categoryMalformedResponse, errors.New("no choices in response")
	}
	choice := parsed.Choices[0]
	if choice.FinishReason == "content_filter" || choice.Message.Refusal != "" {
		return "", nil, nil, categoryContentRefusal, errors.New("provider refused to generate content")
	}
	if choice.Message.Content == "" {
		return "", nil, nil, categoryMalformedResponse, errors.New("empty content in response")
	}

	return choice.Message.Content, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, "", nil
}

// categorizeTransportError distinguishes a timeout (the caller-supplied
// ctx expiring, or http.Client.Timeout firing) from every other
// transport-level failure. Both a caller-driven cancellation (job lease
// loss, worker shutdown) and a genuine provider timeout surface through
// the same net.Error shape from Go's http.Client; internal/jobs.Manager
// already special-cases jobCtx cancellation independently of whatever
// category the handler returns, so classifying a cancellation as a
// timeout here is safe either way.
func categorizeTransportError(ctx context.Context, err error) category {
	if errors.Is(err, context.DeadlineExceeded) {
		return categoryTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return categoryTimeout
	}
	if ctx.Err() != nil {
		return categoryTimeout
	}
	return categoryTransport
}

func categorizeStatus(status int) category {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return categoryAuth
	case status == http.StatusTooManyRequests:
		return categoryRateLimit
	case status >= 500:
		return categoryServerError
	default:
		return categoryClientError
	}
}

// drainAndClose gives net/http a chance to reuse a keep-alive connection
// after an early return (a non-2xx response or a decode error). The drain
// is bounded because the upstream response is untrusted.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes+1))
	_ = body.Close()
}
