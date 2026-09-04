package httpserver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

func newTestServer() (*Server, *health.Registry) {
	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	return NewServer(logger, reg, Options{}), reg
}

type failingChecker struct{ name string }

func (c failingChecker) Name() string { return c.name }
func (c failingChecker) Check(_ context.Context) error {
	return errors.New("connection refused")
}

func TestServer_ReadyzLogsReasonWhenNotReady(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	reg.Register(failingChecker{name: "downstream"})
	reg.MarkReady()
	srv := NewServer(logger, reg, Options{})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(buf.String(), "downstream") {
		t.Errorf("expected the failing checker's name in the log, got: %s", buf.String())
	}
}

func TestServer_HealthzAlwaysOK(t *testing.T) {
	srv, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestServer_ReadyzBeforeAndAfterMarkReady(t *testing.T) {
	srv, reg := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz before MarkReady = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	reg.MarkReady()

	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz after MarkReady = %d, want %d", rec.Code, http.StatusOK)
	}

	reg.MarkNotReady()

	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz after MarkNotReady = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestServer_HealthzRejectsUnknownMethod(t *testing.T) {
	srv, _ := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
