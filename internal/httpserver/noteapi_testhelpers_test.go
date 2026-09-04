package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// fakeTimelineClock is a settable timeline.Clock, so note-API contract
// tests can control entry ordering/timestamps without depending on
// wall-clock timing (mirrors internal/timeline's own test fake).
type fakeTimelineClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeTimelineClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeTimelineClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// noteAPITestServer bundles a real Server with both MiAuthService and
// TimelineService wired (Issue #7's note routes only register when both
// are present; see NewServer), a scriptable upstream provider, and a
// valid owner API token already issued through the full MiAuth flow.
type noteAPITestServer struct {
	*Server
	db       *sqlite.DB
	timeline *timeline.Service
	clock    *fakeTimelineClock
	provider *fakeProvider
	token    string
	ownerID  string
}

func newNoteAPITestServer(t *testing.T) *noteAPITestServer {
	t.Helper()
	return newNoteAPITestServerWithOptions(t, false, false)
}

// newNoteAPITestServerLLMEnabled builds a noteAPITestServer with Issue
// #9's notes/create enqueue hook turned on, for tests that verify an
// "llm_generation" job is (or is not) enqueued.
func newNoteAPITestServerLLMEnabled(t *testing.T) *noteAPITestServer {
	t.Helper()
	return newNoteAPITestServerWithOptions(t, true, false)
}

// newNoteAPITestServerLLMClassificationEnabled builds a noteAPITestServer
// with Issue #10's notes/create enqueue hook turned on (and Issue #9's
// left off), for tests that verify an "llm_classification" job is (or is
// not) enqueued independently of reply generation.
func newNoteAPITestServerLLMClassificationEnabled(t *testing.T) *noteAPITestServer {
	t.Helper()
	return newNoteAPITestServerWithOptions(t, false, true)
}

func newNoteAPITestServerWithOptions(t *testing.T, llmEnabled, llmClassificationEnabled bool) *noteAPITestServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: path, BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}

	clock := &fakeTimelineClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{Clock: clock})

	provider := fixedProvider(testAllowedUserID, true, nil)
	miauthSvc := miauth.NewService(db, db.Repos, provider, defaultMiAuthTestConfig())

	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	srv := NewServer(logger, reg, Options{
		MiAuthService:            miauthSvc,
		TimelineService:          timelineSvc,
		LocalOrigin:              testLocalOrigin,
		IdentityOrigin:           testIdentityOrigin,
		LLMEnabled:               llmEnabled,
		LLMClassificationEnabled: llmClassificationEnabled,
	})

	ts := &noteAPITestServer{Server: srv, db: db, timeline: timelineSvc, clock: clock, provider: provider}
	ts.token, ts.ownerID = mustIssueToken(t, ts.Server, "note-api-setup", "read:account,write:notes")
	return ts
}

// mustIssueToken drives a full MiAuth local-session -> upstream callback
// -> check flow against srv (whose provider must already be scripted to
// approve testAllowedUserID) for the given requested permission string,
// and returns the issued raw API token and the owner's local actor ID.
// routeSessionID must be unique per call on the same srv: a route session
// is one-time-use.
func mustIssueToken(t *testing.T, srv *Server, routeSessionID, permission string) (token, ownerActorID string) {
	t.Helper()

	startReq := httptest.NewRequest(http.MethodGet, "/miauth/"+routeSessionID+"?permission="+url.QueryEscape(permission), nil)
	startRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusFound {
		t.Fatalf("GET /miauth/%s = %d %q, want %d", routeSessionID, startRec.Code, startRec.Body.String(), http.StatusFound)
	}
	loc, err := startRec.Result().Location()
	if err != nil {
		t.Fatalf("parse redirect location: %v", err)
	}
	cbQuery := loc.Query().Get("callback")
	cb, err := url.Parse(cbQuery)
	if err != nil {
		t.Fatalf("parse embedded callback %q: %v", cbQuery, err)
	}
	id := cb.Query().Get("id")
	state := cb.Query().Get("state")
	if id == "" || state == "" {
		t.Fatalf("callback missing id/state: %q", cbQuery)
	}

	callbackReq := httptest.NewRequest(http.MethodGet, "/miauth/callback?id="+id+"&state="+state, nil)
	callbackRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusOK {
		t.Fatalf("GET /miauth/callback = %d %q, want %d", callbackRec.Code, callbackRec.Body.String(), http.StatusOK)
	}

	checkReq := httptest.NewRequest(http.MethodPost, "/api/miauth/"+routeSessionID+"/check", strings.NewReader("{}"))
	checkRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(checkRec, checkReq)
	if checkRec.Code != http.StatusOK {
		t.Fatalf("POST check = %d %q, want %d", checkRec.Code, checkRec.Body.String(), http.StatusOK)
	}
	var resp checkSuccessResponse
	if err := json.Unmarshal(checkRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode check response: %v; body=%s", err, checkRec.Body.String())
	}
	if !resp.OK || resp.Token == "" {
		t.Fatalf("check did not succeed: %+v", resp)
	}
	return resp.Token, resp.User.ID
}

// post sends a JSON POST to path, authenticated with ts.token unless the
// caller already set "i" in body.
func (ts *noteAPITestServer) post(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["i"]; !ok {
		body["i"] = ts.token
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	return rec
}

// postRaw sends body verbatim (for malformed-JSON tests that must bypass
// the map[string]any marshaling above).
func (ts *noteAPITestServer) postRaw(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	return rec
}
