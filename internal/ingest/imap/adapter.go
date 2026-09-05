// Package imap implements Issue #12's IMAP internal/ingest.Adapter as a
// thin RPC client to cmd/mailfetch over a Unix domain socket, per
// docs/decisions/0003-imap-mailfetch-isolation.md. This package
// deliberately imports neither an IMAP nor a MIME library — the untrusted
// protocol/parsing work lives entirely in internal/mailfetch, used only
// by cmd/mailfetch — so github.com/emersion/go-imap and go-message never
// appear in cmd/server's build graph.
package imap

import (
	"context"
	"errors"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
)

// Kind is the domain.ExternalSource.Kind value this Adapter handles.
const Kind = "imap"

// connectSlack bounds the extra time Fetch allows beyond its own
// FetchTimeout for the added Unix-socket hop (dial, write, and read the
// framed request/response) on top of cmd/mailfetch's own IMAP round
// trip, so the client's deadline expires strictly after the request it
// sent asks cmd/mailfetch to give up by — the client should see
// cmd/mailfetch's own timeout response, not race ahead of it.
const connectSlack = 5 * time.Second

// Config bounds one Adapter's per-fetch behavior and identifies the
// single IMAP mailbox it polls. cmd/server builds it from
// internal/config.IMAPConfig. Unlike internal/ingest/rss.Config (paired
// with a distinct domain.ExternalSource.URI per configured feed), Issue
// #12 supports exactly one configured mailbox: every field here is
// shared across the one domain.ExternalSource{Kind: Kind} cmd/server
// seeds, and Fetch never reads source.URI itself — URI exists only as
// that source's (kind, uri) identity/uniqueness anchor in the database.
type Config struct {
	Host     string
	Port     int
	TLSMode  string
	Username string
	Password string
	Mailbox  string
	// SocketPath is the Unix domain socket cmd/mailfetch listens on.
	SocketPath       string
	FetchTimeout     time.Duration
	MaxMessageBytes  int64
	SnippetMaxChars  int
	StoreFullBody    bool
	FullBodyMaxChars int
}

// Adapter implements ingest.Adapter. It holds no IMAP protocol state
// between calls: every Fetch opens a fresh socket connection, sends one
// request, reads one response, and closes it.
type Adapter struct {
	cfg Config
}

// NewAdapter builds an Adapter from cfg.
func NewAdapter(cfg Config) *Adapter {
	return &Adapter{cfg: cfg}
}

var _ ingest.Adapter = (*Adapter)(nil)

func (a *Adapter) Kind() string { return Kind }

// Fetch implements ingest.Adapter by delegating the actual IMAP fetch to
// cmd/mailfetch over a.cfg.SocketPath. source.URI and source.DisplayName
// are never read (see Config's doc comment); only source.ID is sent, to
// scope cmd/mailfetch's fallback dedupe key.
func (a *Adapter) Fetch(ctx context.Context, source domain.ExternalSource, cursor *string) (ingest.FetchResult, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, a.cfg.FetchTimeout+connectSlack)
	defer cancel()

	req := rpc.Request{
		Host:             a.cfg.Host,
		Port:             a.cfg.Port,
		TLSMode:          a.cfg.TLSMode,
		Username:         a.cfg.Username,
		Password:         a.cfg.Password,
		Mailbox:          a.cfg.Mailbox,
		SourceID:         source.ID,
		FetchTimeoutMs:   a.cfg.FetchTimeout.Milliseconds(),
		MaxMessageBytes:  a.cfg.MaxMessageBytes,
		SnippetMaxChars:  a.cfg.SnippetMaxChars,
		StoreFullBody:    a.cfg.StoreFullBody,
		FullBodyMaxChars: a.cfg.FullBodyMaxChars,
	}
	if cursor != nil {
		req.Cursor = *cursor
	}

	resp, err := a.call(fetchCtx, req)
	if err != nil {
		return ingest.FetchResult{}, err
	}
	if resp.Error != nil {
		return ingest.FetchResult{}, ingest.NewFetchError(ingest.Category(resp.Error.Category), errors.New(resp.Error.Message))
	}

	items := make([]ingest.FetchedItem, len(resp.Items))
	for i, it := range resp.Items {
		items[i] = ingest.FetchedItem{
			ExternalID:    it.ExternalID,
			DedupeKey:     it.DedupeKey,
			ProvenanceURL: it.ProvenanceURL,
			PublishedAt:   it.PublishedAt,
			Body:          it.Body,
		}
	}
	return ingest.FetchResult{Items: items, NextCursor: resp.NextCursor}, nil
}
