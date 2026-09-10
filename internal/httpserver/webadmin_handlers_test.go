package httpserver

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
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
	dbPath  string
}

// newWebAdminTestServer builds a webAdminTestServer against a fresh
// migrated DB. mutateRepos, if given, is applied to a copy of db.Repos
// before it is handed to webadmin.NewService/miauth.NewService: both
// services store their own domain.Repos VALUE (not a pointer) at
// construction time, so swapping a field on ts.db.Repos afterwards (e.g.
// to inject a failing decorator) would never be seen by the already-built
// service. Tests that need one repository to behave differently for the
// service under test (see
// TestRecordAdminAction_FailureDoesNotFailTheHTTPResponse) must instead
// pass a hook here, before construction.
func newWebAdminTestServer(t *testing.T, mutateRepos ...func(*domain.Repos)) *webAdminTestServer {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 4,
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
	repos := db.Repos
	for _, mutate := range mutateRepos {
		mutate(&repos)
	}
	svc, err := webadmin.NewService(db, repos, webadmin.Config{
		RPID: webAdminTestRPID, RPDisplayName: "Test Portal", RPOrigins: []string{webAdminTestOrigin},
		OwnerUsername: "owner", OwnerDisplayName: "Test Owner",
		// SessionTTL must be nonzero: FinishLogin's Activate call extends
		// a session's expiry to now+sessionTTL, and a zero TTL would
		// leave every login-produced session already expired the
		// instant it is created.
		SessionCookie: webadmin.SessionCookieConfig{SessionTTL: 12 * time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Issue #136 Phase 3's dashboard reads through s.miauth
	// (ListPendingSessions/ListAPITokens/ApproveSession/...), the same
	// already-wired *miauth.Service field RequireScope's own tests
	// construct via miauth.NewService — see miauth_testhelpers_test.go's
	// newMiAuthTestServer. Without this, GET /admin/ (now a real
	// dashboard, not Phase 2's placeholder) would panic on a nil
	// s.miauth.
	miauthSvc := miauth.NewService(db, repos, miauth.Config{
		ClientCallbacks: []string{"aria://aria/miauth"}, OwnerUsername: "owner", OwnerDisplayName: "Test Owner",
	})
	// LocalOrigin matches webAdminTestOrigin (already https://), so
	// Server.adminCookieSecure() reports true here by default — this is
	// the "https:// LOCAL_ORIGIN" variant plan-136-phase2 §9.5 calls for
	// in the Set-Cookie attribute test, not a separate constructor.
	server := NewServer(logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"}), health.NewRegistry(),
		Options{WebAdmin: svc, MiAuthService: miauthSvc, LocalOrigin: webAdminTestOrigin})
	return &webAdminTestServer{Server: server, db: db, svc: svc, ownerID: owner.ID, dbPath: dbPath}
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

// dynamicWebAuthnVector duplicates internal/webadmin's own test-only
// helper of the same name (see that file's doc comment for why a
// dynamically generated ES256 keypair is used instead of go-webauthn's
// fixed W3C spec fixture: that fixture's paired login vector carries
// sign count 0 on both the registration and login side, which would
// make FinishLogin's persisted SignCount byte-identical before/after —
// unable to prove UpdateAfterLogin actually ran). Duplicated rather than
// exported: this package already duplicates webAdminRegistrationVector/
// sha256Hex from internal/webadmin for the same "test-only helper, not
// worth a cross-package export" reason.
type dynamicWebAuthnVector struct {
	priv         *ecdsa.PrivateKey
	credentialID []byte
}

func newDynamicWebAuthnVector(t *testing.T) *dynamicWebAuthnVector {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &dynamicWebAuthnVector{priv: priv, credentialID: []byte("http-dynamic-test-credential-01")}
}

func (v *dynamicWebAuthnVector) coseKey(t *testing.T) []byte {
	t.Helper()
	data, err := webauthncbor.Marshal(map[int64]any{
		1:  int64(webauthncose.EllipticKey),
		3:  int64(webauthncose.AlgES256),
		-1: int64(webauthncose.P256),
		-2: v.priv.PublicKey.X.FillBytes(make([]byte, 32)),
		-3: v.priv.PublicKey.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (v *dynamicWebAuthnVector) authenticatorData(t *testing.T, flags protocol.AuthenticatorFlags, signCount uint32, includeAttestedData bool) []byte {
	t.Helper()
	hash := sha256.Sum256([]byte(webAdminTestRPID))
	data := make([]byte, 0, 64)
	data = append(data, hash[:]...)
	data = append(data, byte(flags))
	data = binary.BigEndian.AppendUint32(data, signCount)
	if includeAttestedData {
		attested := make([]byte, 16, 16+2+len(v.credentialID))
		attested = binary.BigEndian.AppendUint16(attested, uint16(len(v.credentialID))) //nolint:gosec
		attested = append(attested, v.credentialID...)
		attested = append(attested, v.coseKey(t)...)
		data = append(data, attested...)
	}
	return data
}

func (v *dynamicWebAuthnVector) sign(t *testing.T, authData, clientDataJSON []byte) []byte {
	t.Helper()
	clientDataHash := sha256.Sum256(clientDataJSON)
	signed := make([]byte, 0, len(authData)+len(clientDataHash))
	signed = append(signed, authData...)
	signed = append(signed, clientDataHash[:]...)
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, v.priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func dynamicClientDataJSON(t *testing.T, ceremony, challenge string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type": ceremony, "challenge": challenge, "origin": webAdminTestOrigin, "crossOrigin": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (v *dynamicWebAuthnVector) registrationBody(t *testing.T, challenge string, signCount uint32) []byte {
	t.Helper()
	authData := v.authenticatorData(t, protocol.FlagUserPresent|protocol.FlagAttestedCredentialData, signCount, true)
	clientDataJSON := dynamicClientDataJSON(t, "webauthn.create", challenge)
	attestationObject, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": authData,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(v.credentialID)
	body, err := json.Marshal(map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attestationObject),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientDataJSON),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (v *dynamicWebAuthnVector) loginBody(t *testing.T, challenge string, signCount uint32) []byte {
	t.Helper()
	authData := v.authenticatorData(t, protocol.FlagUserPresent, signCount, false)
	clientDataJSON := dynamicClientDataJSON(t, "webauthn.get", challenge)
	sig := v.sign(t, authData, clientDataJSON)
	id := base64.RawURLEncoding.EncodeToString(v.credentialID)
	body, err := json.Marshal(map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientDataJSON),
			"signature":         base64.RawURLEncoding.EncodeToString(sig),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// registerDynamicCredentialViaHTTP drives Phase 1's real
// POST /admin/setup/{begin,finish} handlers end to end so Phase 2's
// login tests below exercise a genuinely stored, HTTP-registered
// credential.
func registerDynamicCredentialViaHTTP(t *testing.T, ts *webAdminTestServer, vector *dynamicWebAuthnVector, challenge string, signCount uint32) {
	t.Helper()
	raw := ts.issueToken(t)
	ts.setVectorSessionData(t, raw, challenge)
	body := vector.registrationBody(t, challenge, signCount)
	req := httptest.NewRequest(http.MethodPost, "/admin/setup/finish?token="+raw, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register credential via HTTP failed: %d %s", rec.Code, rec.Body.String())
	}
}

// loginViaHTTP drives the two-call POST /admin/login/{begin,finish}
// ceremony end to end and returns the admin_session cookie the server
// set on success.
func loginViaHTTP(t *testing.T, ts *webAdminTestServer, vector *dynamicWebAuthnVector, signCount uint32) *http.Cookie {
	t.Helper()
	beginReq := httptest.NewRequest(http.MethodPost, "/admin/login/begin", nil)
	beginRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(beginRec, beginReq)
	if beginRec.Code != http.StatusOK {
		t.Fatalf("login begin failed: %d %s", beginRec.Code, beginRec.Body.String())
	}
	var begin struct {
		SessionID string `json:"sessionId"`
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(beginRec.Body.Bytes(), &begin); err != nil {
		t.Fatal(err)
	}
	loginBody := vector.loginBody(t, begin.PublicKey.Challenge, signCount)
	finishReq := httptest.NewRequest(http.MethodPost, "/admin/login/finish?sessionId="+begin.SessionID, bytes.NewReader(loginBody))
	finishRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(finishRec, finishReq)
	if finishRec.Code != http.StatusOK {
		t.Fatalf("login finish failed: %d %s", finishRec.Code, finishRec.Body.String())
	}
	for _, c := range finishRec.Result().Cookies() {
		if c.Name == adminSessionCookieName {
			return c
		}
	}
	t.Fatal("no admin_session cookie set")
	return nil
}

// extractCSRFToken pulls the CSRF token out of handleAdminIndex's
// <meta name="admin-csrf-token" content="..."> tag — the only way a
// test (or a real browser's own script) can learn the value to echo
// back in X-Admin-CSRF-Token, since the admin_session cookie itself is
// HttpOnly.
func extractCSRFToken(t *testing.T, html string) string {
	t.Helper()
	const marker = `name="admin-csrf-token" content="`
	idx := strings.Index(html, marker)
	if idx < 0 {
		t.Fatalf("csrf meta tag not found in %q", html)
	}
	rest := html[idx+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("malformed csrf meta tag in %q", html)
	}
	return rest[:end]
}

func TestHandleAdminLogin_ServesStaticPageWithNoInterpolation(t *testing.T) {
	ts := newWebAdminTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "navigator.credentials.get") {
		t.Fatalf("body does not look like the login page: %q", rec.Body.String())
	}
}

func TestHandleAdminLoginBegin_NoCredentialsReturns400WithActionableMessage(t *testing.T) {
	ts := newWebAdminTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/login/begin", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "web-login issue") {
		t.Fatalf("body = %q, want an actionable message mentioning web-login issue", rec.Body.String())
	}
}

func TestHandleAdminLoginBegin_ValidReturnsSessionIDAndAssertion(t *testing.T) {
	ts := newWebAdminTestServer(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredentialViaHTTP(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("http-registration-challenge-aaaa")), 0)

	req := httptest.NewRequest(http.MethodPost, "/admin/login/begin", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got struct {
		SessionID string `json:"sessionId"`
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID == "" || got.PublicKey.Challenge == "" {
		t.Fatalf("response = %+v", got)
	}
}

func TestHandleAdminLoginFinish_HappyPath_SetsSessionCookie(t *testing.T) {
	ts := newWebAdminTestServer(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredentialViaHTTP(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("http-registration-challenge-bbbb")), 0)

	cookie := loginViaHTTP(t, ts, vector, 3)
	// Each attribute is asserted individually and by name, not bundled,
	// per plan-136-phase2 §9.5: this is the test that replaces
	// docs/operations/security-regression.md's "Cookie attributes: Not
	// applicable" row with real evidence.
	if cookie.Path != "/admin" {
		t.Errorf("Path = %q, want /admin", cookie.Path)
	}
	if !cookie.HttpOnly {
		t.Error("HttpOnly = false, want true")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", cookie.SameSite)
	}
	if !cookie.Secure {
		t.Error("Secure = false, want true (LOCAL_ORIGIN is https://)")
	}
	if cookie.Value == "" {
		t.Error("empty session token in cookie")
	}
}

func TestHandleAdminLoginFinish_TamperedResponseReturns401NoCookieSet(t *testing.T) {
	ts := newWebAdminTestServer(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredentialViaHTTP(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("http-registration-challenge-cccc")), 0)

	beginReq := httptest.NewRequest(http.MethodPost, "/admin/login/begin", nil)
	beginRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(beginRec, beginReq)
	var begin struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(beginRec.Body.Bytes(), &begin); err != nil {
		t.Fatal(err)
	}

	wrongChallenge := base64.RawURLEncoding.EncodeToString([]byte("not-the-real-challenge-bytes!!!!"))
	loginBody := vector.loginBody(t, wrongChallenge, 1)
	req := httptest.NewRequest(http.MethodPost, "/admin/login/finish?sessionId="+begin.SessionID, bytes.NewReader(loginBody))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminSessionCookieName {
			t.Fatalf("cookie set on a failed login: %+v", c)
		}
	}
}

func TestHandleAdminLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	ts := newWebAdminTestServer(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredentialViaHTTP(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("http-registration-challenge-dddd")), 0)
	cookie := loginViaHTTP(t, ts, vector, 5)

	indexReq := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	indexReq.AddCookie(cookie)
	indexRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(indexRec, indexReq)
	if indexRec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ status = %d, want 200", indexRec.Code)
	}
	csrfToken := extractCSRFToken(t, indexRec.Body.String())

	logoutReq := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutReq.Header.Set("X-Admin-CSRF-Token", csrfToken)
	logoutRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %q", logoutRec.Code, logoutRec.Body.String())
	}

	var cleared *http.Cookie
	for _, c := range logoutRec.Result().Cookies() {
		if c.Name == adminSessionCookieName {
			cleared = c
		}
	}
	if cleared == nil {
		t.Fatal("logout did not set a clearing Set-Cookie")
	}
	// A clearing cookie whose attributes don't match the original is
	// silently ignored by real browsers — this is a correctness
	// assertion, not cosmetic (plan-136-phase2 §9.5).
	if cleared.Path != cookie.Path || cleared.Secure != cookie.Secure ||
		cleared.HttpOnly != cookie.HttpOnly || cleared.SameSite != cookie.SameSite {
		t.Fatalf("clearing cookie attributes = %+v, want matching original %+v", cleared, cookie)
	}
	if cleared.MaxAge >= 0 {
		t.Fatalf("clearing cookie MaxAge = %d, want negative", cleared.MaxAge)
	}

	followUp := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	followUp.AddCookie(cookie)
	followUpRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(followUpRec, followUp)
	if followUpRec.Code != http.StatusUnauthorized {
		t.Fatalf("status after logout with the old cookie = %d, want 401", followUpRec.Code)
	}
}

func TestHandleAdminIndex_RequiresSession(t *testing.T) {
	ts := newWebAdminTestServer(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredentialViaHTTP(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("http-registration-challenge-eeee")), 0)

	unauth := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	unauthRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(unauthRec, unauth)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("status without session = %d, want 401", unauthRec.Code)
	}

	cookie := loginViaHTTP(t, ts, vector, 9)
	authed := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	authed.AddCookie(cookie)
	authedRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(authedRec, authed)
	if authedRec.Code != http.StatusOK {
		t.Fatalf("status with valid session = %d, want 200", authedRec.Code)
	}
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
