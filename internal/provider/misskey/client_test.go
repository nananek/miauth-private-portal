package misskey

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 5*time.Second), srv
}

func TestClient_Check_Success(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/miauth/sess-1/check") {
			t.Errorf("path = %s, want suffix /api/miauth/sess-1/check", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"token":"REDACTED","user":{"id":"upstream-user-1","username":"owner"}}`))
	})

	userID, ok, err := client.Check(t.Context(), "sess-1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !ok {
		t.Fatal("Check() ok = false, want true")
	}
	if userID != "upstream-user-1" {
		t.Errorf("userID = %q, want upstream-user-1", userID)
	}
}

func TestClient_Check_NotOK(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false}`))
	})

	_, ok, err := client.Check(t.Context(), "sess-1")
	if err != nil {
		t.Fatalf("Check: unexpected error %v", err)
	}
	if ok {
		t.Error("Check() ok = true, want false")
	}
}

func TestClient_Check_MalformedBodyIsNotOK(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	})

	_, ok, err := client.Check(t.Context(), "sess-1")
	if err != nil {
		t.Fatalf("Check: unexpected error %v", err)
	}
	if ok {
		t.Error("Check() ok = true, want false for a response missing ok/token/user.id")
	}
}

func TestClient_Check_NonJSONBodyIsAnError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html>not json</html>`))
	})

	_, ok, err := client.Check(t.Context(), "sess-1")
	if err == nil {
		t.Fatal("Check() expected a decode error, got nil")
	}
	if ok {
		t.Error("Check() ok = true, want false alongside the error")
	}
}

func TestClient_Check_NonSuccessStatusIsAnError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	_, ok, err := client.Check(t.Context(), "sess-1")
	if err == nil {
		t.Fatal("Check() expected an error for a 502 response, got nil")
	}
	if ok {
		t.Error("Check() ok = true, want false alongside the error")
	}
}

func TestClient_Check_TimeoutIsAnError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"token":"t","user":{"id":"u"}}`))
	})
	client.httpClient.Timeout = 10 * time.Millisecond

	_, ok, err := client.Check(t.Context(), "sess-1")
	if err == nil {
		t.Fatal("Check() expected a timeout error, got nil")
	}
	if ok {
		t.Error("Check() ok = true, want false alongside the timeout error")
	}
}

func TestClient_Check_ResponseBodyIsBounded(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Well past maxCheckResponseBytes; the client must not hang or
		// exhaust memory reading this.
		_, _ = w.Write([]byte(`{"padding":"`))
		buf := make([]byte, maxCheckResponseBytes*2)
		for i := range buf {
			buf[i] = 'a'
		}
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte(`"}`))
	})

	_, ok, err := client.Check(t.Context(), "sess-1")
	if err == nil {
		t.Fatal("Check() expected a decode error for an oversized, truncated body, got nil")
	}
	if ok {
		t.Error("Check() ok = true, want false alongside the error")
	}
}
