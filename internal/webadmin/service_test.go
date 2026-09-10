package webadmin

import (
	"bytes"
	"encoding/base64"
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
	if _, err := ts.FinishRegistration(t.Context(), raw, req); !errors.Is(err, ErrRegistrationCeremonyFailed) {
		t.Fatalf("err = %v, want ErrRegistrationCeremonyFailed", err)
	}

	stored, err := ts.db.WebAdminBootstrapTokens.GetByTokenHash(t.Context(), hashBootstrapToken(raw))
	if err != nil || stored.Status != domain.WebAdminBootstrapIssued {
		t.Fatalf("token status = %+v, err = %v, want still issued (never consumed)", stored, err)
	}
	if creds, err := ts.db.WebAdminCredentials.ListByOwner(t.Context(), ts.ownerID); err != nil || len(creds) != 0 {
		t.Fatalf("credentials = %+v, err = %v, want none created", creds, err)
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
