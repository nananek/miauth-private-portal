package mailfetch

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/quotedprintable"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	mcharset "github.com/emersion/go-message/charset"

	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
	"github.com/nananek/miauth-private-portal/internal/textsanitize"
)

// maxMessagesPerFetch bounds how many new messages one Fetch call
// processes. A large first-ever backfill (thousands of pre-existing
// messages in a long-lived mailbox) is deliberately drained gradually,
// maxMessagesPerFetch at a time across successive scheduled polls, rather
// than in one unbounded batch that could make a single fetch run far past
// Request.FetchTimeoutMs or hold the whole mailbox's ENVELOPE/BODYSTRUCTURE
// data in memory at once. It is a fixed constant, not configurable: no
// operator has a legitimate reason to tune it, matching this repository's
// preference for fixed internal bounds over speculative new config keys.
const maxMessagesPerFetch = 200

// defaultTimeout is used when a Request omits FetchTimeoutMs (a value <=
// 0), so a malformed request can never leave a connection attempt
// unbounded.
const defaultTimeout = 30 * time.Second

// Fetch is this package's single entry point: cmd/mailfetch's RPC server
// calls it once per accepted connection. It never returns an error
// itself; every failure is classified into resp.Error so cmd/mailfetch's
// caller only ever needs to write resp back across the socket.
func Fetch(ctx context.Context, req rpc.Request) rpc.Response {
	timeout := time.Duration(req.FetchTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := dialWithContext(ctx, req, timeout)
	if err != nil {
		return errorResponse(classifyConnError(err))
	}
	defer func() { _ = c.Logout() }()

	items, nextCursor, err := fetchMailbox(c, req)
	if err != nil {
		return errorResponse(classifyFetchError(err))
	}
	return rpc.Response{Items: items, NextCursor: nextCursor}
}

// dialWithContext runs dial in a goroutine so a hung TCP handshake or a
// slow LOGIN round trip is still bounded by ctx, not just by the dialer's
// own connect timeout: go-imap's client has no context-aware API of its
// own to plumb ctx through.
func dialWithContext(ctx context.Context, req rpc.Request, timeout time.Duration) (*client.Client, error) {
	type result struct {
		c   *client.Client
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		c, err := dial(req, timeout)
		resCh <- result{c, err}
	}()

	select {
	case <-ctx.Done():
		// The goroutine above may still complete a dial after we give
		// up on it; if it does, close that connection immediately
		// rather than leaking it.
		go func() {
			if r := <-resCh; r.c != nil {
				_ = r.c.Logout()
			}
		}()
		return nil, ctx.Err()
	case r := <-resCh:
		return r.c, r.err
	}
}

func errorResponse(category ingest.Category, msg string) rpc.Response {
	return rpc.Response{Error: &rpc.ErrorInfo{Category: string(category), Message: msg}}
}

// classifyConnError maps a dial/login failure to an ingest.Category. It
// never inspects the underlying error's text: go-imap v1's Client.Login
// returns a plain error built from the server's own NO/BAD response text
// for both "wrong credentials" and other command-execution failures alike
// (there is no typed distinction to check client-side, unlike
// server-side-only *imap.ErrStatusResp), and that text is untrusted,
// server-controlled content this package must not pattern-match on. The
// practical cost of this imprecision: a wrong password is retried like a
// transient failure (internal/jobs' normal backoff, up to
// JOBS_MAX_ATTEMPTS) rather than failing permanently on the first
// attempt — a documented limitation, not a silent one.
func classifyConnError(err error) (ingest.Category, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ingest.CategoryTimeout, "connect or login timed out"
	case errors.Is(err, ErrUnsupportedTLSMode), errors.Is(err, ErrStartTLSUnavailable):
		return ingest.CategoryPolicy, err.Error()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ingest.CategoryTimeout, "connect timed out"
	}
	return ingest.CategoryTransport, "connection failed"
}

func classifyFetchError(err error) (ingest.Category, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return ingest.CategoryTimeout, "fetch timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ingest.CategoryTimeout, "fetch timed out"
	}
	if imap.IsParseError(err) {
		return ingest.CategoryMalformed, "imap server response could not be parsed"
	}
	return ingest.CategoryTransport, "fetch failed"
}

// fetchMailbox EXAMINEs req.Mailbox (never SELECTs it: this package must
// never risk marking mail \Seen or otherwise mutating the mailbox) and
// fetches every new message's ENVELOPE/BODYSTRUCTURE since req.Cursor,
// bounded by maxMessagesPerFetch, oldest first.
func fetchMailbox(c *client.Client, req rpc.Request) ([]rpc.Item, string, error) {
	mbox, err := c.Select(req.Mailbox, true)
	if err != nil {
		return nil, "", err
	}

	cur := resolveCursor(decodeCursor(req.Cursor), mbox.UidValidity)

	seqSet := new(imap.SeqSet)
	if cur.LastUID > 0 {
		seqSet.AddRange(cur.LastUID+1, 0)
	} else {
		seqSet.AddRange(1, 0)
	}

	type fetched struct {
		uid uint32
		msg *imap.Message
	}
	var summaries []fetched
	messages := make(chan *imap.Message, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqSet, []imap.FetchItem{
			imap.FetchUid, imap.FetchEnvelope, imap.FetchBodyStructure, imap.FetchInternalDate,
		}, messages)
	}()
	for msg := range messages {
		summaries = append(summaries, fetched{uid: msg.Uid, msg: msg})
	}
	if err := <-done; err != nil {
		return nil, "", err
	}

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].uid < summaries[j].uid })
	if len(summaries) > maxMessagesPerFetch {
		summaries = summaries[:maxMessagesPerFetch]
	}

	maxChars := req.SnippetMaxChars
	if req.StoreFullBody && req.FullBodyMaxChars > maxChars {
		maxChars = req.FullBodyMaxChars
	}

	items := make([]rpc.Item, 0, len(summaries))
	lastUID := cur.LastUID
	for _, s := range summaries {
		item, err := buildItem(c, s.msg, req, maxChars, mbox.UidValidity)
		if err != nil {
			// A failure partway through the batch must not advance the
			// cursor past unprocessed messages, mirroring
			// internal/ingest.Service.Handle's own "the whole job fails,
			// cursor unchanged" rule for its item loop: the entire
			// mailfetch response fails together, and the next poll
			// retries from cur.LastUID again.
			return nil, "", err
		}
		items = append(items, item)
		lastUID = s.uid
	}

	return items, cursorState{UIDValidity: mbox.UidValidity, LastUID: lastUID}.encode(), nil
}

func buildItem(c *client.Client, msg *imap.Message, req rpc.Request, maxChars int, uidValidity uint32) (rpc.Item, error) {
	receivedAt := msg.Envelope.Date
	if receivedAt.IsZero() {
		receivedAt = msg.InternalDate
	}

	var body string
	if part := selectTextPart(msg.BodyStructure); part != nil {
		raw, err := fetchBodyPart(c, msg.Uid, part.path, req.MaxMessageBytes)
		if err != nil {
			return rpc.Item{}, err
		}
		text := decodePart(raw, part.encoding, part.charset)
		if part.subType == "html" {
			body = textsanitize.StripHTML(text, maxChars)
		} else {
			body = sanitizePlainText(text, maxChars)
		}
	}

	externalID, dedupeKey := identify(req.SourceID, msg.Envelope.MessageId, uidValidity, msg.Uid)
	publishedAt := receivedAt.UTC()

	return rpc.Item{
		ExternalID:  externalID,
		DedupeKey:   dedupeKey,
		PublishedAt: &publishedAt,
		Body:        buildHeaderPrefix(msg.Envelope, receivedAt) + body,
	}, nil
}

// fetchBodyPart issues one UID FETCH BODY.PEEK[path]<0,maxBytes> for a
// single message, using .PEEK so the message's \Seen flag is never set
// (AGENTS.md: "IMAP is read-only by default").
func fetchBodyPart(c *client.Client, uid uint32, path []int, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 1
	}
	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{Path: path},
		Peek:         true,
		Partial:      []int{0, int(maxBytes)},
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqSet, []imap.FetchItem{section.FetchItem()}, messages)
	}()

	var body imap.Literal
	for m := range messages {
		body = m.GetBody(section)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	if body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(body, maxBytes+1))
}

// decodePart reverses a MIME part's Content-Transfer-Encoding and, when
// its declared charset is not already UTF-8/US-ASCII, its charset, using
// github.com/emersion/go-message/charset's wide real-world charset
// coverage. A decoding failure at either stage falls back to whatever was
// successfully decoded so far rather than failing the whole fetch: a
// malformed transfer encoding from an untrusted mail server is exactly
// the kind of input this package must degrade gracefully on, not choke on.
func decodePart(raw []byte, encoding, charset string) string {
	var r io.Reader = bytes.NewReader(raw)
	switch encoding {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	}
	decoded, _ := io.ReadAll(r)

	if charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "us-ascii") {
		if cr, err := mcharset.Reader(charset, bytes.NewReader(decoded)); err == nil {
			if converted, err := io.ReadAll(cr); err == nil {
				decoded = converted
			}
		}
	}
	return string(decoded)
}
