package mailfetch

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"
)

const (
	testUsername = "username"
	testPassword = "password"
)

// generateTestCert builds a self-signed certificate for 127.0.0.1, valid
// for this process's own tests only. AGENTS.md/ADR-0003 require
// production code to always verify the IMAP server's certificate against
// the real system trust store; this exists so a test server can present
// something for that verification to check, with testRootCAs (see
// imapconn.go) as the one test-only seam that trusts it.
func generateTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// testServer is a real IMAP4rev1 server (github.com/emersion/go-imap/server
// with its in-memory backend) this package's tests run internal/mailfetch
// against, in place of a hand-rolled fake protocol responder that would
// otherwise need to reimplement IMAP's SEARCH/FETCH response grammar
// itself.
type testServer struct {
	addr string
	bkd  *memory.Backend
}

// startTestServer starts a TLS-capable IMAP server listening on
// 127.0.0.1:0 and returns once it is accepting connections, having
// already registered its self-signed certificate in testRootCAs (see
// imapconn.go) for the calling test's duration. When implicitTLS is
// true, the raw listener itself is TLS-wrapped (as a real IMAPS port
// would be); srv.TLSConfig is always set too, so a client's STARTTLS also
// works against the same server regardless of implicitTLS.
func startTestServer(t *testing.T, implicitTLS bool) *testServer {
	t.Helper()
	cert := generateTestCert(t)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	prev := testRootCAs
	testRootCAs = pool
	t.Cleanup(func() { testRootCAs = prev })

	bkd := memory.New()
	srv := server.New(bkd)
	srv.TLSConfig = tlsConfig

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if implicitTLS {
		l = tls.NewListener(l, tlsConfig)
	}

	go func() {
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() { _ = srv.Close() })

	return &testServer{addr: l.Addr().String(), bkd: bkd}
}

// addMessage appends one raw RFC 822 message (headers + CRLF + body,
// caller-formatted) to INBOX on top of memory.New()'s single seeded
// message, which is left in place.
func (ts *testServer) addMessage(t *testing.T, raw string, date time.Time) {
	t.Helper()
	mbox := ts.inbox(t)
	if err := mbox.CreateMessage(nil, date, bytes.NewReader([]byte(raw))); err != nil {
		t.Fatalf("create test message: %v", err)
	}
}

// flagsForUID returns the flags backend/memory currently has recorded for
// the message with the given UID, so a test can assert that fetching
// through internal/mailfetch never sets \Seen (AGENTS.md: "IMAP is
// read-only by default").
func (ts *testServer) flagsForUID(t *testing.T, uid uint32) []string {
	t.Helper()
	mbox := ts.inbox(t)
	for _, m := range mbox.Messages {
		if m.Uid == uid {
			return m.Flags
		}
	}
	t.Fatalf("no message with uid %d", uid)
	return nil
}

func (ts *testServer) inbox(t *testing.T) *memory.Mailbox {
	t.Helper()
	user, err := ts.bkd.Login(nil, testUsername, testPassword)
	if err != nil {
		t.Fatalf("test backend login: %v", err)
	}
	raw, err := user.GetMailbox("INBOX")
	if err != nil {
		t.Fatalf("get test INBOX: %v", err)
	}
	mbox, ok := raw.(*memory.Mailbox)
	if !ok {
		t.Fatalf("test INBOX is %T, want *memory.Mailbox", raw)
	}
	return mbox
}

// startPlainTestServer starts an IMAP server with no TLSConfig at all —
// it never advertises STARTTLS — so a test can verify dial() refuses to
// log in over it rather than silently falling back to plaintext.
func startPlainTestServer(t *testing.T) *testServer {
	t.Helper()
	bkd := memory.New()
	srv := server.New(bkd)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() { _ = srv.Close() })

	return &testServer{addr: l.Addr().String(), bkd: bkd}
}
