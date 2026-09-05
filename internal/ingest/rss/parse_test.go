package rss

import (
	"testing"
)

const validRSSFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Example Feed</title>
    <item>
      <title>First &amp; Best &lt;b&gt;Post&lt;/b&gt;</title>
      <link>https://example.com/posts/1</link>
      <guid>urn:example:1</guid>
      <description><![CDATA[<p>Hello <strong>world</strong></p>]]></description>
      <pubDate>Mon, 02 Jan 2006 15:04:05 -0700</pubDate>
    </item>
    <item>
      <title>Second Post</title>
      <link>https://example.com/posts/2</link>
      <description>No guid here</description>
      <pubDate>Tue, 03 Jan 2006 15:04:05 -0700</pubDate>
    </item>
  </channel>
</rss>`

const validAtomFeed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example Atom Feed</title>
  <entry>
    <title>Atom Entry One</title>
    <id>urn:example:atom:1</id>
    <link rel="alternate" href="https://example.com/atom/1"/>
    <summary>&lt;p&gt;Some &lt;em&gt;summary&lt;/em&gt;&lt;/p&gt;</summary>
    <published>2006-01-02T15:04:05Z</published>
    <updated>2006-01-02T16:00:00Z</updated>
  </entry>
</feed>`

const malformedXML = `<rss version="2.0"><channel><item><title>unterminated`

func TestParseFeed_ValidRSS(t *testing.T) {
	items, err := parseFeed([]byte(validRSSFeed), "source-1", Config{SummaryMaxChars: 4000})
	if err != nil {
		t.Fatalf("parseFeed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}

	first := items[0]
	if first.ExternalID != "urn:example:1" {
		t.Errorf("first.ExternalID = %q, want urn:example:1", first.ExternalID)
	}
	if first.ProvenanceURL == nil || *first.ProvenanceURL != "https://example.com/posts/1" {
		t.Errorf("first.ProvenanceURL = %v, want https://example.com/posts/1", first.ProvenanceURL)
	}
	if first.Title != "First & Best Post" {
		t.Errorf("first.Title = %q, want %q (entities decoded, tags stripped)", first.Title, "First & Best Post")
	}
	if first.Body != "Hello world" {
		t.Errorf("first.Body = %q, want %q", first.Body, "Hello world")
	}
	if first.PublishedAt == nil {
		t.Error("first.PublishedAt = nil, want parsed RFC1123Z date")
	}
	if first.DedupeKey == "" {
		t.Error("first.DedupeKey is empty, want non-empty")
	}

	second := items[1]
	if second.ExternalID != "https://example.com/posts/2" {
		t.Errorf("second.ExternalID = %q, want the link fallback %q", second.ExternalID, "https://example.com/posts/2")
	}
}

func TestParseFeed_ValidAtom(t *testing.T) {
	items, err := parseFeed([]byte(validAtomFeed), "source-1", Config{SummaryMaxChars: 4000})
	if err != nil {
		t.Fatalf("parseFeed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	entry := items[0]
	if entry.ExternalID != "urn:example:atom:1" {
		t.Errorf("ExternalID = %q, want urn:example:atom:1", entry.ExternalID)
	}
	if entry.ProvenanceURL == nil || *entry.ProvenanceURL != "https://example.com/atom/1" {
		t.Errorf("ProvenanceURL = %v, want https://example.com/atom/1", entry.ProvenanceURL)
	}
	if entry.Title != "Atom Entry One" {
		t.Errorf("Title = %q, want %q", entry.Title, "Atom Entry One")
	}
	if entry.Body != "Some summary" {
		t.Errorf("Body = %q, want %q", entry.Body, "Some summary")
	}
	if entry.PublishedAt == nil {
		t.Fatal("PublishedAt = nil, want parsed RFC3339 <published>")
	}
	if entry.PublishedAt.Hour() != 15 {
		// <published> (15:04:05Z), not <updated> (16:00:00Z), must win
		// when both are present.
		t.Errorf("PublishedAt hour = %d, want 15 (from <published>, not <updated>)", entry.PublishedAt.Hour())
	}
}

func TestParseFeed_MalformedXMLReturnsError(t *testing.T) {
	_, err := parseFeed([]byte(malformedXML), "source-1", Config{SummaryMaxChars: 4000})
	if err == nil {
		t.Fatal("expected error for malformed XML, got nil")
	}
}

func TestParseFeed_UnrecognizedRootReturnsError(t *testing.T) {
	_, err := parseFeed([]byte(`<html><body>not a feed</body></html>`), "source-1", Config{SummaryMaxChars: 4000})
	if err == nil {
		t.Fatal("expected error for unrecognized root element, got nil")
	}
}

// TestParseFeed_DedupeKeyStableAcrossFetches documents that the same RSS
// item, parsed twice (simulating two separate polls of the same feed),
// produces the same DedupeKey both times — the property
// internal/timeline.CreateExternalEntry's conflict handling depends on
// to avoid creating a duplicate entry.
func TestParseFeed_DedupeKeyStableAcrossFetches(t *testing.T) {
	first, err := parseFeed([]byte(validRSSFeed), "source-1", Config{SummaryMaxChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseFeed([]byte(validRSSFeed), "source-1", Config{SummaryMaxChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].DedupeKey != second[0].DedupeKey {
		t.Errorf("DedupeKey changed across fetches: %q vs %q", first[0].DedupeKey, second[0].DedupeKey)
	}
	if first[0].DedupeKey == first[1].DedupeKey {
		t.Error("two distinct items produced the same DedupeKey")
	}
}

// TestParseFeed_DedupeKeyDiffersAcrossSources documents that the same
// item guid from two different sources does not collide: DedupeKey
// incorporates sourceID.
func TestParseFeed_DedupeKeyDiffersAcrossSources(t *testing.T) {
	a, err := parseFeed([]byte(validRSSFeed), "source-a", Config{SummaryMaxChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseFeed([]byte(validRSSFeed), "source-b", Config{SummaryMaxChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if a[0].DedupeKey == b[0].DedupeKey {
		t.Error("DedupeKey collided across different sources for the same guid")
	}
}

func TestParseFeed_ItemWithoutGUIDOrLinkGetsUniqueExternalID(t *testing.T) {
	feed := `<rss version="2.0"><channel>
		<item><title>A</title><description>first</description></item>
		<item><title>B</title><description>second</description></item>
	</channel></rss>`
	items, err := parseFeed([]byte(feed), "source-1", Config{SummaryMaxChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].ExternalID == "" || items[1].ExternalID == "" {
		t.Fatal("ExternalID must never be empty")
	}
	if items[0].ExternalID == items[1].ExternalID {
		t.Error("distinct items without guid/link got the same ExternalID")
	}
}
