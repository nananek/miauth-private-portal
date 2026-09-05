package mailfetch

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-imap/client"

	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
)

// testRootCAs, when non-nil, replaces the system default trust store for
// every TLS handshake dial performs. It is set only from this package's
// own _test.go files (see fetch_test.go), so tests can trust a local,
// self-signed test certificate; it is never exposed through
// internal/config or rpc.Request, mirroring
// internal/ingest/safehttp.Config.AllowIPForTesting's test-only seam —
// production code always leaves this nil and gets the default system
// trust store.
var testRootCAs *x509.CertPool

// ErrUnsupportedTLSMode reports an unrecognized Request.TLSMode. There is
// deliberately no plaintext option (ADR-0003, AGENTS.md): only "implicit"
// and "starttls" are ever accepted.
var ErrUnsupportedTLSMode = errors.New("mailfetch: unsupported tls mode")

// ErrStartTLSUnavailable reports that TLSMode is "starttls" but the
// server did not advertise STARTTLS support: connecting anyway would send
// req.Password in the clear, so the connection is refused instead.
var ErrStartTLSUnavailable = errors.New("mailfetch: server does not support STARTTLS")

// ErrLoginFailed reports that the server accepted the connection (and,
// for "starttls", the TLS upgrade) but rejected the LOGIN command
// itself — a tagged NO or BAD response to c.Login. classifyConnError
// maps this to ingest.CategoryClientError (permanent) rather than
// CategoryTransport: retrying the exact same credentials against the
// same account cannot succeed, and unlike a transient network blip,
// retrying it anyway on internal/jobs' backoff schedule — and then
// again from scratch on every ingest.Scheduler poll tick, since nothing
// disables a source after repeated failures — risks tripping the mail
// provider's own failed-login lockout. dial reaches this classification
// without needing go-imap's raw *imap.StatusResp.Type (NO vs BAD): that
// distinction is only observable via the low-level Client.Execute path,
// which does not update Client's private auth-state bookkeeping the way
// Client.Login does, so using it here would leave the connection unable
// to Select/Examine afterwards. Tagging the error by which step
// produced it (post-connect, at LOGIN) is sufficient and much simpler.
var ErrLoginFailed = errors.New("mailfetch: imap server rejected login")

// dial connects to req's IMAP server, negotiates TLS per req.TLSMode, and
// logs in. On any error the caller owns no connection to close.
func dial(req rpc.Request, timeout time.Duration) (*client.Client, error) {
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	dialer := &net.Dialer{Timeout: timeout}
	tlsConfig := &tls.Config{ServerName: req.Host, RootCAs: testRootCAs}

	var c *client.Client
	var err error
	switch req.TLSMode {
	case "implicit":
		c, err = client.DialWithDialerTLS(dialer, addr, tlsConfig)
	case "starttls":
		c, err = client.DialWithDialer(dialer, addr)
		if err == nil {
			var ok bool
			ok, err = c.SupportStartTLS()
			if err == nil && !ok {
				err = ErrStartTLSUnavailable
			}
			if err == nil {
				err = c.StartTLS(tlsConfig)
			}
			if err != nil {
				_ = c.Logout()
			}
		}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedTLSMode, req.TLSMode)
	}
	if err != nil {
		return nil, err
	}

	c.Timeout = timeout
	if err := c.Login(req.Username, req.Password); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("%w: %w", ErrLoginFailed, err)
	}
	return c, nil
}
