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
	"github.com/nananek/miauth-private-portal/internal/openwebui"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// openWebUITestModelExternalID is every OpenWebUI-enabled test server
// helper's OPENWEBUI_DEFAULT_MODEL_ID.
const openWebUITestModelExternalID = "gpt-oss:20b"

// openWebUITestModelSlug is the actor_slug openwebui.GenerateActorSlug
// deterministically derives for that seeded default model: since Issue
// #75 removed OPENWEBUI_MODEL_SLUG, Registry.Seed generates it from the
// model's own external id (no display name is known before any catalog
// sync runs, and these tests never run one), the same computation tests
// that used to assert against the literal "model" now have to make
// themselves rather than assume.
func openWebUITestModelSlug() string {
	return openwebui.GenerateActorSlug(openWebUITestModelExternalID, "", func(candidate string) bool {
		return candidate == defaultMiAuthTestConfig().OwnerUsername || candidate == "assistant" || candidate == "system"
	})
}

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
// are present; see NewServer), and a valid owner API token already
// issued through the local MiAuth flow.
type noteAPITestServer struct {
	*Server
	db       *sqlite.DB
	timeline *timeline.Service
	clock    *fakeTimelineClock
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

// openNoteAPITestDBAt opens (creating and migrating if necessary) a
// contract-test SQLite database at path, with Issue #52's reserved
// actors already seeded. It is split out from the *noteAPITestServer
// builders below so a restart-style test can point two independent
// harness instances at the same on-disk file path — see
// openwebui_restart_test.go.
func openNoteAPITestDBAt(t *testing.T, path string) *sqlite.DB {
	t.Helper()
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
	return db
}

func newNoteAPITestServerWithOptions(t *testing.T, llmEnabled, llmClassificationEnabled bool) *noteAPITestServer {
	t.Helper()
	db := openNoteAPITestDBAt(t, filepath.Join(t.TempDir(), "test.db"))

	miauthCfg := defaultMiAuthTestConfig()
	clock := &fakeTimelineClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// OwnerUsername mirrors miauthCfg's so Issue #23 PR5's self-mention
	// detection (an "@" + this username pattern) matches the same
	// username DescribeOwner/resolveUserLite project onto note.user in
	// these contract tests.
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{Clock: clock, OwnerUsername: miauthCfg.OwnerUsername})

	miauthSvc := miauth.NewService(db, db.Repos, miauthCfg)

	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	srv := NewServer(logger, reg, Options{
		MiAuthService:            miauthSvc,
		TimelineService:          timelineSvc,
		LocalOrigin:              testLocalOrigin,
		LLMEnabled:               llmEnabled,
		LLMClassificationEnabled: llmClassificationEnabled,
		// Wired unconditionally, matching cmd/server (Issue #77 PR4/
		// ADR-0008's design-A host display has no feature flag): only
		// entries authored by an ActorExternalSource actor are affected,
		// which no pre-PR4 test creates.
		ExternalSources: db.Repos.ExternalSources,
	})

	ts := &noteAPITestServer{Server: srv, db: db, timeline: timelineSvc, clock: clock}
	ts.token, ts.ownerID = mustIssueToken(t, ts.Server, "note-api-setup", "read:account,write:notes")
	return ts
}

// newNoteAPITestServerOpenWebUIEnabled builds a noteAPITestServer with a
// real, generation-enabled *openwebui.Registry seeded and its *openwebui.
// Bridge wired through Options.OpenWebUIBridge — the same shape
// cmd/server assembles when OPENWEBUI_ENABLED and its generation gate
// are both on — for tests that verify the wiring itself, not
// internal/openwebui's own enqueue-decision logic (covered by that
// package's own tests).
func newNoteAPITestServerOpenWebUIEnabled(t *testing.T) *noteAPITestServer {
	t.Helper()
	return newNoteAPITestServerOpenWebUIEnabledAt(t, filepath.Join(t.TempDir(), "test.db"), "note-api-setup-openwebui")
}

// newNoteAPITestServerOpenWebUIEnabledAt is
// newNoteAPITestServerOpenWebUIEnabled with the database path and the
// one-time-use MiAuth route session ID pulled out as parameters, so a
// restart-style test can build two independent harness instances (two
// separate *Server, *openwebui.Registry, and *openwebui.Bridge values)
// against the very same on-disk database file — the same shape an actual
// process restart takes — without their token-issuing MiAuth flows
// colliding on session ID. See openwebui_restart_test.go.
func newNoteAPITestServerOpenWebUIEnabledAt(t *testing.T, path, tokenSessionID string) *noteAPITestServer {
	t.Helper()
	db := openNoteAPITestDBAt(t, path)

	miauthCfg := defaultMiAuthTestConfig()
	clock := &fakeTimelineClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{Clock: clock, OwnerUsername: miauthCfg.OwnerUsername})
	miauthSvc := miauth.NewService(db, db.Repos, miauthCfg)

	registry := openwebui.NewRegistry(db, db.Repos, openwebui.RegistryConfig{
		Enabled:           true,
		BaseURL:           "https://openwebui.example.net",
		SecretRef:         openwebui.SecretRefAPIKey,
		WorkspaceName:     "Open WebUI",
		PresentationHost:  "openwebui.example.net",
		DefaultModelID:    openWebUITestModelExternalID,
		OwnerUsername:     miauthCfg.OwnerUsername,
		GenerationEnabled: true,
	}, nil, nil, nil)
	if err := registry.Seed(t.Context()); err != nil {
		t.Fatalf("seed openwebui registry: %v", err)
	}
	bridge := openwebui.NewBridge(openwebui.BridgeConfig{MaxContextMessages: 100}, nil, nil)

	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	srv := NewServer(logger, reg, Options{
		MiAuthService:      miauthSvc,
		TimelineService:    timelineSvc,
		LocalOrigin:        testLocalOrigin,
		VirtualActors:      registry,
		OpenWebUIBridge:    bridge.EnqueueTurn,
		OpenWebUITurnLinks: db.Repos.OpenWebUITurnLinks,
	})

	ts := &noteAPITestServer{Server: srv, db: db, timeline: timelineSvc, clock: clock}
	ts.token, ts.ownerID = mustIssueToken(t, ts.Server, tokenSessionID, "read:account,write:notes")
	return ts
}

// mustIssueToken drives a full MiAuth local-session -> CLI-equivalent
// approval -> check flow against srv for the given permission string,
// and returns the issued raw API token and the owner's local actor ID.
// routeSessionID must be unique per call on the same srv: a route session
// is one-time-use.
func mustIssueToken(t *testing.T, srv *Server, routeSessionID, permission string) (token, ownerActorID string) {
	t.Helper()

	startReq := httptest.NewRequest(http.MethodGet, "/miauth/"+routeSessionID+"?permission="+url.QueryEscape(permission), nil)
	startRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("GET /miauth/%s = %d %q, want %d", routeSessionID, startRec.Code, startRec.Body.String(), http.StatusOK)
	}
	if err := srv.miauth.ApproveSession(t.Context(), routeSessionID); err != nil {
		t.Fatalf("approve MiAuth session: %v", err)
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
