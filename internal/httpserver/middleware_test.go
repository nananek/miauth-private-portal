package httpserver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/logging"
)

func TestWithMaxBody_RejectsOversizedBody(t *testing.T) {
	handler := withMaxBody(8, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("this body is definitely longer than 8 bytes"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestWithMaxBody_AllowsBodyWithinLimit(t *testing.T) {
	handler := withMaxBody(1024, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("small body"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestWithRequestID_SetsNonEmptyContextValue(t *testing.T) {
	var captured string
	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = logging.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured == "" {
		t.Error("expected non-empty request ID in context")
	}
}

func TestWithRequestID_DiffersPerRequest(t *testing.T) {
	var ids []string
	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, logging.RequestIDFromContext(r.Context()))
	}))

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	if ids[0] == ids[1] {
		t.Errorf("expected distinct request IDs, got %q twice", ids[0])
	}
}

func TestWithRecover_ReturnsInternalServerErrorOnPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, logging.Config{Format: "json", Level: "info"})

	handler := withRecover(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(buf.String(), "panic recovered") {
		t.Errorf("expected panic to be logged, got: %s", buf.String())
	}
}

func TestWithRecover_PassesThroughWhenNoPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, logging.Config{Format: "json", Level: "info"})

	handler := withRecover(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output without a panic, got: %s", buf.String())
	}
}
