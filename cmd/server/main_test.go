package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/drive"
	"github.com/nananek/miauth-private-portal/internal/ingest/rss"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func TestJobsConfigFrom(t *testing.T) {
	in := config.JobsConfig{
		WorkerID:            "worker-a",
		PollInterval:        time.Second,
		ClaimBatchSize:      3,
		LeaseDuration:       30 * time.Second,
		LeaseRenewMargin:    10 * time.Second,
		MaxAttempts:         7,
		BackoffBase:         2 * time.Second,
		BackoffMax:          time.Minute,
		MaxConcurrentJobs:   2,
		ShutdownGracePeriod: 15 * time.Second,
	}
	got := jobsConfigFrom(in)
	if got.WorkerID != in.WorkerID || got.PollInterval != in.PollInterval ||
		got.ClaimBatchSize != in.ClaimBatchSize || got.LeaseDuration != in.LeaseDuration ||
		got.LeaseRenewMargin != in.LeaseRenewMargin || got.MaxAttempts != in.MaxAttempts ||
		got.BackoffBase != in.BackoffBase || got.BackoffMax != in.BackoffMax ||
		got.MaxConcurrentJobs != in.MaxConcurrentJobs || got.ShutdownGracePeriod != in.ShutdownGracePeriod {
		t.Fatalf("jobsConfigFrom(%+v) = %+v", in, got)
	}
	if got.BackoffJitter != 0.2 {
		t.Errorf("BackoffJitter = %v, want fixed 0.2", got.BackoffJitter)
	}
}

// newEnsureActorsTestFixture builds a real *sqlite.DB, *drive.Service
// (local-disk backed), and a *safehttp.Client for ensureRSSSourceActors'
// tests below. The client's default (non-test) SSRF policy is used
// deliberately, not AllowIPForTesting: every test feed URL below uses a
// loopback host, which that policy rejects outright without a network
// round trip, so favicon fetch fails fast and deterministically — the
// assertions below don't depend on favicon succeeding, only on it never
// blocking actor creation (fetchAndSetSourceFavicon's documented
// best-effort contract).
func newEnsureActorsTestFixture(t *testing.T) (*sqlite.DB, *drive.Service, *safehttp.Client) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second, MaxOpenConns: 4,
	})
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

	faviconClient := safehttp.NewClient(safehttp.Config{MaxRedirects: 3, AllowInsecureHTTP: false})
	driveSvc := drive.NewService(
		drive.NewLocal(t.TempDir()),
		faviconClient,
		db.Repos,
		drive.Config{MaxFileBytes: 1 << 20, MaxImageWidth: 2000, MaxImageHeight: 2000, CapacityBytes: 1 << 30},
	)
	return db, driveSvc, faviconClient
}

func strPtr(s string) *string { return &s }

func discardLogger() *slog.Logger {
	return logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "error"})
}

// TestEnsureRSSSourceActors_ProvisionsSourceCreatedByReconcileFromConfig
// is the live-reload regression test Issue #134 asks for: a source
// created purely by ReconcileFromConfig (simulating exactly what
// ingest.Scheduler.tick's DesiredURIs path does on a live
// `miauthctl config set RSS_FEED_URLS` change — no bootstrap
// cfg.RSS.FeedURLs entry for this URL at all) must get its actor
// provisioned by ensureRSSSourceActors called with empty bootstrap
// lists, matching the scheduler-tick closure's real arguments.
func TestEnsureRSSSourceActors_ProvisionsSourceCreatedByReconcileFromConfig(t *testing.T) {
	db, driveSvc, faviconClient := newEnsureActorsTestFixture(t)
	logger := discardLogger()
	now := time.Now().UTC()
	const feedURL = "https://localhost/new-feed.xml"

	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), rss.Kind, []string{feedURL}, now); err != nil {
		t.Fatalf("ReconcileFromConfig: %v", err)
	}
	if err := ensureRSSSourceActors(t.Context(), db, nil, nil, now, driveSvc, faviconClient, 1<<20, logger); err != nil {
		t.Fatalf("ensureRSSSourceActors: %v", err)
	}

	got, err := db.ExternalSources.GetByURI(t.Context(), rss.Kind, feedURL)
	if err != nil {
		t.Fatalf("GetByURI: %v", err)
	}
	if got.ActorID == nil {
		t.Fatal("ActorID = nil, want provisioned")
	}
	if got.Host == nil || *got.Host != "localhost" {
		t.Errorf("Host = %v, want %q", got.Host, "localhost")
	}
	if got.Username == nil {
		t.Error("Username = nil, want provisioned")
	}

	actor, err := db.Actors.Get(t.Context(), *got.ActorID)
	if err != nil {
		t.Fatalf("Actors.Get: %v", err)
	}
	if actor.Type != domain.ActorExternalSource {
		t.Errorf("actor.Type = %q, want %q", actor.Type, domain.ActorExternalSource)
	}
}

// TestEnsureRSSSourceActors_BackfillsPreExistingActorlessRow is the
// backfill regression test: a bare row created directly (simulating one
// that predates Issue #134's fix, the already-ingested-as-system
// scenario) is provisioned by the same pass, without touching the
// dedupe-scope-relevant fields ReconcileFromConfig itself never touches
// either.
func TestEnsureRSSSourceActors_BackfillsPreExistingActorlessRow(t *testing.T) {
	db, driveSvc, faviconClient := newEnsureActorsTestFixture(t)
	logger := discardLogger()
	now := time.Now().UTC()

	cursor := "old-cursor"
	source := domain.ExternalSource{
		ID: domain.NewID(), Kind: rss.Kind, URI: "https://localhost/legacy.xml", CreatedAt: now,
	}
	if err := db.ExternalSources.Create(t.Context(), source); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.ExternalSources.RecordFetchSuccess(t.Context(), source.ID, &cursor, now); err != nil {
		t.Fatalf("RecordFetchSuccess: %v", err)
	}

	if err := ensureRSSSourceActors(t.Context(), db, nil, nil, now, driveSvc, faviconClient, 1<<20, logger); err != nil {
		t.Fatalf("ensureRSSSourceActors: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActorID == nil || got.Username == nil || got.Host == nil {
		t.Fatalf("identity fields = %+v, want all provisioned", got)
	}
	if got.ID != source.ID {
		t.Errorf("ID changed: got %q, want %q", got.ID, source.ID)
	}
	if got.URI != source.URI {
		t.Errorf("URI changed: got %q, want %q", got.URI, source.URI)
	}
	if !got.CreatedAt.Equal(source.CreatedAt) {
		t.Errorf("CreatedAt changed: got %v, want %v", got.CreatedAt, source.CreatedAt)
	}
	if got.Cursor == nil || *got.Cursor != cursor {
		t.Errorf("Cursor changed: got %v, want unchanged %q (dedupe scope must survive backfill)", got.Cursor, cursor)
	}
}

// TestEnsureRSSSourceActors_StartupPathRegression reproduces the old
// ensureRSSSourcesWithActors startup shape (there was no dedicated test
// for it before this issue's fix — this is new coverage): an explicit
// bootstrap "|username" override is honored, two feeds on the same host
// with no override get distinct auto-derived usernames, and a second
// call (as if from a later restart) makes no further changes.
func TestEnsureRSSSourceActors_StartupPathRegression(t *testing.T) {
	db, driveSvc, faviconClient := newEnsureActorsTestFixture(t)
	logger := discardLogger()
	now := time.Now().UTC()

	feedURLs := []string{
		"https://localhost/explicit.xml",
		"https://localhost/auto-a.xml",
		"https://localhost/auto-b.xml",
	}
	feedUsernames := []*string{strPtr("chosen"), nil, nil}

	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), rss.Kind, feedURLs, now); err != nil {
		t.Fatalf("ReconcileFromConfig: %v", err)
	}
	if err := ensureRSSSourceActors(t.Context(), db, feedURLs, feedUsernames, now, driveSvc, faviconClient, 1<<20, logger); err != nil {
		t.Fatalf("ensureRSSSourceActors (first pass): %v", err)
	}

	sources := make(map[string]domain.ExternalSource, len(feedURLs))
	usernames := make(map[string]bool, len(feedURLs))
	for _, uri := range feedURLs {
		s, err := db.ExternalSources.GetByURI(t.Context(), rss.Kind, uri)
		if err != nil {
			t.Fatalf("GetByURI(%q): %v", uri, err)
		}
		if s.ActorID == nil || s.Username == nil {
			t.Fatalf("source %q not provisioned: %+v", uri, s)
		}
		sources[uri] = s
		if usernames[*s.Username] {
			t.Errorf("username %q reused across sources; want each disambiguated", *s.Username)
		}
		usernames[*s.Username] = true
	}
	if got := *sources["https://localhost/explicit.xml"].Username; got != "chosen" {
		t.Errorf("explicit username = %q, want %q", got, "chosen")
	}

	// Second call, same arguments (as if the process restarted): no
	// further changes — same actor IDs, no new actor rows, no error.
	if err := ensureRSSSourceActors(t.Context(), db, feedURLs, feedUsernames, now, driveSvc, faviconClient, 1<<20, logger); err != nil {
		t.Fatalf("ensureRSSSourceActors (second pass): %v", err)
	}
	for _, uri := range feedURLs {
		s, err := db.ExternalSources.GetByURI(t.Context(), rss.Kind, uri)
		if err != nil {
			t.Fatalf("GetByURI(%q) after second pass: %v", uri, err)
		}
		want := sources[uri]
		if *s.ActorID != *want.ActorID {
			t.Errorf("source %q ActorID changed: got %q, want unchanged %q", uri, *s.ActorID, *want.ActorID)
		}
		if *s.Username != *want.Username {
			t.Errorf("source %q Username changed: got %q, want unchanged %q", uri, *s.Username, *want.Username)
		}
	}
}

// TestEnsureRSSSourceActors_NeverRecomputesExistingActor backs ADR-0008's
// "computed once ... and never recomputed" guarantee end to end: a later
// call with a *different* bootstrap username mapping for the same URI
// must not change an already-provisioned source's identity.
func TestEnsureRSSSourceActors_NeverRecomputesExistingActor(t *testing.T) {
	db, driveSvc, faviconClient := newEnsureActorsTestFixture(t)
	logger := discardLogger()
	now := time.Now().UTC()
	const feedURL = "https://localhost/feed.xml"

	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), rss.Kind, []string{feedURL}, now); err != nil {
		t.Fatalf("ReconcileFromConfig: %v", err)
	}
	if err := ensureRSSSourceActors(t.Context(), db, []string{feedURL}, []*string{strPtr("first")}, now, driveSvc, faviconClient, 1<<20, logger); err != nil {
		t.Fatalf("ensureRSSSourceActors (first pass): %v", err)
	}
	first, err := db.ExternalSources.GetByURI(t.Context(), rss.Kind, feedURL)
	if err != nil {
		t.Fatalf("GetByURI: %v", err)
	}

	// A later tick/restart with a different (or even bootstrap-absent)
	// username mapping for the very same URI must not recompute anything.
	if err := ensureRSSSourceActors(t.Context(), db, []string{feedURL}, []*string{strPtr("second")}, now, driveSvc, faviconClient, 1<<20, logger); err != nil {
		t.Fatalf("ensureRSSSourceActors (second pass): %v", err)
	}

	got, err := db.ExternalSources.GetByURI(t.Context(), rss.Kind, feedURL)
	if err != nil {
		t.Fatalf("GetByURI after second pass: %v", err)
	}
	if *got.ActorID != *first.ActorID {
		t.Errorf("ActorID changed: got %q, want unchanged %q", *got.ActorID, *first.ActorID)
	}
	if *got.Username != *first.Username {
		t.Errorf("Username changed: got %q, want unchanged %q", *got.Username, *first.Username)
	}
	if *got.Username != "first" {
		t.Errorf("Username = %q, want the first pass's %q never overwritten by the second pass's %q", *got.Username, "first", "second")
	}
}

// TestEnsureRSSSourceActors_ConcurrentProvisionIsSafeNoOp exercises the
// "provisioned by a concurrent pass since List above" branch:
// SetActorIdentity racing ahead of ensureRSSSourceActors' own List call
// must be treated as an already-provisioned row, not an error.
func TestEnsureRSSSourceActors_ConcurrentProvisionIsSafeNoOp(t *testing.T) {
	db, driveSvc, faviconClient := newEnsureActorsTestFixture(t)
	logger := discardLogger()
	now := time.Now().UTC()

	source := domain.ExternalSource{ID: domain.NewID(), Kind: rss.Kind, URI: "https://localhost/race.xml", CreatedAt: now}
	if err := db.ExternalSources.Create(t.Context(), source); err != nil {
		t.Fatalf("Create: %v", err)
	}

	actorID := domain.NewID()
	if err := db.Actors.Create(t.Context(), domain.Actor{ID: actorID, Type: domain.ActorExternalSource, CreatedAt: now}); err != nil {
		t.Fatalf("Actors.Create: %v", err)
	}
	if err := db.ExternalSources.SetActorIdentity(t.Context(), source.ID, actorID, "raced", "localhost"); err != nil {
		t.Fatalf("SetActorIdentity: %v", err)
	}

	if err := ensureRSSSourceActors(t.Context(), db, nil, nil, now, driveSvc, faviconClient, 1<<20, logger); err != nil {
		t.Fatalf("ensureRSSSourceActors: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *got.ActorID != actorID || *got.Username != "raced" {
		t.Errorf("source = %+v, want the concurrently-set identity left untouched", got)
	}
}
