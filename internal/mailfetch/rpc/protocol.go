// Package rpc defines the wire shape internal/ingest/imap (running inside
// cmd/server) and cmd/mailfetch speak to each other over a Unix domain
// socket, per docs/decisions/0003-imap-mailfetch-isolation.md: one
// newline-terminated JSON Request, one newline-terminated JSON Response,
// one socket connection per call. This package intentionally imports
// nothing beyond the standard library so that internal/ingest/imap
// importing it never pulls an IMAP or MIME library into cmd/server's
// build graph — the isolation ADR-0003 decides on depends on this package
// staying dependency-free.
package rpc

import "time"

// Request is one internal/ingest/imap.Adapter.Fetch call's RPC request.
// Credentials travel only here, in the per-request payload, never as a
// command-line argument or a value read from cmd/mailfetch's own
// environment (see ADR-0003's threat model).
type Request struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLSMode  string `json:"tlsMode"` // "implicit" | "starttls"
	Username string `json:"username"`
	Password string `json:"password"`
	Mailbox  string `json:"mailbox"`
	// SourceID is the polled domain.ExternalSource's ID, used only to
	// scope the fallback dedupe key (see internal/mailfetch's
	// identify) the same way internal/ingest/rss scopes its own
	// content-hash fallback by source.
	SourceID string `json:"sourceId"`
	// FetchTimeoutMs bounds the whole IMAP round trip (connect through
	// LOGOUT), in milliseconds.
	FetchTimeoutMs int64 `json:"fetchTimeoutMs"`
	// MaxMessageBytes bounds how many octets of a single message's text
	// body cmd/mailfetch reads (the BODY.PEEK<0,N> upper bound).
	MaxMessageBytes int64 `json:"maxMessageBytes"`
	// SnippetMaxChars bounds Item.Body's length after HTML sanitization,
	// unless StoreFullBody raises that bound to FullBodyMaxChars.
	SnippetMaxChars  int  `json:"snippetMaxChars"`
	StoreFullBody    bool `json:"storeFullBody"`
	FullBodyMaxChars int  `json:"fullBodyMaxChars"`
	// Cursor is the opaque cursor JSON internal/ingest/imap last
	// recorded (empty on a source's first-ever fetch). cmd/mailfetch
	// decodes/encodes its own cursor shape; internal/ingest/imap never
	// inspects Cursor's contents, only round-trips it.
	Cursor string `json:"cursor,omitempty"`
}

// Item is one normalized message cmd/mailfetch returns, matching
// internal/ingest.FetchedItem field-for-field so
// internal/ingest/imap.Adapter.Fetch only needs to copy it across, never
// interpret it.
type Item struct {
	ExternalID    string     `json:"externalId"`
	DedupeKey     string     `json:"dedupeKey"`
	ProvenanceURL *string    `json:"provenanceUrl,omitempty"`
	PublishedAt   *time.Time `json:"publishedAt,omitempty"`
	Body          string     `json:"body"`
}

// ErrorInfo classifies a failed fetch. Category is one of
// internal/ingest.Category's string values (for example "transport" or
// "timeout"); internal/ingest/imap converts it back with
// ingest.Category(resp.Error.Category). Message is a short, loggable
// description — never raw IMAP server text or untrusted mail content,
// matching AGENTS.md's rule against logging message bodies.
type ErrorInfo struct {
	Category string `json:"category"`
	Message  string `json:"message"`
}

// Response is cmd/mailfetch's reply to one Request.
type Response struct {
	// Items is empty when there is nothing new since Request.Cursor.
	Items []Item `json:"items,omitempty"`
	// NextCursor is cmd/mailfetch's own opaque cursor for the next
	// Request against this mailbox. Always set on success, even when
	// Items is empty, so a source with no new mail still advances past
	// messages internal/ingest/imap already knows about.
	NextCursor string     `json:"nextCursor,omitempty"`
	Error      *ErrorInfo `json:"error,omitempty"`
}
