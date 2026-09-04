// Package safehttp implements an outbound HTTP client that refuses to
// reach a private, loopback, link-local, unspecified, or multicast
// address at any hop of a request, including redirects — AGENTS.md's
// "External fetchers require fixed schemes, host validation, redirect
// limits, and SSRF protections", applied here as the one shared fetcher
// internal/ingest/rss (and, when Issue #12 adds it, any future adapter
// that fetches over HTTP) builds on rather than each adapter
// implementing its own SSRF policy.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ErrPolicyViolation classifies a Client failure that retrying can never
// fix: a disallowed scheme, a disallowed resolved IP address at any hop,
// or an exceeded redirect limit. Callers distinguish this permanent
// class of failure from an ordinary transient network error with
// errors.Is(err, ErrPolicyViolation).
var ErrPolicyViolation = errors.New("safehttp: policy violation")

const dialTimeout = 10 * time.Second

// Config bounds one Client's redirect and scheme policy.
type Config struct {
	// MaxRedirects is the maximum number of redirect hops followed. 0
	// disallows any redirect.
	MaxRedirects int
	// AllowInsecureHTTP permits the request's own scheme (and, unless a
	// redirect would downgrade from https, any same-scheme redirect) to
	// be "http" instead of requiring "https". A redirect from https to
	// http is always rejected regardless of this flag: an in-flight
	// downgrade is a stronger signal of interception than an operator
	// having explicitly configured a plain-http source from the start.
	AllowInsecureHTTP bool
	// AllowIPForTesting overrides the default public-unicast-only IP
	// policy this Client enforces at every hop. It exists only so tests
	// can point a Client at an httptest.Server bound to 127.0.0.1;
	// internal/config.RSSConfig exposes no operator-facing setting that
	// maps to this field, and cmd/server never sets it, so a production
	// deployment always uses the default policy no matter what an
	// operator configures.
	AllowIPForTesting func(net.IP) bool
}

// Client is a *http.Client wrapper enforcing Config's scheme/redirect/IP
// policy. Callers bound request duration themselves with
// context.WithTimeout: Client sets no fixed http.Client.Timeout, so a
// caller polling several sources with different budgets can size each
// request's ctx independently.
type Client struct {
	cfg        Config
	httpClient *http.Client
}

// NewClient builds a production Client that only ever reaches public
// unicast IP addresses.
func NewClient(cfg Config) *Client {
	allow := isPublicUnicastIP
	if cfg.AllowIPForTesting != nil {
		allow = cfg.AllowIPForTesting
	}

	dialer := &net.Dialer{Timeout: dialTimeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid address %q: %v", ErrPolicyViolation, addr, err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve host: %w", err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("%w: no addresses resolved", ErrPolicyViolation)
			}
			for _, ip := range ips {
				if !allow(ip.IP) {
					return nil, fmt.Errorf("%w: resolved address is not a public unicast address", ErrPolicyViolation)
				}
			}
			// Dial the already-validated IP directly rather than
			// letting the dialer re-resolve addr: a second DNS lookup
			// between the check above and the connection itself could
			// return a different, unvalidated address (DNS rebinding).
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > cfg.MaxRedirects {
				return fmt.Errorf("%w: exceeded %d redirects", ErrPolicyViolation, cfg.MaxRedirects)
			}
			if err := validateScheme(req.URL.Scheme, cfg.AllowInsecureHTTP); err != nil {
				return err
			}
			if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect downgrades scheme from https to %q", ErrPolicyViolation, req.URL.Scheme)
			}
			return nil
		},
	}
	return &Client{cfg: cfg, httpClient: client}
}

// Do validates req's own scheme, then performs it, following redirects
// under the same policy. The caller is responsible for bounding
// response size (see ReadLimited) and for closing the returned
// response's body.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if err := validateScheme(req.URL.Scheme, c.cfg.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

func validateScheme(scheme string, allowInsecureHTTP bool) error {
	if scheme == "https" {
		return nil
	}
	if scheme == "http" && allowInsecureHTTP {
		return nil
	}
	return fmt.Errorf("%w: scheme %q not allowed", ErrPolicyViolation, scheme)
}

// isPublicUnicastIP is the default, production IP policy: only a
// globally routable unicast address may be dialed.
func isPublicUnicastIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return true
}

// ReadLimited reads at most maxBytes from r, returning an error instead
// of the partial read result when the response is larger, so a caller
// never buffers an oversized or unbounded body in memory (AGENTS.md:
// "bound request sizes, timeouts, concurrency, and retry counts").
func ReadLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("safehttp: read response body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("safehttp: response exceeds %d byte limit", maxBytes)
	}
	return data, nil
}
