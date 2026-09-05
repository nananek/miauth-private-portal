package mailfetch

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
)

func TestDecodePart_QuotedPrintable(t *testing.T) {
	got := decodePart([]byte("Caf=C3=A9"), "quoted-printable", "utf-8")
	if got != "Café" {
		t.Errorf("decodePart(quoted-printable) = %q, want %q", got, "Café")
	}
}

func TestDecodePart_Base64(t *testing.T) {
	// "hello" base64-encoded, with an embedded line break as real mail
	// servers commonly wrap base64 bodies.
	got := decodePart([]byte("aGVs\r\nbG8="), "base64", "utf-8")
	if got != "hello" {
		t.Errorf("decodePart(base64) = %q, want %q", got, "hello")
	}
}

func TestDecodePart_NonUTF8Charset(t *testing.T) {
	// "café" in ISO-8859-1: the same bytes as UTF-8 "caf" plus a single
	// 0xE9 byte for "é".
	got := decodePart([]byte("caf\xe9"), "", "iso-8859-1")
	if got != "café" {
		t.Errorf("decodePart(iso-8859-1) = %q, want %q", got, "café")
	}
}

func TestDecodePart_UnknownEncodingPassesThroughRaw(t *testing.T) {
	got := decodePart([]byte("plain text"), "", "utf-8")
	if got != "plain text" {
		t.Errorf("decodePart(no encoding) = %q, want raw passthrough", got)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassifyConnError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ingest.Category
	}{
		{"deadline exceeded", context.DeadlineExceeded, ingest.CategoryTimeout},
		{"net timeout", timeoutError{}, ingest.CategoryTimeout},
		{"unsupported tls mode", ErrUnsupportedTLSMode, ingest.CategoryPolicy},
		{"starttls unavailable", ErrStartTLSUnavailable, ingest.CategoryPolicy},
		{"other", errors.New("connection reset"), ingest.CategoryTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, msg := classifyConnError(tt.err)
			if cat != tt.want {
				t.Errorf("classifyConnError(%v) category = %q, want %q", tt.err, cat, tt.want)
			}
			if msg == "" {
				t.Error("classifyConnError message is empty")
			}
		})
	}
}

func TestClassifyFetchError(t *testing.T) {
	if cat, _ := classifyFetchError(context.DeadlineExceeded); cat != ingest.CategoryTimeout {
		t.Errorf("classifyFetchError(deadline) = %q, want timeout", cat)
	}
	if cat, _ := classifyFetchError(timeoutError{}); cat != ingest.CategoryTimeout {
		t.Errorf("classifyFetchError(net timeout) = %q, want timeout", cat)
	}
	if cat, _ := classifyFetchError(errors.New("boom")); cat != ingest.CategoryTransport {
		t.Errorf("classifyFetchError(other) = %q, want transport", cat)
	}
}

func baseRequest(addr string) rpc.Request {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return rpc.Request{
		Host:             host,
		Port:             port,
		Username:         testUsername,
		Password:         testPassword,
		Mailbox:          "INBOX",
		SourceID:         "source-1",
		FetchTimeoutMs:   5000,
		MaxMessageBytes:  1 << 20,
		SnippetMaxChars:  2000,
		FullBodyMaxChars: 20000,
	}
}

func TestFetch_ImplicitTLSHappyPath(t *testing.T) {
	ts := startTestServer(t, true)
	req := baseRequest(ts.addr)
	req.TLSMode = "implicit"

	resp := Fetch(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("Fetch() error = %+v", resp.Error)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1 (memory.New()'s single seeded message)", len(resp.Items))
	}
	item := resp.Items[0]
	if item.ExternalID != "<0000000@localhost/>" {
		t.Errorf("ExternalID = %q, want the seeded Message-ID", item.ExternalID)
	}
	if !strings.Contains(item.Body, "Subject: A little message, just for you") {
		t.Errorf("Body = %q, missing Subject header line", item.Body)
	}
	if !strings.Contains(item.Body, "Hi there :)") {
		t.Errorf("Body = %q, missing message text", item.Body)
	}
	if resp.NextCursor == "" {
		t.Error("NextCursor is empty, want an encoded cursor after a successful fetch")
	}
}

func TestFetch_NeverSetsSeenFlag(t *testing.T) {
	ts := startTestServer(t, true)
	// memory.New()'s single seeded message (UID 6) ships with \Seen
	// already set by the fixture itself, which would make this
	// assertion meaningless against it; add a fresh, unseen message
	// instead so this test actually exercises BODY.PEEK's read-only
	// guarantee.
	ts.addMessage(t, "From: sender@example.com\r\n"+
		"Subject: Unseen\r\n"+
		"Message-ID: <unseen@example.com>\r\n"+
		"Content-Type: text/plain\r\n"+
		"\r\n"+
		"Body", time.Now())

	req := baseRequest(ts.addr)
	req.TLSMode = "implicit"
	resp := Fetch(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("Fetch() error = %+v", resp.Error)
	}

	// AGENTS.md: "IMAP is read-only by default and must not mark, move,
	// or delete mail" — BODY.PEEK must never have set \Seen on the
	// message this test just fetched.
	for _, f := range ts.flagsForUID(t, 7) {
		if f == "\\Seen" {
			t.Error("fetching through internal/mailfetch set \\Seen on a message; BODY.PEEK must never do this")
		}
	}
}

func TestFetch_StartTLSHappyPath(t *testing.T) {
	ts := startTestServer(t, false)
	req := baseRequest(ts.addr)
	req.TLSMode = "starttls"

	resp := Fetch(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("Fetch() error = %+v", resp.Error)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(resp.Items))
	}
}

func TestFetch_SecondPollOnlyReturnsNewMessages(t *testing.T) {
	ts := startTestServer(t, true)
	req := baseRequest(ts.addr)
	req.TLSMode = "implicit"

	first := Fetch(context.Background(), req)
	if first.Error != nil {
		t.Fatalf("first Fetch() error = %+v", first.Error)
	}

	ts.addMessage(t, "From: new@example.com\r\n"+
		"Subject: Second message\r\n"+
		"Message-ID: <second@example.com>\r\n"+
		"Content-Type: text/plain\r\n"+
		"\r\n"+
		"New content", time.Now())

	req.Cursor = first.NextCursor
	second := Fetch(context.Background(), req)
	if second.Error != nil {
		t.Fatalf("second Fetch() error = %+v", second.Error)
	}
	if len(second.Items) != 1 {
		t.Fatalf("len(second.Items) = %d, want 1 (only the newly added message)", len(second.Items))
	}
	if second.Items[0].ExternalID != "<second@example.com>" {
		t.Errorf("ExternalID = %q, want the new message's Message-ID", second.Items[0].ExternalID)
	}
}

func TestFetch_MultipartAlternativePrefersPlainAndDecodesQuotedPrintable(t *testing.T) {
	ts := startTestServer(t, true)
	const boundary = "BOUNDARY"
	raw := "From: sender@example.com\r\n" +
		"Subject: =?UTF-8?B?5pel5pys6Kqe?=\r\n" +
		"Message-ID: <multipart@example.com>\r\n" +
		"Content-Type: multipart/alternative; boundary=" + boundary + "\r\n" +
		"\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"Caf=C3=A9\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>Café</p>\r\n" +
		"--" + boundary + "--\r\n"
	ts.addMessage(t, raw, time.Now())

	req := baseRequest(ts.addr)
	req.TLSMode = "implicit"
	resp := Fetch(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("Fetch() error = %+v", resp.Error)
	}

	var got *rpc.Item
	for i := range resp.Items {
		if resp.Items[i].ExternalID == "<multipart@example.com>" {
			got = &resp.Items[i]
		}
	}
	if got == nil {
		t.Fatalf("multipart message not found in %+v", resp.Items)
	}
	if !strings.Contains(got.Body, "Subject: 日本語") {
		t.Errorf("Body = %q, missing decoded RFC 2047 subject", got.Body)
	}
	if !strings.Contains(got.Body, "Café") {
		t.Errorf("Body = %q, missing decoded quoted-printable text/plain part", got.Body)
	}
	if strings.Contains(got.Body, "<p>") {
		t.Errorf("Body = %q, text/plain part should have been preferred over text/html", got.Body)
	}
}

func TestDial_StartTLSUnavailableIsRefused(t *testing.T) {
	ts := startPlainTestServer(t)
	req := baseRequest(ts.addr)
	req.TLSMode = "starttls"

	resp := Fetch(context.Background(), req)
	if resp.Error == nil {
		t.Fatal("Fetch() over a server with no STARTTLS support succeeded, want an error")
	}
	if resp.Error.Category != string(ingest.CategoryPolicy) {
		t.Errorf("Error.Category = %q, want %q", resp.Error.Category, ingest.CategoryPolicy)
	}
}

func TestFetch_WrongPasswordFails(t *testing.T) {
	ts := startTestServer(t, true)
	req := baseRequest(ts.addr)
	req.TLSMode = "implicit"
	req.Password = "wrong"

	resp := Fetch(context.Background(), req)
	if resp.Error == nil {
		t.Fatal("Fetch() with a wrong password succeeded, want an error")
	}
	if len(resp.Items) != 0 {
		t.Errorf("Items = %v, want none on a failed login", resp.Items)
	}
}

func TestFetch_UnreachableHostIsTransport(t *testing.T) {
	req := rpc.Request{
		Host: "127.0.0.1", Port: 1, TLSMode: "implicit",
		Username: "x", Password: "x", Mailbox: "INBOX",
		FetchTimeoutMs: 2000,
	}
	resp := Fetch(context.Background(), req)
	if resp.Error == nil {
		t.Fatal("Fetch() to an unreachable port succeeded, want an error")
	}
	if resp.Error.Category != string(ingest.CategoryTransport) {
		t.Errorf("Error.Category = %q, want %q", resp.Error.Category, ingest.CategoryTransport)
	}
}
