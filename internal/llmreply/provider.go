// Package llmreply implements Issue #9's versioned LLM reply and
// follow-up-question generation: the reply/follow-up decision policy,
// high-risk qualified-language handling, thread-context prompt
// assembly, and the "llm_generation" durable job handler that ties them
// to internal/timeline.Service and an internal/provider/openai-shaped
// outbound Provider. Like internal/miauth and internal/timeline, this
// package depends only on internal/domain and its own narrow Provider
// port, never on net/http or a specific provider package (AGENTS.md:
// "Domain/use-case code must not depend on...a specific LLM provider" —
// internal/provider/openai depends on this package, not the reverse,
// mirroring internal/provider/misskey's relationship to internal/miauth).
package llmreply

import (
	"context"
	"errors"
)

// Message is one chat-style prompt turn.
type Message struct {
	// Role is "system", "user", or "assistant".
	Role    string
	Content string
}

// CompletionRequest is one generation call.
type CompletionRequest struct {
	Messages []Message
	// MaxOutputTokens bounds the completion length. Zero defers to the
	// provider's own default.
	MaxOutputTokens int
}

// CompletionResult is a successful generation. PromptTokens and
// CompletionTokens are nil when the provider does not report usage.
type CompletionResult struct {
	Content          string
	PromptTokens     *int
	CompletionTokens *int
}

// Provider is the narrow outbound boundary this package uses to
// generate text. internal/provider/openai implements it against a real
// OpenAI-compatible endpoint; tests use a fake, per AGENTS.md's "put
// provider boundaries behind narrow interfaces" rule.
type Provider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error)
}

// Category classifies a Provider.Complete failure so the job handler can
// decide retryable vs. permanent and record it as
// domain.LLMGeneration.ErrorCategory, without ever needing the
// underlying error text: AGENTS.md forbids logging LLM prompts or
// response bodies, and an upstream error body can echo request content
// back, so only Category (never a ProviderError's Error() text) may
// reach a log line.
type Category string

const (
	// CategoryTransport is a network-level failure (DNS, connection
	// refused, connection reset) below the HTTP response layer.
	// Retryable.
	CategoryTransport Category = "transport"
	// CategoryTimeout is a request that exceeded its deadline. Retryable.
	CategoryTimeout Category = "timeout"
	// CategoryAuth is an authentication/authorization failure (invalid or
	// missing API key, no access to the configured model). Permanent:
	// retrying the same configuration cannot help.
	CategoryAuth Category = "auth"
	// CategoryRateLimit is a rate-limit response. Retryable.
	CategoryRateLimit Category = "rate_limit"
	// CategoryServerError is an upstream server-side failure. Retryable.
	CategoryServerError Category = "server_error"
	// CategoryClientError is any other rejected request (a malformed
	// request, an unknown model, ...). Permanent.
	CategoryClientError Category = "client_error"
	// CategoryMalformedResponse is a response this package could not
	// interpret (invalid JSON, no choices, empty content). Permanent.
	CategoryMalformedResponse Category = "malformed_response"
	// CategoryContentRefusal is a response in which the model declined to
	// answer. Permanent: retrying the identical request is not expected
	// to help.
	CategoryContentRefusal Category = "content_refusal"
)

// permanentCategories are the Category values the job handler treats as
// non-retryable (jobs.Permanent). Every other category is retryable.
var permanentCategories = map[Category]bool{
	CategoryAuth:              true,
	CategoryClientError:       true,
	CategoryMalformedResponse: true,
	CategoryContentRefusal:    true,
}

// IsPermanent reports whether c should reach domain.GenerationFailed
// immediately rather than being retried.
func (c Category) IsPermanent() bool { return permanentCategories[c] }

// ProviderError wraps a classified Provider.Complete failure. Providers
// (internal/provider/openai) construct it via NewProviderError; the job
// handler reads Category back via ClassifyProviderError.
type ProviderError struct {
	Category Category
	err      error
}

// NewProviderError classifies err under category. A nil err returns nil
// so a provider can write "return NewProviderError(category, err)"
// without manufacturing a failure.
func NewProviderError(category Category, err error) error {
	if err == nil {
		return nil
	}
	return &ProviderError{Category: category, err: err}
}

func (e *ProviderError) Error() string {
	return "llmreply: " + string(e.Category) + ": " + e.err.Error()
}
func (e *ProviderError) Unwrap() error { return e.err }

// ClassifyProviderError extracts the Category a provider attached to err
// via NewProviderError. An err that is not a *ProviderError (or wraps
// one) classifies as CategoryClientError: an unclassified provider
// failure is treated as non-retryable rather than silently retried
// forever, since the job handler cannot otherwise tell whether retrying
// could help.
func ClassifyProviderError(err error) Category {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Category
	}
	return CategoryClientError
}
