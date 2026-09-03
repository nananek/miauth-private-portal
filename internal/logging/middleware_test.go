package logging

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
