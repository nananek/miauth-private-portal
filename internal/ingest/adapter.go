// Package ingest implements Issue #11's extensible external-source
// ingestion framework: an adapter-agnostic fetch/normalize boundary
// (Adapter), the durable "external_source_poll" job handler (Service)
// that ties an Adapter to internal/timeline.Service.CreateExternalEntry
// for dedupe and provenance-preserving entry creation, and a lightweight
// Scheduler that periodically enqueues one poll job per configured
// domain.ExternalSource. Only internal/ingest/rss (Issue #11) implements
// Adapter today; Issue #12's IMAP adapter is expected to register into
// the same Service (a new Service.RegisterAdapter call in cmd/server)
// without this package changing. Like internal/llmreply and
// internal/llmclassify, this package depends only on internal/domain,
// internal/jobs, and internal/timeline, never on net/http or a specific
// adapter's wire format (AGENTS.md: "put provider boundaries behind
// narrow interfaces").
package ingest

import (
	"context"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// FetchedItem is one adapter-normalized item, ready for dedupe and
// promotion into the timeline. Body must already be sanitized,
// untrusted-safe plain text: AGENTS.md requires external content is
// treated as untrusted and never executed, and Service never sanitizes
// Body itself. Body is also never fed to an LLM prompt by this package;
// that stays out of scope until an explicit future issue wires it in
// (AGENTS.md: never auto-feed external content into an LLM prompt
// without explicit configuration).
type FetchedItem struct {
	// ExternalID is the source's own stable item identifier (an RSS
	// guid, an Atom id, an IMAP Message-ID, ...). It may be empty for a
	// source whose items carry no stable ID of their own; DedupeKey is
	// then the adapter's own fallback dedupe signal.
	ExternalID string
	// DedupeKey is a fallback dedupe signal, always non-empty and
	// deterministic for the same logical item, even when ExternalID is
	// empty. See internal/domain.ExternalItem.DedupeKey.
	DedupeKey     string
	ProvenanceURL *string
	PublishedAt   *time.Time
	Title         string
	Body          string
}

// FetchResult is one Adapter.Fetch call's outcome.
type FetchResult struct {
	// Items is empty when NotModified is true.
	Items []FetchedItem
	// NextCursor is the adapter's own opaque cursor for the next Fetch
	// call against this source. Empty means "no new cursor to record":
	// Service leaves the previously stored cursor unchanged rather than
	// clearing it (see domain.ExternalSourceRepository.RecordFetchSuccess).
	NextCursor string
	// NotModified reports the source has no new content since the
	// cursor Fetch was called with (for example an HTTP 304). Items is
	// always empty when this is true.
	NotModified bool
}

// Adapter fetches and normalizes one external source kind.
// internal/ingest/rss.Adapter is the first implementation; Issue #12
// adds an IMAP one behind the same interface.
type Adapter interface {
	// Kind is the domain.ExternalSource.Kind value this Adapter handles
	// (for example "rss"). Service dispatches to an Adapter by looking
	// up a claimed job's source.Kind in its registry.
	Kind() string
	// Fetch retrieves source's content since cursor (nil on a source's
	// first-ever fetch, or after a prior fetch that returned no new
	// cursor). Implementations must honor ctx cancellation/timeout and
	// must never fetch anything but source.URI: AGENTS.md's SSRF
	// protections (fixed scheme, host validation, redirect limits) are
	// each adapter's own responsibility, typically delegated to
	// internal/ingest/safehttp.
	Fetch(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error)
}
