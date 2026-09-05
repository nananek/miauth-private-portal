package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
)

func TestGetenvDefault(t *testing.T) {
	t.Setenv("MAILFETCH_TEST_KEY", "value")
	if got := getenvDefault("MAILFETCH_TEST_KEY", "default"); got != "value" {
		t.Errorf("getenvDefault(set) = %q, want %q", got, "value")
	}
	if got := getenvDefault("MAILFETCH_TEST_KEY_UNSET", "default"); got != "default" {
		t.Errorf("getenvDefault(unset) = %q, want %q", got, "default")
	}
}

func TestListen_CreatesParentDirAndRestrictsSocketPermissions(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "nested", "mailfetch.sock")

	l, err := listen(socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket permissions = %o, want 0600", perm)
	}
}

func TestListen_RemovesStaleSocketFile(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "mailfetch.sock")
	if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	l, err := listen(socketPath)
	if err != nil {
		t.Fatalf("listen over stale socket file: %v", err)
	}
	defer l.Close()
}

func waitForListening(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing accepting connections at %s", socketPath)
}

func TestServe_HandlesOneRequestThenShutsDownOnCancel(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "mailfetch.sock")
	l, err := listen(socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, l, logger) }()

	waitForListening(t, socketPath)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// An unsupported TLS mode fails fast in internal/mailfetch.dial
	// before any network I/O, so this test needs no fake IMAP server to
	// get a real, well-defined response back.
	req := rpc.Request{Host: "127.0.0.1", Port: 993, TLSMode: "bogus"}
	if err := rpc.WriteFrame(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	var resp rpc.Response
	if err := rpc.ReadFrame(conn, &resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	conn.Close()
	if resp.Error == nil {
		t.Fatal("resp.Error is nil, want a policy error for an unsupported TLS mode")
	}
	if resp.Error.Category != "policy" {
		t.Errorf("resp.Error.Category = %q, want policy", resp.Error.Category)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve() did not return after ctx cancellation")
	}
}
