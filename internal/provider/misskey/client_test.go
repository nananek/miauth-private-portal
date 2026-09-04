package misskey

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingBody struct {
	reader     *strings.Reader
	closed     bool
	reachedEOF bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF {
		b.reachedEOF = true
	}
	return n, err
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

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

func TestClient_Check_EscapesSessionIDAsOnePathSegment(t *testing.T) {
	client := NewClient("https://misskey.example", 5*time.Second)
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got, want := req.URL.EscapedPath(), "/api/miauth/session%2Fwith%3Fdelimiters/check"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"token":"REDACTED","user":{"id":"owner"}}`)),
			Request:    req,
		}, nil
	})

	if _, ok, err := client.Check(context.Background(), "session/with?delimiters"); err != nil || !ok {
		t.Fatalf("Check() = ok %v, err %v; want success", ok, err)
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

func TestClient_Check_DrainsAndClosesBodyOnEarlyReturn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "non-success status", statusCode: http.StatusBadGateway, body: "upstream failure"},
		{name: "decode error", statusCode: http.StatusOK, body: "not-json trailing response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackingBody{reader: strings.NewReader(tc.body)}
			client := NewClient("https://misskey.example", 5*time.Second)
			client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tc.statusCode,
					Header:     make(http.Header),
					Body:       body,
					Request:    req,
				}, nil
			})

			if _, _, err := client.Check(context.Background(), "session"); err == nil {
				t.Fatal("Check() error = nil, want an error")
			}
			if !body.reachedEOF || !body.closed {
				t.Errorf("response body cleanup = reachedEOF %v, closed %v; want both true", body.reachedEOF, body.closed)
			}
		})
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
