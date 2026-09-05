package logging

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeHijacker wraps an http.ResponseWriter to additionally implement
// http.Hijacker, standing in for the real net/http ResponseWriter these
// tests cannot construct directly (httptest.NewRecorder does not
// implement http.Hijacker at all, which is exactly why statusRecorder's
// own Hijack needs a dedicated test rather than reusing the httptest
// recorder every other test in this file relies on).
type fakeHijacker struct {
	http.ResponseWriter
	conn net.Conn
	rw   *bufio.ReadWriter
}

func (f *fakeHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return f.conn, f.rw, nil
}

func TestAccessLog_LogsPatternNotRawPath(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	handler := AccessLog(logger, "/miauth/{session}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/miauth/super-secret-session-id?state=leaked-state-value", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, "/miauth/{session}") {
		t.Errorf("expected log to contain route pattern, got: %s", out)
	}
	if strings.Contains(out, "super-secret-session-id") {
		t.Errorf("raw path leaked into log: %s", out)
	}
	if strings.Contains(out, "leaked-state-value") {
		t.Errorf("query string leaked into log: %s", out)
	}
}

func TestAccessLog_NeverLogsHeaders(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	handler := AccessLog(logger, "/api/notes/create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", nil)
	req.Header.Set("Authorization", "Bearer distinctive-token-xyz")
	req.Header.Set("Cookie", "session=distinctive-cookie-abc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if strings.Contains(out, "distinctive-token-xyz") || strings.Contains(out, "distinctive-cookie-abc") {
		t.Errorf("header value leaked into log: %s", out)
	}
}

func TestAccessLog_RecordsMethodAndStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	handler := AccessLog(logger, "/api/notes/create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, `"method":"POST"`) {
		t.Errorf("expected method POST in log, got: %s", out)
	}
	if !strings.Contains(out, `"status":201`) {
		t.Errorf("expected status 201 in log, got: %s", out)
	}
}

func TestAccessLog_LogsCompletionThenRepanicsOnHandlerPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	handler := AccessLog(logger, "/panics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/panics", nil)
	rec := httptest.NewRecorder()

	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("expected AccessLog to re-panic after logging the completion line")
			}
		}()
		handler.ServeHTTP(rec, req)
	}()

	out := buf.String()
	if !strings.Contains(out, "http_request") {
		t.Errorf("expected an http_request completion log even when the handler panics, got: %s", out)
	}
	if !strings.Contains(out, `"status":500`) {
		t.Errorf("expected status 500 logged for a panicking handler, got: %s", out)
	}
}

func TestAccessLog_DefaultsStatusToOKWhenNotExplicitlyWritten(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	handler := AccessLog(logger, "/healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("expected default status 200 in log, got: %s", buf.String())
	}
}

func TestAccessLog_PanicAfterWriteHeaderLogsActualStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	handler := AccessLog(logger, "/panics-after-write", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("boom after headers sent")
	}))

	req := httptest.NewRequest(http.MethodGet, "/panics-after-write", nil)
	rec := httptest.NewRecorder()

	func() {
		defer func() { recover() }()
		handler.ServeHTTP(rec, req)
	}()

	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("expected the already-written 200 status logged, not a hardcoded 500, got: %s", buf.String())
	}
}

func TestStatusRecorder_HijackDelegatesToUnderlyingResponseWriter(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	fake := &fakeHijacker{
		ResponseWriter: httptest.NewRecorder(),
		conn:           server,
		rw:             bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)),
	}

	rec := &statusRecorder{ResponseWriter: fake, status: http.StatusOK}

	hijacker, ok := http.ResponseWriter(rec).(http.Hijacker)
	if !ok {
		t.Fatal("expected *statusRecorder to implement http.Hijacker")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("Hijack() error = %v", err)
	}
	if conn != server {
		t.Error("Hijack() did not return the underlying ResponseWriter's connection")
	}
}

func TestStatusRecorder_HijackErrorsWhenUnderlyingResponseWriterCannotHijack(t *testing.T) {
	// httptest.NewRecorder does not implement http.Hijacker, matching a
	// ResponseWriter type that genuinely cannot be hijacked (e.g. an
	// HTTP/2 stream).
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}

	if _, _, err := rec.Hijack(); err == nil {
		t.Fatal("expected an error when the underlying ResponseWriter does not support hijacking")
	}
}

func TestAccessLog_AllowsHandlerToHijackConnection(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	fake := &fakeHijacker{
		ResponseWriter: httptest.NewRecorder(),
		conn:           server,
		rw:             bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)),
	}

	var hijackErr error
	handler := AccessLog(logger, "/streaming", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			hijackErr = errors.New("ResponseWriter passed through AccessLog does not implement http.Hijacker")
			return
		}
		_, _, hijackErr = hijacker.Hijack()
	}))

	req := httptest.NewRequest(http.MethodGet, "/streaming", nil)
	handler.ServeHTTP(fake, req)

	if hijackErr != nil {
		t.Fatalf("handler could not hijack through AccessLog's statusRecorder: %v", hijackErr)
	}
}

func TestAccessLog_LogsHijackedStatusInsteadOfDefaultingTo200(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	fake := &fakeHijacker{
		ResponseWriter: httptest.NewRecorder(),
		conn:           server,
		rw:             bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)),
	}

	handler := AccessLog(logger, "/streaming", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker := w.(http.Hijacker)
		if _, _, err := hijacker.Hijack(); err != nil {
			t.Errorf("Hijack() error = %v", err)
		}
		// Mirrors streaming_handlers.go: mark the true wire status after a
		// successful upgrade, since Upgrade wrote it directly to the raw
		// connection rather than through w.
		w.(HijackStatusSetter).SetHijackedStatus(http.StatusSwitchingProtocols)
	}))

	req := httptest.NewRequest(http.MethodGet, "/streaming", nil)
	handler.ServeHTTP(fake, req)

	if strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("expected the hijacked status (101) logged, not the unset default 200, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"status":101`) {
		t.Errorf("expected status 101 logged for a hijacked upgrade, got: %s", buf.String())
	}
}

func TestStatusRecorder_SecondWriteHeaderCallDoesNotOverwriteStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})

	handler := AccessLog(logger, "/double-writeheader", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusInternalServerError) // superfluous; net/http keeps the first on the wire
	}))

	req := httptest.NewRequest(http.MethodGet, "/double-writeheader", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("expected the first WriteHeader status (200) to be logged, got: %s", buf.String())
	}
}
