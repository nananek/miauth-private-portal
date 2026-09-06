package mailfetch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	mcharset "github.com/emersion/go-message/charset"
)

// cursorState is this adapter's opaque cursor JSON, round-tripped through
// internal/ingest/imap. LastUID only ever advances after every message up
// to it has been durably processed by internal/ingest.Service — see
// fetchMailbox's "the whole batch fails together" behavior.
type cursorState struct {
	UIDValidity uint32 `json:"uidValidity"`
	LastUID     uint32 `json:"lastUid"`
}

// decodeCursor parses raw as a cursorState. A missing or corrupted cursor
// (never written by this package, or from a previous, incompatible
// version) is treated as absent — a corrupted cursor must never
// permanently wedge a source — rather than failing the fetch, mirroring
// internal/ingest/rss.Adapter.Fetch's same treatment of its own cursor.
func decodeCursor(raw string) cursorState {
	if raw == "" {
		return cursorState{}
	}
	var s cursorState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return cursorState{}
	}
	return s
}

// resolveCursor returns cur adjusted for the mailbox's current
// UIDVALIDITY: a cursor recorded under a different (non-zero) UIDVALIDITY
// than currentUidValidity means the mailbox was recreated or renumbered
// since, so previously-recorded UIDs are no longer comparable and
// fetching starts over from UID 1 — messages re-fetched this way still
// dedupe correctly downstream by Message-ID (see identify), which is the
// mechanism behind Issue #12's "a UIDVALIDITY change does not duplicate
// timeline entries" acceptance criterion. A zero-value cur.UIDValidity (a
// source's first-ever fetch, or a corrupted cursor decodeCursor already
// reset) is never treated as a mismatch.
func resolveCursor(cur cursorState, currentUidValidity uint32) cursorState {
	if cur.UIDValidity != 0 && cur.UIDValidity != currentUidValidity {
		return cursorState{}
	}
	return cur
}

func (s cursorState) encode() string {
	b, err := json.Marshal(s)
	if err != nil {
		// s is always a plain struct of two uint32s; Marshal cannot fail.
		return ""
	}
	return string(b)
}

// wordDecoder decodes RFC 2047 encoded-words (subjects and display names
// commonly arrive as "=?charset?B?...?="); its CharsetReader delegates to
// go-message/charset, which covers a much wider set of real-world mail
// charsets (ISO-2022-JP, Shift_JIS, GBK, ...) than mime.WordDecoder's
// built-in utf-8/iso-8859-1/us-ascii.
var wordDecoder = &mime.WordDecoder{CharsetReader: mcharset.Reader}

// decodeHeaderWord best-effort decodes an RFC 2047 encoded-word header
// value. A value that fails to decode (malformed encoding, an unknown
// charset) is returned as-is rather than failing the whole fetch: a
// slightly garbled subject is a cosmetic issue, not a correctness one.
func decodeHeaderWord(raw string) string {
	decoded, err := wordDecoder.DecodeHeader(raw)
	if err != nil {
		return raw
	}
	return decoded
}

// formatAddress renders one envelope address as "Display Name
// <mailbox@host>", or just "mailbox@host" when there is no display name.
func formatAddress(addr *imap.Address) string {
	if addr == nil {
		return ""
	}
	mailbox := addr.Address()
	name := strings.TrimSpace(decodeHeaderWord(addr.PersonalName))
	if name == "" {
		return mailbox
	}
	return name + " <" + mailbox + ">"
}

// formatFrom joins every From address (rare for a single message to have
// more than one, but ENVELOPE always returns a list).
func formatFrom(addrs []*imap.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if f := formatAddress(a); f != "" {
			parts = append(parts, f)
		}
	}
	return strings.Join(parts, ", ")
}

// buildHeaderPrefix renders sender/subject/received time as plain text
// ahead of the sanitized body snippet. This is the only place
// From/Subject/received time reach Item.Body: internal/ingest.Service's
// composeExternalBody (Issue #13) does read FetchedItem.Title, but only
// to fold it into a generic "[<kind>] <title>" header — it has no notion
// of From/Subject/Date structure, so an IMAP adapter that set Title
// instead of writing this prefix directly into Body would still lose
// the sender/subject/received-time shape Issue #12's "store sender,
// subject, received time" acceptance criterion requires. Folding them
// into Body here is what actually satisfies it.
func buildHeaderPrefix(env *imap.Envelope, receivedAt time.Time) string {
	var sb strings.Builder
	sb.WriteString("From: ")
	sb.WriteString(formatFrom(env.From))
	sb.WriteString("\nSubject: ")
	sb.WriteString(decodeHeaderWord(env.Subject))
	sb.WriteString("\nDate: ")
	sb.WriteString(receivedAt.UTC().Format(time.RFC3339))
	sb.WriteString("\n\n")
	return sb.String()
}

// sanitizePlainText normalizes a text/plain part the same way
// textsanitize.StripHTML normalizes text/html: collapsed whitespace,
// bounded length. text/plain has no markup to strip, but still needs the
// same untrusted-content bounding (AGENTS.md: "treat...mail...as
// untrusted data").
func sanitizePlainText(raw string, maxChars int) string {
	text := strings.Join(strings.Fields(raw), " ")
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) > maxChars {
		runes = runes[:maxChars]
	}
	return string(runes)
}

// identify derives ExternalID and DedupeKey for one message. messageID is
// the envelope's Message-ID header (RFC 5322), stable across a
// UIDVALIDITY change and therefore preferred: re-fetching the same
// message after a UIDVALIDITY reset reuses the same ExternalID/DedupeKey,
// so internal/timeline.Service.CreateExternalEntry's dedupe (unique on
// (source_id, external_id) and on dedupe_key) recognizes it as the same
// item rather than creating a duplicate timeline entry — the mechanism
// behind Issue #12's "UIDVALIDITY change does not duplicate entries"
// acceptance criterion. A message with no Message-ID (permitted, if rare,
// under RFC 5322) falls back to a hash of source+UIDVALIDITY+UID, which
// is stable only until the next UIDVALIDITY change — a documented, narrow
// limitation for that rare case.
func identify(sourceID, messageID string, uidValidity, uid uint32) (externalID, dedupeKey string) {
	h := sha256.New()
	h.Write([]byte(sourceID))
	h.Write([]byte{'|'})
	messageID = strings.TrimSpace(messageID)
	if messageID != "" {
		h.Write([]byte(messageID))
		return messageID, hex.EncodeToString(h.Sum(nil))
	}
	fallback := strconv.FormatUint(uint64(uidValidity), 10) + ":" + strconv.FormatUint(uint64(uid), 10)
	h.Write([]byte(fallback))
	key := hex.EncodeToString(h.Sum(nil))
	return key, key
}

// selectTextPart walks bs looking for a text/plain part, falling back to
// text/html when no text/plain part exists (the common
// multipart/alternative shape). It returns nil when the message has no
// textual part at all (for example a message whose only content is a
// non-text attachment, which this package never fetches — AGENTS.md: no
// attachment indexing).
func selectTextPart(bs *imap.BodyStructure) *textPart {
	if bs == nil {
		return nil
	}
	var plain, html *textPart
	bs.Walk(func(path []int, part *imap.BodyStructure) bool {
		if !strings.EqualFold(part.MIMEType, "text") {
			return true
		}
		tp := &textPart{
			path:     append([]int(nil), path...),
			subType:  strings.ToLower(part.MIMESubType),
			encoding: strings.ToLower(part.Encoding),
			charset:  part.Params["charset"],
		}
		switch tp.subType {
		case "plain":
			if plain == nil {
				plain = tp
			}
		case "html":
			if html == nil {
				html = tp
			}
		}
		return true
	})
	if plain != nil {
		return plain
	}
	return html
}

type textPart struct {
	path     []int
	subType  string
	encoding string
	charset  string
}
