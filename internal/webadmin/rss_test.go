package webadmin

import (
	"context"
	"errors"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
)

func strp(s string) *string { return &s }

func TestListRSSFeeds_NoOverride_ReturnsBootstrapValue(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
		c.RSSFeedUsernamesBootstrap = []*string{strp("alice")}
	})

	snap, err := ts.ListRSSFeeds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snap.FromOverride {
		t.Fatal("FromOverride = true, want false (no app_config row exists)")
	}
	if len(snap.Feeds) != 1 || snap.Feeds[0].URL != "https://bootstrap.example/feed" || snap.Feeds[0].Username == nil || *snap.Feeds[0].Username != "alice" {
		t.Fatalf("Feeds = %+v, want one bootstrap feed with username alice", snap.Feeds)
	}
}

func TestListRSSFeeds_WithOverride_ReturnsOverrideValue_NotBootstrap(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
	})
	if err := ts.db.Config.Set(t.Context(), config.KeyRSSFeedURLs, "https://override.example/a,https://override.example/b|bob", 0, ts.ownerID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}

	snap, err := ts.ListRSSFeeds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.FromOverride {
		t.Fatal("FromOverride = false, want true (an app_config row exists)")
	}
	if len(snap.Feeds) != 2 {
		t.Fatalf("Feeds = %+v, want 2 entries", snap.Feeds)
	}
	if snap.Feeds[0].URL != "https://override.example/a" || snap.Feeds[0].Username != nil {
		t.Errorf("Feeds[0] = %+v, want https://override.example/a with no username", snap.Feeds[0])
	}
	if snap.Feeds[1].URL != "https://override.example/b" || snap.Feeds[1].Username == nil || *snap.Feeds[1].Username != "bob" {
		t.Errorf("Feeds[1] = %+v, want https://override.example/b|bob", snap.Feeds[1])
	}
}

func TestAddRSSFeed_NoExistingOverride_CreatesOverrideSeededFromBootstrap(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
		c.RSSFeedUsernamesBootstrap = []*string{strp("alice")}
	})

	if err := ts.AddRSSFeed(t.Context(), ts.ownerID, "https://new.example/feed", nil); err != nil {
		t.Fatal(err)
	}

	entry, err := ts.db.Config.Get(t.Context(), config.KeyRSSFeedURLs)
	if err != nil {
		t.Fatal(err)
	}
	// This is the regression test for reconstructing the bootstrap value
	// via cfg.RSS.FeedURLs/FeedUsernames directly rather than
	// cfg.Redacted(): the bootstrap entry's own "|alice" suffix must
	// survive into the new override.
	want := "https://bootstrap.example/feed|alice,https://new.example/feed"
	if entry.Value != want {
		t.Fatalf("app_config value = %q, want %q (bootstrap username suffix must be preserved)", entry.Value, want)
	}
}

func TestAddRSSFeed_ExistingOverride_AppendsAndBumpsVersion(t *testing.T) {
	ts := newTestService(t)
	if err := ts.db.Config.Set(t.Context(), config.KeyRSSFeedURLs, "https://existing.example/a", 0, ts.ownerID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}

	if err := ts.AddRSSFeed(t.Context(), ts.ownerID, "https://existing.example/b", nil); err != nil {
		t.Fatal(err)
	}

	entry, err := ts.db.Config.Get(t.Context(), config.KeyRSSFeedURLs)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://existing.example/a,https://existing.example/b"
	if entry.Value != want {
		t.Fatalf("app_config value = %q, want %q", entry.Value, want)
	}
	if entry.Version != 2 {
		t.Fatalf("version = %d, want 2", entry.Version)
	}
}

func TestAddRSSFeed_DuplicateURL_ReturnsErrRSSFeedAlreadyExists(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
		c.RSSFeedUsernamesBootstrap = []*string{nil}
	})

	err := ts.AddRSSFeed(t.Context(), ts.ownerID, "https://bootstrap.example/feed", nil)
	if !errors.Is(err, ErrRSSFeedAlreadyExists) {
		t.Fatalf("err = %v, want ErrRSSFeedAlreadyExists", err)
	}
}

func TestAddRSSFeed_InvalidURL_ReturnsErrRSSFeedInvalid_WrapsValidationError(t *testing.T) {
	ts := newTestService(t)

	err := ts.AddRSSFeed(t.Context(), ts.ownerID, "not-a-url", nil)
	if !errors.Is(err, ErrRSSFeedInvalid) {
		t.Fatalf("err = %v, want ErrRSSFeedInvalid", err)
	}
	if err.Error() == ErrRSSFeedInvalid.Error() {
		t.Fatalf("err.Error() = %q, want it to contain the wrapped validation reason, not just the generic sentinel text", err.Error())
	}
}

func TestAddRSSFeed_WritesExactlyOneAppConfigAuditEntry(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
		c.RSSFeedUsernamesBootstrap = []*string{nil}
	})

	if err := ts.AddRSSFeed(t.Context(), ts.ownerID, "https://new.example/feed", nil); err != nil {
		t.Fatal(err)
	}

	entries, err := ts.db.ConfigAudit.ListByKey(t.Context(), config.KeyRSSFeedURLs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1: %+v", len(entries), entries)
	}
	if entries[0].NewValue == nil || *entries[0].NewValue != "https://bootstrap.example/feed,https://new.example/feed" {
		t.Fatalf("audit entry = %+v, unexpected NewValue", entries[0])
	}
}

func TestRemoveRSSFeed_FromOverride_LeavesRemainderInOverride(t *testing.T) {
	ts := newTestService(t)
	if err := ts.db.Config.Set(t.Context(), config.KeyRSSFeedURLs, "https://existing.example/a,https://existing.example/b", 0, ts.ownerID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}

	removedUsername, err := ts.RemoveRSSFeed(t.Context(), ts.ownerID, "https://existing.example/a")
	if err != nil {
		t.Fatal(err)
	}
	if removedUsername != nil {
		t.Fatalf("removedUsername = %v, want nil", removedUsername)
	}

	entry, err := ts.db.Config.Get(t.Context(), config.KeyRSSFeedURLs)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Value != "https://existing.example/b" {
		t.Fatalf("app_config value = %q, want just the remaining feed", entry.Value)
	}
}

func TestRemoveRSSFeed_LastEntryOfOverride_UnsetsOverride_RevertsToBootstrap(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
		c.RSSFeedUsernamesBootstrap = []*string{nil}
	})
	if err := ts.db.Config.Set(t.Context(), config.KeyRSSFeedURLs, "https://override.example/only", 0, ts.ownerID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}

	if _, err := ts.RemoveRSSFeed(t.Context(), ts.ownerID, "https://override.example/only"); err != nil {
		t.Fatal(err)
	}

	if _, err := ts.db.Config.Get(t.Context(), config.KeyRSSFeedURLs); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after unset = %v, want domain.ErrNotFound", err)
	}
	snap, err := ts.ListRSSFeeds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snap.FromOverride {
		t.Fatal("FromOverride = true after unset, want false")
	}
	if len(snap.Feeds) != 1 || snap.Feeds[0].URL != "https://bootstrap.example/feed" {
		t.Fatalf("Feeds = %+v, want the bootstrap feed again", snap.Feeds)
	}
}

func TestRemoveRSSFeed_LastEntryOfBootstrapValue_ReturnsErrRSSFeedCannotRemoveLastBootstrapFeed(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/only"}
		c.RSSFeedUsernamesBootstrap = []*string{nil}
	})

	_, err := ts.RemoveRSSFeed(t.Context(), ts.ownerID, "https://bootstrap.example/only")
	if !errors.Is(err, ErrRSSFeedCannotRemoveLastBootstrapFeed) {
		t.Fatalf("err = %v, want ErrRSSFeedCannotRemoveLastBootstrapFeed", err)
	}

	snap, err := ts.ListRSSFeeds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Feeds) != 1 || snap.Feeds[0].URL != "https://bootstrap.example/only" {
		t.Fatalf("Feeds = %+v, want the bootstrap feed still present (nothing silently changed)", snap.Feeds)
	}
}

func TestRemoveRSSFeed_UnknownURL_ReturnsErrRSSFeedNotFound(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
		c.RSSFeedUsernamesBootstrap = []*string{nil}
	})

	_, err := ts.RemoveRSSFeed(t.Context(), ts.ownerID, "https://does-not-exist.example/feed")
	if !errors.Is(err, ErrRSSFeedNotFound) {
		t.Fatalf("err = %v, want ErrRSSFeedNotFound", err)
	}
}

// conflictingConfigRepo wraps a real domain.ConfigRepository and, on
// every Get call for RSS_FEED_URLS, performs an additional
// version-bumping Set on that same key (writing the row's own current
// value back, via a second handle to the same DB — the "second browser
// tab" stand-in) as a side effect before returning the pre-bump entry to
// the real caller. Both AddRSSFeed and RemoveRSSFeed/setRSSFeedURLs read
// RSS_FEED_URLS's current version via repos.Config.Get at least twice
// before their own final Set (once in currentRSSFeedURLs, again in
// setRSSFeedURLs's own pre-write read) — bumping the version on every
// such Get call guarantees the row's actual version has moved one past
// whatever setRSSFeedURLs just read by the time its own Set runs,
// deterministically reproducing the concurrent-write race a second
// browser tab or `miauthctl config set` would otherwise only hit
// nondeterministically. Mirrors Phase 2's listThenDeleteCredentialRepo
// technique (service_test.go).
type conflictingConfigRepo struct {
	domain.ConfigRepository
	t      *testing.T
	rawSet func(ctx context.Context, key, value string, expectedVersion int) error
}

func (r conflictingConfigRepo) Get(ctx context.Context, key string) (domain.AppConfigEntry, error) {
	r.t.Helper()
	entry, err := r.ConfigRepository.Get(ctx, key)
	if err != nil {
		return entry, err
	}
	if key == config.KeyRSSFeedURLs {
		if err := r.rawSet(ctx, key, entry.Value, entry.Version); err != nil {
			r.t.Fatal(err)
		}
	}
	return entry, nil
}

func TestAddRSSFeed_ConcurrentModification_ReturnsDomainErrConflict(t *testing.T) {
	ts := newTestService(t, func(c *Config) {
		c.RSSFeedURLsBootstrap = []string{"https://bootstrap.example/feed"}
		c.RSSFeedUsernamesBootstrap = []*string{nil}
	})
	if err := ts.db.Config.Set(t.Context(), config.KeyRSSFeedURLs, "https://existing.example/feed", 0, ts.ownerID, ts.clock.Now()); err != nil {
		t.Fatal(err)
	}

	racyRepos := ts.db.Repos
	racyRepos.Config = conflictingConfigRepo{
		ConfigRepository: racyRepos.Config,
		t:                t,
		rawSet: func(ctx context.Context, key, value string, expectedVersion int) error {
			return ts.db.Config.Set(ctx, key, value, expectedVersion, ts.ownerID, ts.clock.Now())
		},
	}
	racySvc, err := NewService(ts.db, racyRepos, Config{
		RPID: testRPID, RPDisplayName: "Test Portal", RPOrigins: []string{testRPOrigin},
		OwnerUsername: "owner", OwnerDisplayName: "Test Owner", Clock: ts.clock,
		SessionCookie:             SessionCookieConfig{SessionTTL: testSessionTTL},
		RSSFeedURLsBootstrap:      []string{"https://bootstrap.example/feed"},
		RSSFeedUsernamesBootstrap: []*string{nil},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = racySvc.AddRSSFeed(t.Context(), ts.ownerID, "https://new.example/feed", nil)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v, want domain.ErrConflict", err)
	}
}
