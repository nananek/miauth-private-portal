// Package misskey implements internal/miauth.UpstreamProvider against a
// real upstream Misskey instance's MiAuth check endpoint. It is the
// outbound counterpart to internal/storage/sqlite: a narrow adapter
// behind a domain-owned interface, so internal/miauth never depends on
// net/http directly (AGENTS.md's "put provider boundaries behind narrow
// interfaces" rule). internal/provider is a new top-level directory in
// this repository; docs/roadmap/openwebui.md already uses the same
// "provider/adapter" language for a future outbound integration, which
// this establishes a location for (internal/provider/openwebui).
package misskey

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/miauth"
)

var _ miauth.UpstreamProvider = (*Client)(nil)

// maxCheckResponseBytes bounds how much of the upstream check response
// this client will read, so a misbehaving or compromised upstream
// cannot make this service buffer unbounded memory (AGENTS.md: "bound
// request sizes, timeouts, concurrency, and retry counts").
const maxCheckResponseBytes = 1 << 20 // 1 MiB

// Client calls POST {identityOrigin}/api/miauth/{session}/check.
type Client struct {
	identityOrigin string
	httpClient     *http.Client
}

// NewClient builds a Client against identityOrigin (a scheme+host
// origin with no trailing slash or path, matching ADR-0001's
// IDENTITY_ORIGIN), bounding every request by timeout.
func NewClient(identityOrigin string, timeout time.Duration) *Client {
	return &Client{
		identityOrigin: strings.TrimRight(identityOrigin, "/"),
		httpClient:     &http.Client{Timeout: timeout},
	}
}

// checkResponse decodes only the fields this client needs from
// docs/compat/aria-v1.5.11.md's documented check success shape
// (`{"ok":true,"token":"...","user":{"id":"...", ...}}`). Extra fields
// in the real response (the full UserDetailedNotMe object) are ignored
// by encoding/json, not an error.
type checkResponse struct {
	OK    bool   `json:"ok"`
	Token string `json:"token"`
	User  struct {
		ID string `json:"id"`
	} `json:"user"`
}

// Check implements miauth.UpstreamProvider.
//
// A non-2xx HTTP status is treated as a transport-level failure (err),
// not an ordinary "not yet approved" result: it most likely means the
// request never reached Misskey's MiAuth check logic at all (a
// misconfigured origin, a proxy error, an outage), and conflating that
// with an authoritative not-approved response would let an
// infrastructure problem look identical to a real denial. A 2xx
// response that decodes but does not match the documented
// {"ok":true,...} shape is ok=false, err=nil: the compat doc records
// that Aria's own client does not distinguish pending from denial in
// this response, so this boundary does not invent a distinction the
// wire contract does not make. The exact non-2xx status/error body
// shape is documented as 要実機確認 (needs real-instance verification)
// in docs/compat/aria-v1.5.11.md; this behavior may need revisiting
// once that is verified against a real Misskey instance.
func (c *Client) Check(ctx context.Context, upstreamSessionID string) (string, bool, error) {
	checkURL := fmt.Sprintf("%s/api/miauth/%s/check", c.identityOrigin, url.PathEscape(upstreamSessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkURL, strings.NewReader("{}"))
	if err != nil {
		return "", false, fmt.Errorf("misskey: build check request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("misskey: check request: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("misskey: check request returned status %d", resp.StatusCode)
	}

	var parsed checkResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCheckResponseBytes)).Decode(&parsed); err != nil {
		return "", false, fmt.Errorf("misskey: decode check response: %w", err)
	}

	if !parsed.OK || parsed.Token == "" || parsed.User.ID == "" {
		return "", false, nil
	}
	return parsed.User.ID, true, nil
}

// drainAndClose gives net/http a chance to reuse a keep-alive connection
// when Check returns before decoding the body (for example, a non-2xx
// response) or when the JSON decoder stops at malformed input. The drain
// is bounded because the upstream response is untrusted; an oversized
// response may forfeit connection reuse but cannot make cleanup unbounded.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxCheckResponseBytes+1))
	_ = body.Close()
}
