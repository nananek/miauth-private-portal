package ingest

import "errors"

// Category classifies an Adapter.Fetch failure so Service can decide
// retryable vs. permanent, mirroring internal/llmclassify.Category and
// internal/llmreply.Category's shape (a fixed, loggable classification;
// never the underlying error text, which for an ingestion adapter can
// echo back untrusted remote content).
type Category string

const (
	// CategoryTransport is a network-level failure (connection refused,
	// DNS failure, TLS error, ...). Retryable.
	CategoryTransport Category = "transport"
	// CategoryTimeout is a context deadline or transport-level timeout.
	// Retryable.
	CategoryTimeout Category = "timeout"
	// CategoryTooLarge is a response exceeding the configured size
	// bound. Not retryable: the source will not become smaller.
	CategoryTooLarge Category = "too_large"
	// CategoryServerError is a 5xx response. Retryable.
	CategoryServerError Category = "server_error"
	// CategoryClientError is a 4xx response other than one implying a
	// policy violation. Not retryable.
	CategoryClientError Category = "client_error"
	// CategoryMalformed is a response this adapter could not parse
	// (invalid XML, unrecognized feed root element, ...). Not
	// retryable.
	CategoryMalformed Category = "malformed"
	// CategoryPolicy is an SSRF/scheme/redirect policy violation (see
	// internal/ingest/safehttp). Not retryable: the configured URI
	// itself is disallowed, and retrying cannot change that.
	CategoryPolicy Category = "policy"
)

// permanentCategories are the Category values Service treats as
// non-retryable (jobs.Permanent). Every other category is retryable.
var permanentCategories = map[Category]bool{
	CategoryTooLarge:    true,
	CategoryClientError: true,
	CategoryMalformed:   true,
	CategoryPolicy:      true,
}

// IsPermanent reports whether c should fail the job immediately rather
// than being retried.
func (c Category) IsPermanent() bool { return permanentCategories[c] }

// FetchError wraps a classified Adapter.Fetch failure. Adapters
// construct it via NewFetchError; Service reads Category back via
// ClassifyFetchError.
type FetchError struct {
	Category Category
	err      error
}

// NewFetchError classifies err under category. A nil err returns nil so
// an adapter can write "return NewFetchError(category, err)" without
// manufacturing a failure.
func NewFetchError(category Category, err error) error {
	if err == nil {
		return nil
	}
	return &FetchError{Category: category, err: err}
}

func (e *FetchError) Error() string {
	return "ingest: " + string(e.Category) + ": " + e.err.Error()
}
func (e *FetchError) Unwrap() error { return e.err }

// ClassifyFetchError extracts the Category an adapter attached to err
// via NewFetchError. An err that is not a *FetchError (or wrapping one)
// classifies as CategoryClientError: an unclassified adapter failure is
// treated as non-retryable rather than silently retried forever, since
// Service cannot otherwise tell whether retrying could help.
func ClassifyFetchError(err error) Category {
	var fe *FetchError
	if errors.As(err, &fe) {
		return fe.Category
	}
	return CategoryClientError
}
