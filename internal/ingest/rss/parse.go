package rss

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/textsanitize"
)

// rssRootXML and atomRootXML deliberately omit an XMLName field: Go's
// encoding/xml only validates the root element name when one is
// present, and this package already determines RSS vs. Atom itself (see
// detectRootLocalName) before choosing which of these two to unmarshal
// into. Every other field's tag also omits a namespace, which makes
// encoding/xml match on local name alone — necessary for Atom, whose
// real-world feeds are inconsistent about declaring/prefixing the
// "http://www.w3.org/2005/Atom" namespace.
type rssRootXML struct {
	Channel struct {
		Items []rssItemXML `xml:"item"`
	} `xml:"channel"`
}

type rssItemXML struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}

type atomRootXML struct {
	Entries []atomEntryXML `xml:"entry"`
}

type atomEntryXML struct {
	Title     string        `xml:"title"`
	ID        string        `xml:"id"`
	Links     []atomLinkXML `xml:"link"`
	Summary   string        `xml:"summary"`
	Content   string        `xml:"content"`
	Updated   string        `xml:"updated"`
	Published string        `xml:"published"`
}

type atomLinkXML struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

var rssDateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	time.RFC3339,
}

var atomDateLayouts = []string{
	time.RFC3339,
	time.RFC3339Nano,
}

// parseFeed detects whether data's root element is an RSS <rss> or an
// Atom <feed> and normalizes its items/entries into ingest.FetchedItem.
// Any other root element, or XML that cannot be tokenized at all, is a
// CategoryMalformed condition the caller wraps.
func parseFeed(data []byte, sourceID string, cfg Config) ([]ingest.FetchedItem, error) {
	root, err := detectRootLocalName(data)
	if err != nil {
		return nil, fmt.Errorf("determine feed root element: %w", err)
	}

	switch root {
	case "rss":
		var feed rssRootXML
		if err := xml.Unmarshal(data, &feed); err != nil {
			return nil, fmt.Errorf("parse rss feed: %w", err)
		}
		return normalizeRSSItems(feed.Channel.Items, sourceID, cfg), nil
	case "feed":
		var feed atomRootXML
		if err := xml.Unmarshal(data, &feed); err != nil {
			return nil, fmt.Errorf("parse atom feed: %w", err)
		}
		return normalizeAtomEntries(feed.Entries, sourceID, cfg), nil
	default:
		return nil, fmt.Errorf("unrecognized feed root element %q", root)
	}
}

func detectRootLocalName(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, nil
		}
	}
}

func normalizeRSSItems(items []rssItemXML, sourceID string, cfg Config) []ingest.FetchedItem {
	out := make([]ingest.FetchedItem, 0, len(items))
	for _, item := range items {
		externalID := strings.TrimSpace(item.GUID)
		link := strings.TrimSpace(item.Link)
		if externalID == "" {
			externalID = link
		}

		key := dedupeKey(sourceID, externalID, item.Title+"|"+item.Description+"|"+item.PubDate)
		if externalID == "" {
			// Guarantees ExternalID is always unique within a source
			// even for a feed item with neither a guid nor a link: the
			// (source_id, external_id) UNIQUE constraint
			// (internal/storage/sqlite/migrations/0008_external_sources.sql)
			// would otherwise reject a second such item under the same
			// empty external_id even though it is a distinct item with
			// its own distinct dedupe_key.
			externalID = key
		}

		var provenanceURL *string
		if link != "" {
			provenanceURL = &link
		}

		out = append(out, ingest.FetchedItem{
			ExternalID:    externalID,
			DedupeKey:     key,
			ProvenanceURL: provenanceURL,
			PublishedAt:   parseFlexibleDate(item.PubDate, rssDateLayouts),
			Title:         textsanitize.StripHTML(item.Title, cfg.SummaryMaxChars),
			Body:          textsanitize.StripHTML(item.Description, cfg.SummaryMaxChars),
		})
	}
	return out
}

func normalizeAtomEntries(entries []atomEntryXML, sourceID string, cfg Config) []ingest.FetchedItem {
	out := make([]ingest.FetchedItem, 0, len(entries))
	for _, entry := range entries {
		link := atomCanonicalLink(entry.Links)
		externalID := strings.TrimSpace(entry.ID)
		if externalID == "" {
			externalID = link
		}

		body := entry.Summary
		if body == "" {
			body = entry.Content
		}
		key := dedupeKey(sourceID, externalID, entry.Title+"|"+body+"|"+entry.Updated)
		if externalID == "" {
			externalID = key
		}

		var provenanceURL *string
		if link != "" {
			provenanceURL = &link
		}

		publishedAt := parseFlexibleDate(entry.Published, atomDateLayouts)
		if publishedAt == nil {
			publishedAt = parseFlexibleDate(entry.Updated, atomDateLayouts)
		}

		out = append(out, ingest.FetchedItem{
			ExternalID:    externalID,
			DedupeKey:     key,
			ProvenanceURL: provenanceURL,
			PublishedAt:   publishedAt,
			Title:         textsanitize.StripHTML(entry.Title, cfg.SummaryMaxChars),
			Body:          textsanitize.StripHTML(body, cfg.SummaryMaxChars),
		})
	}
	return out
}

// atomCanonicalLink picks the entry's "alternate" (or rel-less, which
// defaults to alternate per RFC 4287) link, falling back to the first
// link present.
func atomCanonicalLink(links []atomLinkXML) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return strings.TrimSpace(l.Href)
		}
	}
	if len(links) > 0 {
		return strings.TrimSpace(links[0].Href)
	}
	return ""
}

// dedupeKey is always non-empty and deterministic for the same (source,
// item): when externalID is non-empty it alone is hashed (a guid/id is
// meant to be a stable identity, so re-fetching the same item always
// reproduces the same key); when externalID is empty, fallback
// (adapter-chosen content, e.g. title+body+date) is hashed instead.
func dedupeKey(sourceID, externalID, fallback string) string {
	h := sha256.New()
	h.Write([]byte(sourceID))
	h.Write([]byte{'|'})
	if externalID != "" {
		h.Write([]byte(externalID))
	} else {
		h.Write([]byte(fallback))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func parseFlexibleDate(s string, layouts []string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}
