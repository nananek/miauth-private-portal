package mailfetch

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

func TestCursor_EncodeDecodeRoundTrip(t *testing.T) {
	s := cursorState{UIDValidity: 42, LastUID: 7}
	got := decodeCursor(s.encode())
	if got != s {
		t.Errorf("decodeCursor(encode()) = %+v, want %+v", got, s)
	}
}

func TestDecodeCursor_EmptyIsZeroValue(t *testing.T) {
	if got := decodeCursor(""); got != (cursorState{}) {
		t.Errorf("decodeCursor(\"\") = %+v, want zero value", got)
	}
}

func TestDecodeCursor_CorruptedIsTreatedAsAbsent(t *testing.T) {
	if got := decodeCursor("not json"); got != (cursorState{}) {
		t.Errorf("decodeCursor(corrupted) = %+v, want zero value (refetch from scratch)", got)
	}
}

func TestResolveCursor_SameUIDValidityIsUnchanged(t *testing.T) {
	cur := cursorState{UIDValidity: 5, LastUID: 100}
	got := resolveCursor(cur, 5)
	if got != cur {
		t.Errorf("resolveCursor = %+v, want unchanged %+v", got, cur)
	}
}

func TestResolveCursor_ChangedUIDValidityResets(t *testing.T) {
	cur := cursorState{UIDValidity: 5, LastUID: 100}
	got := resolveCursor(cur, 6)
	if got != (cursorState{}) {
		t.Errorf("resolveCursor after UIDVALIDITY change = %+v, want zero value", got)
	}
}

func TestResolveCursor_ZeroUIDValidityNeverMismatches(t *testing.T) {
	// A source's first-ever fetch (no cursor yet) must never be treated
	// as a UIDVALIDITY mismatch against a freshly EXAMINE'd mailbox.
	got := resolveCursor(cursorState{}, 5)
	if got != (cursorState{}) {
		t.Errorf("resolveCursor(zero cursor) = %+v, want zero value", got)
	}
}

func TestDecodeHeaderWord_DecodesUTF8Base64EncodedWord(t *testing.T) {
	// "日本語" base64-encoded per RFC 2047.
	got := decodeHeaderWord("=?UTF-8?B?5pel5pys6Kqe?=")
	if got != "日本語" {
		t.Errorf("decodeHeaderWord = %q, want %q", got, "日本語")
	}
}

func TestDecodeHeaderWord_PlainASCIIPassesThrough(t *testing.T) {
	if got := decodeHeaderWord("Hello"); got != "Hello" {
		t.Errorf("decodeHeaderWord = %q, want %q", got, "Hello")
	}
}

func TestDecodeHeaderWord_MalformedFallsBackToRaw(t *testing.T) {
	raw := "=?bogus-charset?Q?broken?="
	if got := decodeHeaderWord(raw); got != raw {
		t.Errorf("decodeHeaderWord(malformed) = %q, want raw input %q unchanged", got, raw)
	}
}

func TestFormatAddress_WithAndWithoutDisplayName(t *testing.T) {
	named := &imap.Address{PersonalName: "Alice", MailboxName: "alice", HostName: "example.com"}
	if got := formatAddress(named); got != "Alice <alice@example.com>" {
		t.Errorf("formatAddress(named) = %q", got)
	}

	bare := &imap.Address{MailboxName: "bob", HostName: "example.com"}
	if got := formatAddress(bare); got != "bob@example.com" {
		t.Errorf("formatAddress(bare) = %q", got)
	}

	if got := formatAddress(nil); got != "" {
		t.Errorf("formatAddress(nil) = %q, want empty", got)
	}
}

func TestBuildHeaderPrefix_IncludesFromSubjectAndDate(t *testing.T) {
	env := &imap.Envelope{
		From:    []*imap.Address{{PersonalName: "Alice", MailboxName: "alice", HostName: "example.com"}},
		Subject: "Hello",
	}
	receivedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	prefix := buildHeaderPrefix(env, receivedAt)
	for _, want := range []string{"From: Alice <alice@example.com>", "Subject: Hello", "2026-01-02T03:04:05Z"} {
		if !strings.Contains(prefix, want) {
			t.Errorf("buildHeaderPrefix() = %q, missing %q", prefix, want)
		}
	}
}

func TestIdentify_PreferMessageIDAndStableAcrossUIDValidity(t *testing.T) {
	extA, keyA := identify("source-1", "<msg-1@example.com>", 10, 100)
	extB, keyB := identify("source-1", "<msg-1@example.com>", 20, 999)
	if extA != extB || keyA != keyB {
		t.Errorf("identify with the same Message-ID under different UIDVALIDITY/UID diverged: (%q,%q) vs (%q,%q)", extA, keyA, extB, keyB)
	}
	if extA != "<msg-1@example.com>" {
		t.Errorf("ExternalID = %q, want the Message-ID itself", extA)
	}
}

func TestIdentify_DifferentSourcesNeverCollide(t *testing.T) {
	_, keyA := identify("source-1", "<msg-1@example.com>", 1, 1)
	_, keyB := identify("source-2", "<msg-1@example.com>", 1, 1)
	if keyA == keyB {
		t.Error("identify produced the same dedupe key for the same Message-ID under two different sources")
	}
}

func TestIdentify_MissingMessageIDFallsBackToUIDValidityAndUID(t *testing.T) {
	ext1, key1 := identify("source-1", "", 10, 100)
	ext2, key2 := identify("source-1", "", 10, 100)
	if ext1 != ext2 || key1 != key2 {
		t.Error("identify's fallback key is not deterministic for the same (source, uidValidity, uid)")
	}
	ext3, key3 := identify("source-1", "", 11, 100)
	if ext1 == ext3 || key1 == key3 {
		t.Error("identify's fallback key did not change when uidValidity changed")
	}
}

func bodyStructure(mimeType, subType, encoding, charset string, parts ...*imap.BodyStructure) *imap.BodyStructure {
	return &imap.BodyStructure{
		MIMEType: mimeType, MIMESubType: subType, Encoding: encoding,
		Params: map[string]string{"charset": charset},
		Parts:  parts,
	}
}

func TestSelectTextPart_SingleTextPlain(t *testing.T) {
	bs := bodyStructure("text", "plain", "quoted-printable", "utf-8")
	part := selectTextPart(bs)
	if part == nil {
		t.Fatal("selectTextPart = nil, want a part")
	}
	if part.subType != "plain" || len(part.path) != 1 || part.path[0] != 1 {
		t.Errorf("part = %+v, want subType=plain path=[1]", part)
	}
}

func TestSelectTextPart_MultipartAlternativePrefersPlain(t *testing.T) {
	bs := bodyStructure("multipart", "alternative", "",
		"",
		bodyStructure("text", "plain", "7bit", "us-ascii"),
		bodyStructure("text", "html", "quoted-printable", "utf-8"),
	)
	part := selectTextPart(bs)
	if part == nil || part.subType != "plain" {
		t.Fatalf("selectTextPart = %+v, want text/plain preferred", part)
	}
}

func TestSelectTextPart_FallsBackToHTMLWhenNoPlainPart(t *testing.T) {
	bs := bodyStructure("multipart", "alternative", "",
		"",
		bodyStructure("text", "html", "quoted-printable", "utf-8"),
	)
	part := selectTextPart(bs)
	if part == nil || part.subType != "html" {
		t.Fatalf("selectTextPart = %+v, want text/html fallback", part)
	}
}

func TestSelectTextPart_NoTextualPartReturnsNil(t *testing.T) {
	bs := bodyStructure("image", "png", "base64", "")
	if part := selectTextPart(bs); part != nil {
		t.Errorf("selectTextPart = %+v, want nil for a non-text message", part)
	}
}

func TestSanitizePlainText_CollapsesWhitespaceAndTruncates(t *testing.T) {
	if got := sanitizePlainText("a\n\n  b   c", 100); got != "a b c" {
		t.Errorf("sanitizePlainText = %q, want %q", got, "a b c")
	}
	if got := sanitizePlainText("0123456789", 5); got != "01234" {
		t.Errorf("sanitizePlainText truncation = %q, want %q", got, "01234")
	}
}
