package imap

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
)

// fakeMailfetch speaks internal/mailfetch/rpc's protocol over a real Unix
// domain socket without any real IMAP server behind it: this package's
// own responsibility is request-building and response-interpretation
// (the actual IMAP/MIME logic is internal/mailfetch's, tested there — see
// this ticket's plan section 10's two-tier test split).
type fakeMailfetch struct {
	l net.Listener

	mu      sync.Mutex
	lastReq rpc.Request
}

func startFakeMailfetch(t *testing.T, handler func(rpc.Request) rpc.Response) (socketPath string, srv *fakeMailfetch) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mailfetch.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeMailfetch{l: l}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var req rpc.Request
				if err := rpc.ReadFrame(conn, &req); err != nil {
					return
				}
				s.mu.Lock()
				s.lastReq = req
				s.mu.Unlock()
				_ = rpc.WriteFrame(conn, handler(req))
			}()
		}
	}()

	return path, s
}

func (s *fakeMailfetch) capturedRequest() rpc.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReq
}

func testConfig(socketPath string) Config {
	return Config{
		Host: "imap.example.com", Port: 993, TLSMode: "implicit",
		Username: "owner", Password: "hunter2", Mailbox: "INBOX",
		SocketPath: socketPath, FetchTimeout: 2 * time.Second,
		MaxMessageBytes: 1 << 20, SnippetMaxChars: 2000, FullBodyMaxChars: 20000,
	}
}

func TestAdapter_Fetch_ReturnsItemsAndCursor(t *testing.T) {
	published := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	socketPath, _ := startFakeMailfetch(t, func(req rpc.Request) rpc.Response {
		return rpc.Response{
			Items:      []rpc.Item{{ExternalID: "<a@example.com>", DedupeKey: "key-a", PublishedAt: &published, Body: "From: a\n\nHi"}},
			NextCursor: `{"uidValidity":1,"lastUid":5}`,
		}
	})

	adapter := NewAdapter(testConfig(socketPath))
	source := domain.ExternalSource{ID: "source-1", Kind: Kind}

	result, err := adapter.Fetch(context.Background(), source, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].ExternalID != "<a@example.com>" {
		t.Fatalf("Items = %+v", result.Items)
	}
	if result.NextCursor != `{"uidValidity":1,"lastUid":5}` {
		t.Errorf("NextCursor = %q", result.NextCursor)
	}
}

func TestAdapter_Fetch_SendsConfigAndSourceAndCursor(t *testing.T) {
	socketPath, srv := startFakeMailfetch(t, func(req rpc.Request) rpc.Response {
		return rpc.Response{}
	})

	adapter := NewAdapter(testConfig(socketPath))
	source := domain.ExternalSource{ID: "source-1", Kind: Kind}
	cursor := `{"uidValidity":1,"lastUid":5}`

	if _, err := adapter.Fetch(context.Background(), source, &cursor); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got := srv.capturedRequest()
	want := testConfig(socketPath)
	if got.Host != want.Host || got.Port != want.Port || got.TLSMode != want.TLSMode ||
		got.Username != want.Username || got.Password != want.Password || got.Mailbox != want.Mailbox {
		t.Errorf("request config fields = %+v, want matching %+v", got, want)
	}
	if got.SourceID != source.ID {
		t.Errorf("SourceID = %q, want %q", got.SourceID, source.ID)
	}
	if got.Cursor != cursor {
		t.Errorf("Cursor = %q, want %q", got.Cursor, cursor)
	}
	if got.FetchTimeoutMs != want.FetchTimeout.Milliseconds() {
		t.Errorf("FetchTimeoutMs = %d, want %d", got.FetchTimeoutMs, want.FetchTimeout.Milliseconds())
	}
}

func TestAdapter_Fetch_NilCursorSendsEmptyString(t *testing.T) {
	socketPath, srv := startFakeMailfetch(t, func(req rpc.Request) rpc.Response { return rpc.Response{} })
	adapter := NewAdapter(testConfig(socketPath))

	if _, err := adapter.Fetch(context.Background(), domain.ExternalSource{ID: "s"}, nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := srv.capturedRequest().Cursor; got != "" {
		t.Errorf("Cursor = %q, want empty for a nil cursor", got)
	}
}

func TestAdapter_Fetch_ServerErrorIsClassified(t *testing.T) {
	socketPath, _ := startFakeMailfetch(t, func(req rpc.Request) rpc.Response {
		return rpc.Response{Error: &rpc.ErrorInfo{Category: string(ingest.CategoryClientError), Message: "authentication failed"}}
	})
	adapter := NewAdapter(testConfig(socketPath))

	_, err := adapter.Fetch(context.Background(), domain.ExternalSource{ID: "s"}, nil)
	if err == nil {
		t.Fatal("Fetch returned nil error, want a classified FetchError")
	}
	if got := ingest.ClassifyFetchError(err); got != ingest.CategoryClientError {
		t.Errorf("ClassifyFetchError = %q, want %q", got, ingest.CategoryClientError)
	}
}

func TestAdapter_Fetch_MailfetchNotRunningIsTransport(t *testing.T) {
	adapter := NewAdapter(testConfig(filepath.Join(t.TempDir(), "does-not-exist.sock")))

	_, err := adapter.Fetch(context.Background(), domain.ExternalSource{ID: "s"}, nil)
	if err == nil {
		t.Fatal("Fetch against a missing socket succeeded, want an error")
	}
	if got := ingest.ClassifyFetchError(err); got != ingest.CategoryTransport {
		t.Errorf("ClassifyFetchError = %q, want %q", got, ingest.CategoryTransport)
	}
}

func TestAdapter_Fetch_MalformedResponseIsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailfetch.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("not json\n"))
	}()

	adapter := NewAdapter(testConfig(path))
	_, err = adapter.Fetch(context.Background(), domain.ExternalSource{ID: "s"}, nil)
	if err == nil {
		t.Fatal("Fetch with a malformed response succeeded, want an error")
	}
	if got := ingest.ClassifyFetchError(err); got != ingest.CategoryMalformed {
		t.Errorf("ClassifyFetchError = %q, want %q", got, ingest.CategoryMalformed)
	}
}

func TestAdapter_Fetch_TimesOutWhenMailfetchNeverResponds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailfetch.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	block := make(chan struct{})
	t.Cleanup(func() { _ = l.Close(); close(block) })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the request (so the client's write does not block) but
		// never respond, to exercise the client's own timeout.
		var req rpc.Request
		_ = rpc.ReadFrame(conn, &req)
		<-block
	}()

	cfg := testConfig(path)
	cfg.FetchTimeout = 100 * time.Millisecond
	adapter := NewAdapter(cfg)

	start := time.Now()
	_, err = adapter.Fetch(context.Background(), domain.ExternalSource{ID: "s"}, nil)
	if err == nil {
		t.Fatal("Fetch against a server that never responds succeeded, want an error")
	}
	// FetchTimeout(100ms) + connectSlack(5s) bounds this; allow generous
	// scheduling headroom above that without asserting exact timing.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Fetch took %v to time out, want it bounded by FetchTimeout+connectSlack", elapsed)
	}
}

func TestAdapter_Kind(t *testing.T) {
	if (&Adapter{}).Kind() != "imap" {
		t.Errorf("Kind() = %q, want imap", (&Adapter{}).Kind())
	}
}
