// Package llmclassify implements Issue #10's versioned, LLM-authored post
// classification: structured-output schema validation/normalization,
// same-thread related-post candidate prompt assembly, and the
// "llm_classification" durable job handler that ties them to
// internal/domain's LLMClassificationRepository and an
// internal/provider/openai-shaped outbound Provider. Like internal/miauth,
// internal/timeline, and internal/llmreply, this package depends only on
// internal/domain and its own narrow Provider port, never on net/http, a
// specific provider package, or internal/timeline: classification never
// creates a new Entry, only versioned metadata about an existing one
// (AGENTS.md: "Domain/use-case code must not depend on...a specific LLM
// provider").
package llmclassify

import (
	"context"
	"errors"
)

// Message is one chat-style prompt turn.
type Message struct {
	// Role is "system" or "user". Classification never plays back an
	// assistant turn (see promptbuilder.go).
	Role    string
	Content string
}

// CompletionRequest is one classification call.
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

// Provider is the narrow outbound boundary this package uses to produce
// structured classification output. internal/provider/openai implements
// it (CompleteForClassification) against the same real OpenAI-compatible
// endpoint internal/llmreply.Provider uses; tests use a fake, per
// AGENTS.md's "put provider boundaries behind narrow interfaces" rule.
// This is a distinct, independent type from internal/llmreply.Provider
// (never an alias or embedding of it): the two use-case packages must not
// depend on each other, so each defines its own copy of this narrow
// shape.
type Provider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error)
}

// Category classifies a Provider.Complete failure so the job handler can
// decide retryable vs. permanent and record it as
// domain.LLMClassification.ErrorCategory, without ever needing the
// underlying error text: AGENTS.md forbids logging LLM prompts or
// response bodies, and an upstream error body can echo request content
// back, so only Category (never a ProviderError's Error() text) may reach
// a log line. These constants mirror internal/llmreply.Category exactly
// (same provider, same wire failure modes); schema validation's own
// failures (invalid JSON, all required fields missing) are also
// classified CategoryMalformedResponse, matching internal/llmreply's
// meaning of "a response this package could not interpret".
type Category string

const (
	CategoryTransport         Category = "transport"
	CategoryTimeout           Category = "timeout"
	CategoryAuth              Category = "auth"
	CategoryRateLimit         Category = "rate_limit"
	CategoryServerError       Category = "server_error"
	CategoryClientError       Category = "client_error"
	CategoryMalformedResponse Category = "malformed_response"
	CategoryContentRefusal    Category = "content_refusal"
)

// permanentCategories are the Category values the job handler treats as
// non-retryable (jobs.Permanent). Every other category is retryable.
var permanentCategories = map[Category]bool{
	CategoryAuth:              true,
	CategoryClientError:       true,
	CategoryMalformedResponse: true,
	CategoryContentRefusal:    true,
}

// IsPermanent reports whether c should reach domain.ClassificationFailed
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
	return "llmclassify: " + string(e.Category) + ": " + e.err.Error()
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
