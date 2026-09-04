package rss

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
)

func testSafehttpClient() *safehttp.Client {
	return safehttp.NewClient(safehttp.Config{
		MaxRedirects:      3,
		AllowInsecureHTTP: true,
		AllowIPForTesting: func(net.IP) bool { return true },
	})
}

func TestAdapter_Fetch_ValidFeedReturnsItemsAndCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/rss+xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validRSSFeed))
	}))
	defer server.Close()

	adapter := NewAdapter(testSafehttpClient(), Config{FetchTimeout: 5 * time.Second, MaxResponseBytes: 1 << 20, SummaryMaxChars: 4000})
	source := domain.ExternalSource{ID: "source-1", Kind: Kind, URI: server.URL}

	result, err := adapter.Fetch(context.Background(), source, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if result.NotModified {
		t.Error("NotModified = true, want false on first fetch")
	}
	if len(result.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(result.Items))
	}
	if result.NextCursor == "" {
		t.Fatal("NextCursor is empty, want a cursor carrying the ETag")
	}
	var state cursorState
	if err := json.Unmarshal([]byte(result.NextCursor), &state); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if state.ETag != `"v1"` {
		t.Errorf("cursor ETag = %q, want %q", state.ETag, `"v1"`)
	}
}

// TestAdapter_Fetch_ConditionalRequestReturnsNotModified is the ETag
// round-trip Issue #11 requires: a cursor from a prior fetch is sent
// back as If-None-Match, and a 304 response yields NotModified with no
// items, never re-processing the same items.
func TestAdapter_Fetch_ConditionalRequestReturnsNotModified(t *testing.T) {
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	adapter := NewAdapter(testSafehttpClient(), Config{FetchTimeout: 5 * time.Second, MaxResponseBytes: 1 << 20, SummaryMaxChars: 4000})
	source := domain.ExternalSource{ID: "source-1", Kind: Kind, URI: server.URL}

	cursor := `{"etag":"\"v1\""}`
	result, err := adapter.Fetch(context.Background(), source, &cursor)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !result.NotModified {
		t.Error("NotModified = false, want true for a 304 response")
	}
	if len(result.Items) != 0 {
		t.Errorf("len(Items) = %d, want 0 for NotModified", len(result.Items))
	}
	if gotIfNoneMatch != `"v1"` {
		t.Errorf("If-None-Match sent = %q, want %q", gotIfNoneMatch, `"v1"`)
	}
}

func TestAdapter_Fetch_MalformedCursorIsTreatedAsAbsent(t *testing.T) {
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validRSSFeed))
	}))
	defer server.Close()

	adapter := NewAdapter(testSafehttpClient(), Config{FetchTimeout: 5 * time.Second, MaxResponseBytes: 1 << 20, SummaryMaxChars: 4000})
	source := domain.ExternalSource{ID: "source-1", Kind: Kind, URI: server.URL}

	corrupted := "not valid json"
	result, err := adapter.Fetch(context.Background(), source, &corrupted)
	if err != nil {
		t.Fatalf("Fetch with corrupted cursor must not fail the job: %v", err)
	}
	if len(result.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2 (refetched from scratch)", len(result.Items))
	}
	if gotIfNoneMatch != "" {
		t.Errorf("If-None-Match sent = %q, want empty (corrupted cursor discarded)", gotIfNoneMatch)
	}
}

func TestAdapter_Fetch_MalformedFeedReturnsCategoryMalformed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(malformedXML))
	}))
	defer server.Close()

	adapter := NewAdapter(testSafehttpClient(), Config{FetchTimeout: 5 * time.Second, MaxResponseBytes: 1 << 20, SummaryMaxChars: 4000})
	source := domain.ExternalSource{ID: "source-1", Kind: Kind, URI: server.URL}

	_, err := adapter.Fetch(context.Background(), source, nil)
	if err == nil {
		t.Fatal("expected error for malformed feed, got nil")
	}
	if got := ingest.ClassifyFetchError(err); got != ingest.CategoryMalformed {
		t.Errorf("category = %q, want %q", got, ingest.CategoryMalformed)
	}
}

func TestAdapter_Fetch_OversizedResponseReturnsCategoryTooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer server.Close()

	adapter := NewAdapter(testSafehttpClient(), Config{FetchTimeout: 5 * time.Second, MaxResponseBytes: 10, SummaryMaxChars: 4000})
	source := domain.ExternalSource{ID: "source-1", Kind: Kind, URI: server.URL}

	_, err := adapter.Fetch(context.Background(), source, nil)
	if err == nil {
		t.Fatal("expected error for oversized response, got nil")
	}
	if got := ingest.ClassifyFetchError(err); got != ingest.CategoryTooLarge {
		t.Errorf("category = %q, want %q", got, ingest.CategoryTooLarge)
	}
}

func TestAdapter_Fetch_TimeoutReturnsCategoryTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validRSSFeed))
	}))
	defer server.Close()

	adapter := NewAdapter(testSafehttpClient(), Config{FetchTimeout: 10 * time.Millisecond, MaxResponseBytes: 1 << 20, SummaryMaxChars: 4000})
	source := domain.ExternalSource{ID: "source-1", Kind: Kind, URI: server.URL}

	_, err := adapter.Fetch(context.Background(), source, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if got := ingest.ClassifyFetchError(err); got != ingest.CategoryTimeout {
		t.Errorf("category = %q, want %q", got, ingest.CategoryTimeout)
	}
}

func TestAdapter_Fetch_SSRFBlockedAddressReturnsCategoryPolicy(t *testing.T) {
	// A production-policy client (no AllowIPForTesting) must refuse a
	// source URI pointing at a loopback address, classified as
	// CategoryPolicy (permanent — retrying can never fix a
	// misconfigured/malicious source URI).
	client := safehttp.NewClient(safehttp.Config{MaxRedirects: 3, AllowInsecureHTTP: true})
	adapter := NewAdapter(client, Config{FetchTimeout: 5 * time.Second, MaxResponseBytes: 1 << 20, SummaryMaxChars: 4000})
	source := domain.ExternalSource{ID: "source-1", Kind: Kind, URI: "http://127.0.0.1:1/feed.xml"}

	_, err := adapter.Fetch(context.Background(), source, nil)
	if err == nil {
		t.Fatal("expected error for loopback address, got nil")
	}
	if got := ingest.ClassifyFetchError(err); got != ingest.CategoryPolicy {
		t.Errorf("category = %q, want %q", got, ingest.CategoryPolicy)
	}
	if !errors.Is(err, safehttp.ErrPolicyViolation) {
		t.Errorf("err = %v, want it to wrap safehttp.ErrPolicyViolation", err)
	}
}

var _ ingest.Adapter = (*Adapter)(nil)
