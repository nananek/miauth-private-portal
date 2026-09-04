package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

// TestClient_Do_RejectsLoopbackAddressByDefault documents the production
// SSRF policy: without AllowIPForTesting set, a Client refuses to dial a
// loopback address even though httptest.Server binds to one.
func TestClient_Do_RejectsLoopbackAddressByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(Config{MaxRedirects: 3})
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrPolicyViolation) {
		t.Errorf("err = %v, want ErrPolicyViolation", err)
	}
}

func TestClient_Do_AllowsLoopbackWithTestOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := NewClient(Config{
		MaxRedirects:      3,
		AllowInsecureHTTP: true,
		AllowIPForTesting: func(ip net.IP) bool { return ip.IsLoopback() },
	})
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestClient_Do_RejectsRedirectToDisallowedAddress verifies the IP
// policy is enforced at every hop, not only the initial request: the
// test's allow function accepts IPv4 loopback but not IPv6 loopback, and
// the server redirects to an IPv6-loopback literal.
func TestClient_Do_RejectsRedirectToDisallowedAddress(t *testing.T) {
	var redirectTarget string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget, http.StatusFound)
	}))
	defer server.Close()
	redirectTarget = "http://[::1]:1/target"

	client := NewClient(Config{
		MaxRedirects:      3,
		AllowInsecureHTTP: true, // the initial request must reach the redirect, not fail on scheme
		AllowIPForTesting: func(ip net.IP) bool {
			return ip.IsLoopback() && ip.To4() != nil
		},
	})
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrPolicyViolation) {
		t.Errorf("err = %v, want ErrPolicyViolation", err)
	}
	if !strings.Contains(err.Error(), "not a public unicast address") {
		t.Errorf("err = %v, want it to name the IP-address rejection reason (not a scheme rejection)", err)
	}
}

func TestClient_Do_RespectsContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(Config{
		MaxRedirects:      3,
		AllowInsecureHTTP: true,
		AllowIPForTesting: func(net.IP) bool { return true },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestCheckRedirect_RejectsExceededRedirectCount(t *testing.T) {
	fn := checkRedirect(Config{MaxRedirects: 1})
	via := []*http.Request{
		{URL: mustParseURL(t, "https://example.com/a")},
		{URL: mustParseURL(t, "https://example.com/b")},
	}
	req := &http.Request{URL: mustParseURL(t, "https://example.com/c")}
	err := fn(req, via)
	if err == nil || !errors.Is(err, ErrPolicyViolation) {
		t.Errorf("err = %v, want ErrPolicyViolation", err)
	}
}

func TestCheckRedirect_AllowsWithinRedirectLimit(t *testing.T) {
	fn := checkRedirect(Config{MaxRedirects: 2})
	via := []*http.Request{{URL: mustParseURL(t, "https://example.com/a")}}
	req := &http.Request{URL: mustParseURL(t, "https://example.com/b")}
	if err := fn(req, via); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCheckRedirect_RejectsSchemeDowngradeFromHTTPS documents that a
// downgrade is rejected even when AllowInsecureHTTP is true: that flag
// only permits a source's own configured scheme to be http from the
// start, never an in-flight https-to-http redirect (a stronger signal of
// interception).
func TestCheckRedirect_RejectsSchemeDowngradeFromHTTPS(t *testing.T) {
	fn := checkRedirect(Config{MaxRedirects: 5, AllowInsecureHTTP: true})
	via := []*http.Request{{URL: mustParseURL(t, "https://example.com/a")}}
	req := &http.Request{URL: mustParseURL(t, "http://example.com/b")}
	err := fn(req, via)
	if err == nil || !errors.Is(err, ErrPolicyViolation) {
		t.Errorf("err = %v, want ErrPolicyViolation", err)
	}
}

func TestCheckRedirect_AllowsSameSchemeRedirect(t *testing.T) {
	fn := checkRedirect(Config{MaxRedirects: 5})
	via := []*http.Request{{URL: mustParseURL(t, "https://example.com/a")}}
	req := &http.Request{URL: mustParseURL(t, "https://example.com/b")}
	if err := fn(req, via); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckRedirect_RejectsHTTPWithoutAllowInsecureHTTP(t *testing.T) {
	fn := checkRedirect(Config{MaxRedirects: 5})
	req := &http.Request{URL: mustParseURL(t, "http://example.com/b")}
	err := fn(req, nil)
	if err == nil || !errors.Is(err, ErrPolicyViolation) {
		t.Errorf("err = %v, want ErrPolicyViolation", err)
	}
}

func TestClient_Do_RejectsDisallowedSchemeOnInitialRequest(t *testing.T) {
	client := NewClient(Config{MaxRedirects: 3})
	req, err := http.NewRequest(http.MethodGet, "ftp://example.com/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil || !errors.Is(err, ErrPolicyViolation) {
		t.Errorf("err = %v, want ErrPolicyViolation", err)
	}
}

func TestReadLimited_RejectsOversizedBody(t *testing.T) {
	_, err := ReadLimited(strings.NewReader("0123456789"), 5)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestReadLimited_AllowsExactLimit(t *testing.T) {
	data, err := ReadLimited(strings.NewReader("01234"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "01234" {
		t.Errorf("data = %q, want %q", data, "01234")
	}
}

func TestIsPublicUnicastIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"169.254.1.1", false},
		{"0.0.0.0", false},
		{"::1", false},
		{"224.0.0.1", false},
		{"8.8.8.8", true},
		{"1.1.1.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse %q", tt.ip)
			}
			if got := isPublicUnicastIP(ip); got != tt.want {
				t.Errorf("isPublicUnicastIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}
