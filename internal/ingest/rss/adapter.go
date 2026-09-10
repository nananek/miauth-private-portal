// Package rss implements Issue #11's first internal/ingest.Adapter:
// RSS 2.0 and Atom feed polling over internal/ingest/safehttp's
// SSRF-protected client, with ETag/Last-Modified conditional-fetch
// cursors and content-hash dedupe fallback.
package rss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
)

// Kind is the domain.ExternalSource.Kind value this Adapter handles.
const Kind = "rss"

const userAgent = "miauth-private-portal-rss-ingest/1"

// Config bounds one Adapter's per-fetch behavior. cmd/server builds it
// from internal/config.RSSConfig.
type Config struct {
	// FetchTimeout bounds a single Fetch call's HTTP round trip,
	// independent of whatever deadline ctx itself already carries.
	FetchTimeout time.Duration
	// MaxResponseBytes bounds how much of a feed response is read into
	// memory; a larger response fails the fetch as CategoryTooLarge.
	MaxResponseBytes int64
	// SummaryMaxChars bounds each item's normalized title/body length
	// after HTML tags are stripped. This is the *initial* value: Fetch
	// reloads it on every call via ReloadSummaryMaxChars, if set.
	SummaryMaxChars int
	// ReloadSummaryMaxChars, if non-nil, is called at the start of every
	// Fetch (Issue #76 PR4a, ADR-0006 Tier A) to get the current
	// effective RSS_SUMMARY_MAX_CHARS value; a non-positive result
	// (including the zero value from a Store that found no override) is
	// ignored and SummaryMaxChars above is used instead. Nil disables
	// reload entirely: SummaryMaxChars never changes after construction,
	// exactly this type's pre-#76 behavior.
	ReloadSummaryMaxChars func(ctx context.Context) int
}

// Adapter implements ingest.Adapter for RSS 2.0 and Atom feeds.
type Adapter struct {
	client *safehttp.Client
	cfg    Config
	// filter, when non-nil, drops items matching RSS_FILTER_SCRIPT_PATH's
	// compiled Starlark predicate (Issue #135) before Fetch returns them.
	// nil means no filtering is configured: every item is kept, exactly
	// pre-#135 behavior.
	filter *Filter
	logger *slog.Logger
}

// NewAdapter builds an Adapter. client is normally
// safehttp.NewClient(...); tests pass one built with
// safehttp.Config.AllowIPForTesting set. filter is nil when
// RSS_FILTER_SCRIPT_PATH is unset (Issue #135); logger is only used to
// warn-log a per-item filter evaluation failure, and defaults to
// slog.Default() when nil.
func NewAdapter(client *safehttp.Client, cfg Config, filter *Filter, logger *slog.Logger) *Adapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{client: client, cfg: cfg, filter: filter, logger: logger}
}

var _ ingest.Adapter = (*Adapter)(nil)

func (a *Adapter) Kind() string { return Kind }

// cursorState is this adapter's opaque cursor JSON: the conditional-
// fetch headers to send next time, round-tripped from the previous
// response's own ETag/Last-Modified.
type cursorState struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
}

// Fetch implements ingest.Adapter. It issues one conditional GET against
// source.URI, parses a 2xx response as RSS or Atom, and normalizes each
// item/entry. A malformed stored cursor is treated as absent (refetch
// from scratch) rather than failing the job: a corrupted cursor must
// never permanently wedge a source.
func (a *Adapter) Fetch(ctx context.Context, source domain.ExternalSource, cursor *string) (ingest.FetchResult, error) {
	var state cursorState
	if cursor != nil && *cursor != "" {
		if err := json.Unmarshal([]byte(*cursor), &state); err != nil {
			state = cursorState{}
		}
	}

	fetchCtx, cancel := context.WithTimeout(ctx, a.cfg.FetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, source.URI, nil)
	if err != nil {
		return ingest.FetchResult{}, ingest.NewFetchError(ingest.CategoryPolicy, fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml")
	if state.ETag != "" {
		req.Header.Set("If-None-Match", state.ETag)
	}
	if state.LastModified != "" {
		req.Header.Set("If-Modified-Since", state.LastModified)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return ingest.FetchResult{}, classifyDoError(fetchCtx, err)
	}
	defer drainAndClose(resp.Body, a.cfg.MaxResponseBytes)

	if resp.StatusCode == http.StatusNotModified {
		return ingest.FetchResult{NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ingest.FetchResult{}, ingest.NewFetchError(categorizeStatus(resp.StatusCode), fmt.Errorf("status %d", resp.StatusCode))
	}

	data, err := safehttp.ReadLimited(resp.Body, a.cfg.MaxResponseBytes)
	if err != nil {
		return ingest.FetchResult{}, ingest.NewFetchError(ingest.CategoryTooLarge, err)
	}

	fetchCfg := a.cfg
	if a.cfg.ReloadSummaryMaxChars != nil {
		if v := a.cfg.ReloadSummaryMaxChars(ctx); v > 0 {
			fetchCfg.SummaryMaxChars = v
		}
	}
	items, err := parseFeed(data, source.ID, fetchCfg)
	if err != nil {
		return ingest.FetchResult{}, ingest.NewFetchError(ingest.CategoryMalformed, err)
	}
	items = a.applyFilter(source, items)

	newState := cursorState{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	if newState.ETag == "" {
		newState.ETag = state.ETag
	}
	if newState.LastModified == "" {
		newState.LastModified = state.LastModified
	}
	var nextCursor string
	if newState.ETag != "" || newState.LastModified != "" {
		b, err := json.Marshal(newState)
		if err != nil {
			return ingest.FetchResult{}, ingest.NewFetchError(ingest.CategoryMalformed, fmt.Errorf("encode cursor: %w", err))
		}
		nextCursor = string(b)
	}

	return ingest.FetchResult{Items: items, NextCursor: nextCursor}, nil
}

// classifyDoError distinguishes an SSRF/scheme/redirect policy
// violation (safehttp.ErrPolicyViolation) and a timeout from every other
// transport-level failure, mirroring
// internal/provider/openai.categorizeTransportError's shape.
func classifyDoError(ctx context.Context, err error) error {
	if errors.Is(err, safehttp.ErrPolicyViolation) {
		return ingest.NewFetchError(ingest.CategoryPolicy, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ingest.NewFetchError(ingest.CategoryTimeout, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ingest.NewFetchError(ingest.CategoryTimeout, err)
	}
	if ctx.Err() != nil {
		return ingest.NewFetchError(ingest.CategoryTimeout, err)
	}
	return ingest.NewFetchError(ingest.CategoryTransport, err)
}

func categorizeStatus(status int) ingest.Category {
	switch {
	case status >= 500:
		return ingest.CategoryServerError
	default:
		return ingest.CategoryClientError
	}
}

// applyFilter drops every item a.filter's matches() reports true for
// (Issue #135). A per-item evaluation error is logged and the item is
// kept — fail open, see Filter.shouldExclude's own doc comment for why.
// A nil a.filter (RSS_FILTER_SCRIPT_PATH unset) returns items unchanged,
// exactly pre-#135 behavior.
func (a *Adapter) applyFilter(source domain.ExternalSource, items []ingest.FetchedItem) []ingest.FetchedItem {
	if a.filter == nil {
		return items
	}
	kept := items[:0]
	for _, item := range items {
		exclude, err := a.filter.shouldExclude(source, item)
		if err != nil {
			a.logger.Warn("rss filter evaluation failed, keeping item", "source_id", source.ID, "error", err.Error())
			kept = append(kept, item)
			continue
		}
		if !exclude {
			kept = append(kept, item)
		}
	}
	return kept
}

// drainAndClose gives net/http a chance to reuse a keep-alive connection
// after an early return (a 304/non-2xx response). The drain is bounded
// because the upstream response is untrusted.
func drainAndClose(body io.ReadCloser, maxBytes int64) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxBytes+1))
	_ = body.Close()
}
