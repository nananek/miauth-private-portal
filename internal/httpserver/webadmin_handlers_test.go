package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/webadmin"
)

// webAdminTestRPID/webAdminTestOrigin are fixed to match
// webAdminRegistrationVector's pre-baked rpIdHash/origin below — not a
// real deployment's LOCAL_ORIGIN, just what the W3C spec test vector
// was generated against.
const (
	webAdminTestRPID   = "example.org"
	webAdminTestOrigin = "https://example.org"
)

type webAdminTestServer struct {
	*Server
	db      *sqlite.DB
	svc     *webadmin.Service
	ownerID string
}

func newWebAdminTestServer(t *testing.T) *webAdminTestServer {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second, MaxOpenConns: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: time.Now()}
	if err := db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	svc, err := webadmin.NewService(db, db.Repos, webadmin.Config{
		RPID: webAdminTestRPID, RPDisplayName: "Test Portal", RPOrigins: []string{webAdminTestOrigin},
		OwnerUsername: "owner", OwnerDisplayName: "Test Owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"}), health.NewRegistry(), Options{WebAdmin: svc})
	return &webAdminTestServer{Server: server, db: db, svc: svc, ownerID: owner.ID}
}

func (ts *webAdminTestServer) issueToken(t *testing.T) string {
	t.Helper()
	raw, _, err := ts.svc.IssueBootstrapToken(t.Context(), ts.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// setVectorSessionData points the bootstrap token identified by raw at
// the fixed challenge/user id webAdminRegistrationVector's response
// body was signed against, bypassing POST /admin/setup/begin's own
// random challenge: there is no way to drive a real
// navigator.credentials.create() browser call from a Go test, so
// handleAdminSetupFinish's wiring is instead exercised against a
// pre-recorded W3C spec test vector (see webAdminRegistrationVector's
// doc comment).
func (ts *webAdminTestServer) setVectorSessionData(t *testing.T, raw, challenge string) {
	t.Helper()
	session := webauthn.SessionData{
		Challenge: challenge,
		UserID:    []byte(ts.ownerID),
		CredParams: []protocol.CredentialParameter{
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
		},
	}
	sessionJSON, err := json.Marshal(&session)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), sha256Hex(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.db.WebAdminBootstrapTokens.SetSessionData(t.Context(), tok.ID, string(sessionJSON), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestHandleAdminSetup_ValidTokenServesRegistrationPage(t *testing.T) {
	ts := newWebAdminTestServer(t)
	raw := ts.issueToken(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/setup?token="+raw, nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "navigator.credentials.create") {
		t.Fatalf("body does not look like the registration page: %q", rec.Body.String())
	}
}

func TestHandleAdminSetup_InvalidTokenServesGenericError(t *testing.T) {
	ts := newWebAdminTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/setup?token=not-a-real-token", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if strings.Contains(rec.Body.String(), "not-a-real-token") {
		t.Fatalf("response reflected the invalid token value: %q", rec.Body.String())
	}
}

func TestHandleAdminSetupBegin_ValidTokenReturnsCredentialCreationJSON(t *testing.T) {
	ts := newWebAdminTestServer(t)
	raw := ts.issueToken(t)
	body, _ := json.Marshal(map[string]string{"token": raw})
	req := httptest.NewRequest(http.MethodPost, "/admin/setup/begin", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var creation protocol.CredentialCreation
	if err := json.Unmarshal(rec.Body.Bytes(), &creation); err != nil {
		t.Fatal(err)
	}
	if len(creation.Response.Challenge) == 0 || creation.Response.RelyingParty.ID != webAdminTestRPID {
		t.Fatalf("creation options = %+v", creation.Response)
	}
}

func TestHandleAdminSetupBegin_InvalidTokenReturns401(t *testing.T) {
	ts := newWebAdminTestServer(t)
	body, _ := json.Marshal(map[string]string{"token": "not-a-real-token"})
	req := httptest.NewRequest(http.MethodPost, "/admin/setup/begin", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAdminSetupFinish_HappyPath_Returns200AndCreatesCredential(t *testing.T) {
	ts := newWebAdminTestServer(t)
	raw := ts.issueToken(t)
	respBody, challenge, credentialID := webAdminRegistrationVector(t)
	ts.setVectorSessionData(t, raw, challenge)

	req := httptest.NewRequest(http.MethodPost, "/admin/setup/finish?token="+raw, bytes.NewReader(respBody))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.OK {
		t.Fatalf("response body = %q, err = %v", rec.Body.String(), err)
	}

	creds, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID)
	if err != nil || len(creds) != 1 || creds[0].CredentialID != base64.RawURLEncoding.EncodeToString(credentialID) {
		t.Fatalf("credentials = %+v, err = %v", creds, err)
	}
}

func TestHandleAdminSetupFinish_TamperedResponseReturns401Or500NotSilentSuccess(t *testing.T) {
	ts := newWebAdminTestServer(t)
	raw := ts.issueToken(t)
	respBody, _, _ := webAdminRegistrationVector(t)
	wrongChallenge := base64.RawURLEncoding.EncodeToString([]byte("not-the-real-challenge-bytes!!!!"))
	ts.setVectorSessionData(t, raw, wrongChallenge)

	req := httptest.NewRequest(http.MethodPost, "/admin/setup/finish?token="+raw, bytes.NewReader(respBody))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("tampered response returned 200: %q", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 401 or 500", rec.Code)
	}
	if creds, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID); err != nil || len(creds) != 0 {
		t.Fatalf("credentials = %+v, err = %v, want none created", creds, err)
	}
}

// sha256Hex duplicates internal/webadmin's unexported hashBootstrapToken
// (same 32-byte-random-token/SHA-256-hash-at-rest shape as
// internal/miauth's hashAPIToken) so this test package can look a
// bootstrap token up by its hash without depending on internal/webadmin's
// unexported API surface.
func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func webAdminRegistrationVector(t *testing.T) (body []byte, challenge string, credentialID []byte) {
	t.Helper()
	const (
		attestationObjectHex = "a363666d74646e6f6e656761747453746d74a068617574684461746158a4bfabc37432958b063360d3ad6461c9c4735ae7f8edd46592a5e0f01452b2e4b559000000008446ccb9ab1db374750b2367ff6f3a1f0020f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4a5010203262001215820afefa16f97ca9b2d23eb86ccb64098d20db90856062eb249c33a9b672f26df61225820930a56b87a2fca66334b03458abf879717c12cc68ed73290af2e2664796b9220"
		clientDataJSONHex    = "7b2274797065223a22776562617574686e2e637265617465222c226368616c6c656e6765223a22414d4d507434557878475453746e63647134313759447742466938767049612d7077386f4f755657345441222c226f726967696e223a2268747470733a2f2f6578616d706c652e6f7267222c2263726f73734f726967696e223a66616c73652c22657874726144617461223a22636c69656e74446174614a534f4e206d617920626520657874656e6465642077697468206164646974696f6e616c206669656c647320696e20746865206675747572652c207375636820617320746869733a20426b5165446a646354427258426941774a544c453551227d"
		credentialIDHex      = "f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4" //nolint:gosec
		challengeHex         = "00c30fb78531c464d2b6771dab8d7b603c01162f2fa486bea70f283ae556e130"
	)

	var err error
	credentialID, err = hex.DecodeString(credentialIDHex)
	if err != nil {
		t.Fatal(err)
	}
	challengeBytes, err := hex.DecodeString(challengeHex)
	if err != nil {
		t.Fatal(err)
	}
	challenge = base64.RawURLEncoding.EncodeToString(challengeBytes)

	attObjBytes, err := hex.DecodeString(attestationObjectHex)
	if err != nil {
		t.Fatal(err)
	}
	cdjBytes, err := hex.DecodeString(clientDataJSONHex)
	if err != nil {
		t.Fatal(err)
	}

	id := base64.RawURLEncoding.EncodeToString(credentialID)
	response := map[string]any{
		"id":    id,
		"rawId": id,
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObjBytes),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cdjBytes),
		},
	}
	body, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return body, challenge, credentialID
}
