package webadmin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// testRPID/testRPOrigin are fixed to match webauthnRegistrationVector's
// pre-baked rpIdHash/origin below: they are not this deployment's real
// LOCAL_ORIGIN, just the values the W3C spec test vector was generated
// against.
const (
	testRPID     = "example.org"
	testRPOrigin = "https://example.org"
	// testSessionTTL stands in for ADMIN_SESSION_TTL in every Phase 2
	// test service: long enough that no test's fixed fakeClock ever
	// drifts past it by accident, and long enough to be trivially
	// distinguishable from loginCeremonyTTL (5m) in assertions that
	// check FinishLogin extended expiry to the *long* window.
	testSessionTTL = 12 * time.Hour
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type testService struct {
	*Service
	db      *sqlite.DB
	clock   *fakeClock
	ownerID string
}

func newTestService(t *testing.T) *testService {
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
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	svc, err := NewService(db, db.Repos, Config{
		RPID: testRPID, RPDisplayName: "Test Portal", RPOrigins: []string{testRPOrigin},
		OwnerUsername: "owner", OwnerDisplayName: "Test Owner", Clock: clock,
		SessionCookie: SessionCookieConfig{SessionTTL: testSessionTTL},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testService{Service: svc, db: db, clock: clock, ownerID: owner.ID}
}

func (ts *testService) issue(t *testing.T) (raw string, tok domain.WebAdminBootstrapToken) {
	t.Helper()
	raw, tok, err := ts.IssueBootstrapToken(t.Context(), ts.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	return raw, tok
}

func TestIssueBootstrapToken_CreatesIssuedTokenWithTenMinuteTTL(t *testing.T) {
	ts := newTestService(t)
	raw, tok := ts.issue(t)
	if raw == "" {
		t.Fatal("empty raw token")
	}
	if tok.Status != domain.WebAdminBootstrapIssued {
		t.Fatalf("status = %q, want issued", tok.Status)
	}
	if !tok.ExpiresAt.Equal(tok.CreatedAt.Add(bootstrapTokenTTL)) {
		t.Fatalf("ExpiresAt = %v, want CreatedAt+%v", tok.ExpiresAt, bootstrapTokenTTL)
	}
	if tok.OwnerActorID != ts.ownerID {
		t.Fatalf("OwnerActorID = %q, want %q", tok.OwnerActorID, ts.ownerID)
	}
	stored, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil || stored.ID != tok.ID {
		t.Fatalf("stored token = %+v, err = %v", stored, err)
	}
}

func TestCheckBootstrapToken_ValidTokenSucceeds(t *testing.T) {
	ts := newTestService(t)
	raw, _ := ts.issue(t)
	if err := ts.CheckBootstrapToken(t.Context(), raw); err != nil {
		t.Fatalf("CheckBootstrapToken = %v, want nil", err)
	}
}

func TestCheckBootstrapToken_UnknownTokenIsInvalid(t *testing.T) {
	ts := newTestService(t)
	if err := ts.CheckBootstrapToken(t.Context(), "not-a-real-token"); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("err = %v, want ErrBootstrapTokenInvalid", err)
	}
}

func TestCheckBootstrapToken_ExpiredTokenIsInvalid(t *testing.T) {
	ts := newTestService(t)
	raw, _ := ts.issue(t)
	ts.clock.Advance(bootstrapTokenTTL + time.Second)
	if err := ts.CheckBootstrapToken(t.Context(), raw); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("err = %v, want ErrBootstrapTokenInvalid", err)
	}
}

func TestCheckBootstrapToken_ConsumedTokenIsInvalid(t *testing.T) {
	ts := newTestService(t)
	raw, tok := ts.issue(t)
	if _, err := ts.db.WebAdminBootstrapTokens.Consume(t.Context(), tok.ID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ts.CheckBootstrapToken(t.Context(), raw); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("err = %v, want ErrBootstrapTokenInvalid", err)
	}
}

func TestBeginRegistration_UnknownOrExpiredTokenReturnsErrBootstrapTokenInvalid(t *testing.T) {
	ts := newTestService(t)
	if _, err := ts.BeginRegistration(t.Context(), "not-a-real-token"); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("unknown token err = %v, want ErrBootstrapTokenInvalid", err)
	}
	raw, _ := ts.issue(t)
	ts.clock.Advance(bootstrapTokenTTL + time.Second)
	if _, err := ts.BeginRegistration(t.Context(), raw); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("expired token err = %v, want ErrBootstrapTokenInvalid", err)
	}
}

func TestBeginRegistration_PersistsSessionDataAgainstToken(t *testing.T) {
	ts := newTestService(t)
	raw, tok := ts.issue(t)
	creation, err := ts.BeginRegistration(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if creation == nil {
		t.Fatal("nil creation options")
	}
	stored, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != tok.ID || stored.WebAuthnSessionData == nil {
		t.Fatalf("stored token = %+v, want non-nil WebAuthnSessionData", stored)
	}
}

func TestBeginRegistration_SecondCallOverwritesFirstSessionData(t *testing.T) {
	ts := newTestService(t)
	raw, tok := ts.issue(t)
	if _, err := ts.BeginRegistration(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	first, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.BeginRegistration(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	second, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != domain.WebAdminBootstrapIssued {
		t.Fatalf("status after second BeginRegistration = %q, want issued", second.Status)
	}
	if *first.WebAuthnSessionData == *second.WebAuthnSessionData {
		t.Fatal("second BeginRegistration reused the first call's challenge/session data")
	}
	if tok.ID != first.ID {
		t.Fatal("bad test fixture")
	}
}

func TestFinishRegistration_RequiresPriorBeginRegistration(t *testing.T) {
	ts := newTestService(t)
	raw, _ := ts.issue(t)
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(nil))}
	if _, err := ts.FinishRegistration(t.Context(), raw, req); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("err = %v, want ErrBootstrapTokenInvalid", err)
	}
}

func TestFinishRegistration_ExpiredBetweenBeginAndFinish(t *testing.T) {
	ts := newTestService(t)
	raw, _ := ts.issue(t)
	if _, err := ts.BeginRegistration(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(bootstrapTokenTTL + time.Second)
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(nil))}
	if _, err := ts.FinishRegistration(t.Context(), raw, req); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("err = %v, want ErrBootstrapTokenInvalid", err)
	}
	stored, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.WebAdminBootstrapIssued {
		t.Fatalf("status = %q, want issued (never consumed)", stored.Status)
	}
	if creds, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID); err != nil || len(creds) != 0 {
		t.Fatalf("credentials = %+v, err = %v, want none created", creds, err)
	}
}

// setVectorSessionData points tok's stored session data at the fixed
// challenge/user id webauthnRegistrationVector's response body was
// signed against, bypassing BeginRegistration's own random challenge —
// there is no way to make a real navigator.credentials.create() browser
// call in a unit test, so the ceremony's second half is exercised
// against a pre-recorded W3C spec test vector instead (see
// webauthnRegistrationVector's doc comment).
func setVectorSessionData(t *testing.T, ts *testService, tok domain.WebAdminBootstrapToken, challenge string) {
	t.Helper()
	session := &webauthn.SessionData{
		Challenge: challenge,
		UserID:    []byte(ts.ownerID),
		CredParams: []protocol.CredentialParameter{
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
		},
	}
	sessionJSON, err := encodeSessionData(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.db.WebAdminBootstrapTokens.SetSessionData(t.Context(), tok.ID, sessionJSON, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestFinishRegistration_HappyPath_ConsumesTokenAndCreatesCredential(t *testing.T) {
	ts := newTestService(t)
	raw, tok := ts.issue(t)
	body, challenge, credentialID := webauthnRegistrationVector(t)
	setVectorSessionData(t, ts, tok, challenge)

	req := &http.Request{Body: io.NopCloser(bytes.NewReader(body))}
	cred, err := ts.FinishRegistration(t.Context(), raw, req)
	if err != nil {
		t.Fatal(err)
	}
	wantCredentialID := base64.RawURLEncoding.EncodeToString(credentialID)
	if cred.CredentialID != wantCredentialID || cred.OwnerActorID != ts.ownerID {
		t.Fatalf("credential = %+v, want CredentialID=%q OwnerActorID=%q", cred, wantCredentialID, ts.ownerID)
	}

	stored, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil || stored.Status != domain.WebAdminBootstrapConsumed {
		t.Fatalf("token status = %+v, err = %v, want consumed", stored, err)
	}
	creds, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("credentials = %+v, err = %v, want exactly one", creds, err)
	}

	// A second FinishRegistration with the same (now-consumed) token must
	// fail, never create a second credential.
	req2 := &http.Request{Body: io.NopCloser(bytes.NewReader(body))}
	if _, err := ts.FinishRegistration(t.Context(), raw, req2); !errors.Is(err, ErrBootstrapTokenInvalid) {
		t.Fatalf("second FinishRegistration err = %v, want ErrBootstrapTokenInvalid", err)
	}
	if creds, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID); err != nil || len(creds) != 1 {
		t.Fatalf("credentials after replay = %+v, err = %v, want still exactly one", creds, err)
	}
}

func TestFinishRegistration_TamperedResponseFailsCeremony(t *testing.T) {
	ts := newTestService(t)
	raw, tok := ts.issue(t)
	body, _, _ := webauthnRegistrationVector(t)
	// A wrong challenge simulates a tampered/replayed response: the
	// library's signature-independent "none" attestation format has no
	// signature over clientDataJSON to re-verify directly, but the
	// challenge embedded in clientDataJSON is checked against
	// SessionData.Challenge, so mismatching them here reproduces the
	// same class of failure (response doesn't match what was asked for)
	// without needing to forge or break a real signature.
	wrongChallenge := base64.RawURLEncoding.EncodeToString([]byte("not-the-real-challenge-bytes!!!!"))
	setVectorSessionData(t, ts, tok, wrongChallenge)

	req := &http.Request{Body: io.NopCloser(bytes.NewReader(body))}
	if _, err := ts.FinishRegistration(t.Context(), raw, req); !errors.Is(err, ErrCeremonyFailed) {
		t.Fatalf("err = %v, want ErrCeremonyFailed", err)
	}

	stored, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil || stored.Status != domain.WebAdminBootstrapIssued {
		t.Fatalf("token status = %+v, err = %v, want still issued (never consumed)", stored, err)
	}
	if creds, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID); err != nil || len(creds) != 0 {
		t.Fatalf("credentials = %+v, err = %v, want none created", creds, err)
	}
}

// dynamicWebAuthnVector generates a fresh ES256 (P-256) keypair bound to
// testRPID/testRPOrigin and can produce both a "none"-attestation
// registration response and a matching login assertion response, each
// with its own explicit signature counter.
//
// This is deliberately NOT webauthnRegistrationVector's fixed W3C spec
// fixture: go-webauthn's own paired login vector for that fixture
// (login_test.go's testLoginSpecVectorNoneES256) carries authenticator
// counter 0 on both the registration and the login side, so replaying
// it here would leave every Authenticator field (SignCount, Flags)
// byte-identical before and after FinishLogin — a test built on it could
// not tell a correct UpdateAfterLogin call apart from a silently-skipped
// one, which is exactly the bug plan-136-phase2 flags as the single most
// important correctness detail in this phase. Generating our own pair
// lets the login side's counter differ from the registration side's, so
// the persisted Authenticator.SignCount can be asserted to actually
// change end to end (see TestFinishLogin_HappyPath_... below).
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
	return &dynamicWebAuthnVector{priv: priv, credentialID: []byte("dynamic-test-credential-id-00001")}
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
	hash := sha256.Sum256([]byte(testRPID))
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

func webauthnClientDataJSON(t *testing.T, ceremony, challenge string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type": ceremony, "challenge": challenge, "origin": testRPOrigin, "crossOrigin": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// registrationBody builds a "none"-attestation registration response
// for this vector's credential, flagged UserPresent|AttestedCredentialData
// (no BackupEligible — loginBody's flags must match on that bit, per
// go-webauthn's own "Backup Eligible flag inconsistency" check).
func (v *dynamicWebAuthnVector) registrationBody(t *testing.T, challenge string, signCount uint32) []byte {
	t.Helper()
	authData := v.authenticatorData(t, protocol.FlagUserPresent|protocol.FlagAttestedCredentialData, signCount, true)
	clientDataJSON := webauthnClientDataJSON(t, "webauthn.create", challenge)
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

// loginBody builds an assertion response for this vector's credential,
// flagged UserPresent only (matching registrationBody's BackupEligible=
// false), signed with signCount as the authenticator's reported counter.
func (v *dynamicWebAuthnVector) loginBody(t *testing.T, challenge string, signCount uint32) []byte {
	t.Helper()
	authData := v.authenticatorData(t, protocol.FlagUserPresent, signCount, false)
	clientDataJSON := webauthnClientDataJSON(t, "webauthn.get", challenge)
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

// registerDynamicCredential runs the real bootstrap-token registration
// ceremony (Phase 1's own flow) end to end for vector, so
// BeginLogin/FinishLogin tests below exercise a genuinely stored
// credential rather than a hand-inserted row.
func registerDynamicCredential(t *testing.T, ts *testService, vector *dynamicWebAuthnVector, challenge string, signCount uint32) domain.WebAdminCredential {
	t.Helper()
	raw, tok := ts.issue(t)
	setVectorSessionData(t, ts, tok, challenge)
	body := vector.registrationBody(t, challenge, signCount)
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(body))}
	cred, err := ts.FinishRegistration(t.Context(), raw, req)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestBeginLogin_NoCredentialsRegisteredReturnsErrNoCredentialsRegistered(t *testing.T) {
	ts := newTestService(t)
	if _, _, err := ts.BeginLogin(t.Context()); !errors.Is(err, ErrNoCredentialsRegistered) {
		t.Fatalf("err = %v, want ErrNoCredentialsRegistered", err)
	}
}

func TestBeginLogin_CreatesPendingSessionWithFiveMinuteTTL(t *testing.T) {
	ts := newTestService(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredential(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("registration-challenge-aaaaaaaa")), 0)

	_, sessionID, err := ts.BeginLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	session, err := ts.db.WebAdminSessions.Get(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != domain.WebAdminSessionPending {
		t.Fatalf("status = %q, want pending", session.Status)
	}
	if !session.ExpiresAt.Equal(session.CreatedAt.Add(loginCeremonyTTL)) {
		t.Fatalf("ExpiresAt = %v, want CreatedAt+%v", session.ExpiresAt, loginCeremonyTTL)
	}
	if session.WebAuthnSessionData == nil {
		t.Fatal("WebAuthnSessionData not set")
	}
}

func TestFinishLogin_HappyPath_ActivatesSessionAndPersistsUpdatedCredential(t *testing.T) {
	ts := newTestService(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredential(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("registration-challenge-bbbbbbbb")), 0)

	before, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID)
	if err != nil || len(before) != 1 {
		t.Fatalf("credentials before login = %+v, err = %v", before, err)
	}
	var beforeCred webauthn.Credential
	if err := json.Unmarshal([]byte(before[0].CredentialJSON), &beforeCred); err != nil {
		t.Fatal(err)
	}
	if beforeCred.Authenticator.SignCount != 0 {
		t.Fatalf("registration sign count = %d, want 0", beforeCred.Authenticator.SignCount)
	}

	assertion, sessionID, err := ts.BeginLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// signCount 7 deliberately differs from registration's 0 — see
	// dynamicWebAuthnVector's own doc comment for why this must not be
	// the fixed W3C vector pair.
	loginBody := vector.loginBody(t, assertion.Response.Challenge.String(), 7)
	before2 := ts.clock.Now()
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(loginBody))}
	rawToken, session, err := ts.FinishLogin(t.Context(), sessionID, req)
	if err != nil {
		t.Fatal(err)
	}
	if rawToken == "" {
		t.Fatal("empty raw session token")
	}
	if session.Status != domain.WebAdminSessionActive || session.SessionTokenHash == nil || session.CSRFToken == nil {
		t.Fatalf("session after FinishLogin = %+v", session)
	}
	if !session.ExpiresAt.Equal(before2.Add(testSessionTTL)) {
		t.Fatalf("ExpiresAt = %v, want %v (the long sessionTTL, not loginCeremonyTTL)", session.ExpiresAt, before2.Add(testSessionTTL))
	}

	after, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID)
	if err != nil || len(after) != 1 {
		t.Fatalf("credentials after login = %+v, err = %v", after, err)
	}
	// This is the assertion that would have caught a missed
	// UpdateAfterLogin call: not just "login succeeded", but that the
	// stored credential row's own JSON actually changed.
	if after[0].CredentialJSON == before[0].CredentialJSON {
		t.Fatal("UpdateAfterLogin did not change the stored credential JSON — sign-count/clone-detection bookkeeping was not persisted")
	}
	var afterCred webauthn.Credential
	if err := json.Unmarshal([]byte(after[0].CredentialJSON), &afterCred); err != nil {
		t.Fatal(err)
	}
	if afterCred.Authenticator.SignCount != 7 {
		t.Fatalf("SignCount after login = %d, want 7", afterCred.Authenticator.SignCount)
	}
	if after[0].LastUsedAt == nil {
		t.Fatal("LastUsedAt not set after login")
	}
}

func TestFinishLogin_TamperedResponseFailsCeremony_SessionStaysPending(t *testing.T) {
	ts := newTestService(t)
	vector := newDynamicWebAuthnVector(t)
	registerDynamicCredential(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("registration-challenge-cccccccc")), 0)

	before, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID)
	if err != nil || len(before) != 1 {
		t.Fatalf("credentials = %+v, err = %v", before, err)
	}

	_, sessionID, err := ts.BeginLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A wrong challenge simulates a tampered/replayed response, the same
	// technique TestFinishRegistration_TamperedResponseFailsCeremony
	// uses for registration.
	wrongChallenge := base64.RawURLEncoding.EncodeToString([]byte("not-the-real-challenge-bytes!!!!"))
	loginBody := vector.loginBody(t, wrongChallenge, 1)
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(loginBody))}
	if _, _, err := ts.FinishLogin(t.Context(), sessionID, req); !errors.Is(err, ErrCeremonyFailed) {
		t.Fatalf("err = %v, want ErrCeremonyFailed", err)
	}

	pending, err := ts.db.WebAdminSessions.Get(t.Context(), sessionID)
	if err != nil || pending.Status != domain.WebAdminSessionPending {
		t.Fatalf("session = %+v, err = %v, want still pending", pending, err)
	}
	after, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID)
	if err != nil || len(after) != 1 || after[0].CredentialJSON != before[0].CredentialJSON || after[0].LastUsedAt != nil {
		t.Fatalf("credentials after tampered login = %+v, err = %v, want unchanged (no partial commit)", after, err)
	}
}

// TestFinishLogin_CredentialRevokedBeforeCeremonyStartsFailsCleanly covers
// the coarser case: RevokeCredential's Delete lands any time between
// BeginLogin and FinishLogin being invoked at all, so FinishLogin's own
// ListByOwner call (loading credentials to hand the WebAuthn library for
// verification) already sees an empty list. go-webauthn itself then
// rejects the assertion — "User does not own all credentials from the
// allowed credential list" — before this service ever reaches its own
// UpdateAfterLogin/Activate transaction. This is a different, wider
// window than TestFinishLogin_CredentialRevokedBetweenCredentialLoadAndCommitReturnsErrSessionInvalid
// below, which pins down the narrower race FinishLogin's own transaction
// comment specifically reasons about.
func TestFinishLogin_CredentialRevokedBeforeCeremonyStartsFailsCleanly(t *testing.T) {
	ts := newTestService(t)
	vector := newDynamicWebAuthnVector(t)
	cred := registerDynamicCredential(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("registration-challenge-dddddddd")), 0)

	assertion, sessionID, err := ts.BeginLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Simulates RevokeCredential's Delete winning the race against this
	// ceremony's own FinishLogin call, in the window between BeginLogin
	// and here (the session's CredentialID is still nil, so
	// RevokeAllByCredential's own cascade cannot have touched it).
	if err := ts.db.WebAdminCredentials.Delete(t.Context(), cred.ID); err != nil {
		t.Fatal(err)
	}

	loginBody := vector.loginBody(t, assertion.Response.Challenge.String(), 7)
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(loginBody))}
	if _, _, err := ts.FinishLogin(t.Context(), sessionID, req); !errors.Is(err, ErrCeremonyFailed) {
		t.Fatalf("err = %v, want ErrCeremonyFailed (go-webauthn itself rejects an empty allowed-credential list)", err)
	}

	pending, err := ts.db.WebAdminSessions.Get(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status == domain.WebAdminSessionActive {
		t.Fatal("session was activated despite its credential being deleted mid-ceremony — it would outlive the revoke that was supposed to cut it off")
	}
}

// listThenDeleteCredentialRepo wraps a real WebAdminCredentialRepository
// and deletes credentialRowID immediately after ListByOwner returns —
// landing RevokeCredential's Delete in the exact window FinishLogin's
// own UpdateAfterLogin/Activate transaction comment reasons about:
// after FinishLogin's pre-verification credential load (whose result the
// WebAuthn library still verifies against), but before that transaction
// runs. A real concurrent goroutine can't be made to land in a window
// that narrow deterministically; this decorator can.
type listThenDeleteCredentialRepo struct {
	domain.WebAdminCredentialRepository
	t               *testing.T
	credentialRowID string
}

func (r listThenDeleteCredentialRepo) ListByOwner(ctx context.Context, ownerActorID string) ([]domain.WebAdminCredential, error) {
	r.t.Helper()
	creds, err := r.WebAdminCredentialRepository.ListByOwner(ctx, ownerActorID)
	if err != nil {
		return nil, err
	}
	if err := r.WebAdminCredentialRepository.Delete(ctx, r.credentialRowID); err != nil {
		r.t.Fatal(err)
	}
	return creds, nil
}

// TestFinishLogin_CredentialRevokedBetweenCredentialLoadAndCommitReturnsErrSessionInvalid
// pins down the narrow race FinishLogin's own transaction comment
// reasons through, precisely: the WebAuthn ceremony itself succeeds
// (verified against credentials loaded a moment earlier), but
// RevokeCredential's Delete lands before this service's own
// UpdateAfterLogin/Activate transaction starts. UpdateAfterLogin (keyed
// by credentialID) then fails with zero rows affected, aborting Activate
// in the same transaction, so the session never transitions to active
// bound to a credential that no longer exists — the exact outcome
// RevokeAllByCredential's own cascade could never otherwise catch, since
// this session was still 'pending' (CredentialID nil) at the moment the
// credential was deleted. It also pins the specific error to
// ErrSessionInvalid, not just non-nil: this race is expected,
// security-correct behavior whenever it fires, and must read to the
// HTTP layer as an ordinary failed login (401), not an opaque 500.
func TestFinishLogin_CredentialRevokedBetweenCredentialLoadAndCommitReturnsErrSessionInvalid(t *testing.T) {
	ts := newTestService(t)
	vector := newDynamicWebAuthnVector(t)
	cred := registerDynamicCredential(t, ts, vector, base64.RawURLEncoding.EncodeToString([]byte("registration-challenge-eeeeeeee")), 0)

	assertion, sessionID, err := ts.BeginLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	racyRepos := ts.db.Repos
	racyRepos.WebAdminCredentials = listThenDeleteCredentialRepo{
		WebAdminCredentialRepository: racyRepos.WebAdminCredentials,
		t:                            t, credentialRowID: cred.ID,
	}
	racySvc, err := NewService(ts.db, racyRepos, Config{
		RPID: testRPID, RPDisplayName: "Test Portal", RPOrigins: []string{testRPOrigin},
		OwnerUsername: "owner", OwnerDisplayName: "Test Owner", Clock: ts.clock,
		SessionCookie: SessionCookieConfig{SessionTTL: testSessionTTL},
	})
	if err != nil {
		t.Fatal(err)
	}

	loginBody := vector.loginBody(t, assertion.Response.Challenge.String(), 7)
	req := &http.Request{Body: io.NopCloser(bytes.NewReader(loginBody))}
	if _, _, err := racySvc.FinishLogin(t.Context(), sessionID, req); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid (a clean failed-login response, not an opaque 500)", err)
	}

	pending, err := ts.db.WebAdminSessions.Get(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status == domain.WebAdminSessionActive {
		t.Fatal("session was activated despite its credential being deleted mid-transaction — it would outlive the revoke that was supposed to cut it off")
	}
}

func TestFinishLogin_UnknownOrExpiredOrAlreadyActiveSessionIDReturnsErrSessionInvalid(t *testing.T) {
	ts := newTestService(t)
	req := func() *http.Request { return &http.Request{Body: io.NopCloser(bytes.NewReader(nil))} }

	if _, _, err := ts.FinishLogin(t.Context(), "not-a-real-session-id", req()); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("unknown session id err = %v, want ErrSessionInvalid", err)
	}

	expiredData, err := encodeSessionData(&webauthn.SessionData{Challenge: "x", UserID: []byte(ts.ownerID)})
	if err != nil {
		t.Fatal(err)
	}
	expired := domain.WebAdminSession{
		ID: domain.NewID(), OwnerActorID: ts.ownerID, Status: domain.WebAdminSessionPending,
		WebAuthnSessionData: &expiredData,
		CreatedAt:           ts.clock.Now().Add(-2 * loginCeremonyTTL), ExpiresAt: ts.clock.Now().Add(-loginCeremonyTTL),
	}
	if err := ts.db.WebAdminSessions.Create(t.Context(), expired); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.FinishLogin(t.Context(), expired.ID, req()); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired session id err = %v, want ErrSessionInvalid", err)
	}

	activeHash := hashSessionToken("already-active-token")
	csrf := "csrf"
	active := domain.WebAdminSession{
		ID: domain.NewID(), OwnerActorID: ts.ownerID, Status: domain.WebAdminSessionActive,
		SessionTokenHash: &activeHash, CSRFToken: &csrf,
		CreatedAt: ts.clock.Now(), ExpiresAt: ts.clock.Now().Add(time.Hour),
	}
	if err := ts.db.WebAdminSessions.Create(t.Context(), active); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.FinishLogin(t.Context(), active.ID, req()); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("already-active session id err = %v, want ErrSessionInvalid", err)
	}
}

// createActiveSession directly inserts an active WebAdminSession row for
// raw's hash, bypassing BeginLogin/FinishLogin's own random tokens —
// VerifySession/Logout/RevokeCredential's tests below only need a
// session in a known state, not a freshly-completed ceremony.
func createActiveSession(t *testing.T, ts *testService, raw string, createdAt, expiresAt time.Time, credentialID *string) domain.WebAdminSession {
	t.Helper()
	hash := hashSessionToken(raw)
	csrf := "csrf-" + raw
	sess := domain.WebAdminSession{
		ID: domain.NewID(), OwnerActorID: ts.ownerID, CredentialID: credentialID, Status: domain.WebAdminSessionActive,
		SessionTokenHash: &hash, CSRFToken: &csrf, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
	if err := ts.db.WebAdminSessions.Create(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestVerifySession_ActiveUnexpiredSessionSucceeds(t *testing.T) {
	ts := newTestService(t)
	sess := createActiveSession(t, ts, "raw-active", ts.clock.Now(), ts.clock.Now().Add(time.Hour), nil)
	got, err := ts.VerifySession(t.Context(), "raw-active")
	if err != nil || got.ID != sess.ID {
		t.Fatalf("VerifySession = %+v, err = %v", got, err)
	}
}

func TestVerifySession_ExpiredSessionFails(t *testing.T) {
	ts := newTestService(t)
	createActiveSession(t, ts, "raw-expired", ts.clock.Now().Add(-2*time.Hour), ts.clock.Now().Add(-time.Hour), nil)
	if _, err := ts.VerifySession(t.Context(), "raw-expired"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid", err)
	}
}

func TestVerifySession_RevokedSessionFails(t *testing.T) {
	ts := newTestService(t)
	sess := createActiveSession(t, ts, "raw-revoked", ts.clock.Now(), ts.clock.Now().Add(time.Hour), nil)
	if err := ts.db.WebAdminSessions.Revoke(t.Context(), sess.ID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.VerifySession(t.Context(), "raw-revoked"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid", err)
	}
}

func TestVerifySession_UnknownTokenFails(t *testing.T) {
	ts := newTestService(t)
	if _, err := ts.VerifySession(t.Context(), "no-such-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("err = %v, want ErrSessionInvalid", err)
	}
}

func TestLogout_RevokesSession_SubsequentVerifySessionFails(t *testing.T) {
	ts := newTestService(t)
	sess := createActiveSession(t, ts, "raw-logout", ts.clock.Now(), ts.clock.Now().Add(time.Hour), nil)
	if _, err := ts.VerifySession(t.Context(), "raw-logout"); err != nil {
		t.Fatal(err)
	}
	if err := ts.Logout(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.VerifySession(t.Context(), "raw-logout"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("err after logout = %v, want ErrSessionInvalid", err)
	}
}

func TestLogout_AlreadyRevokedOrExpiredSessionIsANoOpNotAnError(t *testing.T) {
	ts := newTestService(t)
	if err := ts.Logout(t.Context(), "unknown-session-id"); err != nil {
		t.Fatalf("Logout on unknown session id = %v, want nil (no-op)", err)
	}
	sess := createActiveSession(t, ts, "raw-double-logout", ts.clock.Now(), ts.clock.Now().Add(time.Hour), nil)
	if err := ts.Logout(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := ts.Logout(t.Context(), sess.ID); err != nil {
		t.Fatalf("second Logout = %v, want nil (idempotent)", err)
	}
}

func TestRevokeCredential_DeletesCredentialAndCascadesToActiveSessions(t *testing.T) {
	ts := newTestService(t)
	now := ts.clock.Now()
	cred1 := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: ts.ownerID, CredentialID: "cred-revoke-1",
		CredentialJSON: `{"id":"cred-revoke-1"}`, CreatedAt: now,
	}
	cred2 := domain.WebAdminCredential{
		ID: domain.NewID(), OwnerActorID: ts.ownerID, CredentialID: "cred-revoke-2",
		CredentialJSON: `{"id":"cred-revoke-2"}`, CreatedAt: now,
	}
	for _, c := range []domain.WebAdminCredential{cred1, cred2} {
		if err := ts.db.WebAdminCredentials.Create(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	createActiveSession(t, ts, "raw-cred1-session", now, now.Add(time.Hour), &cred1.CredentialID)
	createActiveSession(t, ts, "raw-cred2-session", now, now.Add(time.Hour), &cred2.CredentialID)

	result, err := ts.RevokeCredential(t.Context(), cred1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.RevokedSessions != 1 {
		t.Fatalf("RevokedSessions = %d, want 1", result.RevokedSessions)
	}
	if _, err := ts.db.WebAdminCredentials.Get(t.Context(), cred1.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cred1 Get err = %v, want ErrNotFound", err)
	}
	if _, err := ts.VerifySession(t.Context(), "raw-cred1-session"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("cred1's session VerifySession err = %v, want ErrSessionInvalid", err)
	}
	if _, err := ts.VerifySession(t.Context(), "raw-cred2-session"); err != nil {
		t.Fatalf("cred2's session VerifySession err = %v, want nil (unaffected)", err)
	}
	if _, err := ts.db.WebAdminCredentials.Get(t.Context(), cred2.ID); err != nil {
		t.Fatalf("cred2 Get err = %v, want nil (unaffected)", err)
	}
}

func TestRevokeCredential_UnknownIDReturnsErrNotFound(t *testing.T) {
	ts := newTestService(t)
	if _, err := ts.RevokeCredential(t.Context(), "no-such-id"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDecodeCredentials_SkipsCorruptRowsRatherThanFailingEntirely(t *testing.T) {
	valid := domain.WebAdminCredential{CredentialJSON: `{"id":"AQID"}`}
	corrupt := domain.WebAdminCredential{CredentialJSON: `not json`}
	got := decodeCredentials([]domain.WebAdminCredential{valid, corrupt})
	if len(got) != 1 {
		t.Fatalf("decodeCredentials = %+v, want exactly one decoded credential", got)
	}
}

// webauthnRegistrationVector returns the W3C WebAuthn specification's
// published "none" attestation, ES256 registration test vector (the
// same fixture go-webauthn's own TestFinishRegistration_Success uses),
// reproduced here so FinishRegistration's real verification path
// (attestation object decode, rpIdHash check, client data/challenge
// check) can be exercised end to end without a real browser or
// authenticator. It is bound to RPID "example.org" / origin
// "https://example.org" (testRPID/testRPOrigin above) — those are not
// this deployment's real values, just what the vector was generated
// against.
func webauthnRegistrationVector(t *testing.T) (body []byte, challenge string, credentialID []byte) {
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
