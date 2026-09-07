package configstore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/configstore"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func newTestDB(t *testing.T) *sqlite.DB {
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
	return db
}

func TestStore_String_FallsBackWhenUnset(t *testing.T) {
	db := newTestDB(t)
	store := configstore.New(db.Config, nil)

	if got := store.String(t.Context(), "LLM_MODEL", "fallback-model"); got != "fallback-model" {
		t.Errorf("String(unset) = %q, want fallback", got)
	}
}

func TestStore_String_ReadsDBOverride(t *testing.T) {
	db := newTestDB(t)
	store := configstore.New(db.Config, nil)

	if err := db.Config.Set(t.Context(), "LLM_MODEL", "gpt-4o-mini", 0, "system", time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.String(t.Context(), "LLM_MODEL", "fallback-model"); got != "gpt-4o-mini" {
		t.Errorf("String(db override) = %q, want the DB value", got)
	}
}

func TestStore_Duration_ParsesOrFallsBack(t *testing.T) {
	db := newTestDB(t)
	store := configstore.New(db.Config, nil)

	if err := db.Config.Set(t.Context(), "JOBS_POLL_INTERVAL", "5s", 0, "system", time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Duration(t.Context(), "JOBS_POLL_INTERVAL", time.Second); got != 5*time.Second {
		t.Errorf("Duration = %v, want 5s", got)
	}

	// Force a malformed stored value directly, bypassing
	// ValidateKeyValue, to exercise the parse-failure fallback path (a
	// value that was valid when written but no longer parses after a
	// hypothetical internal/config change).
	if err := db.Config.Set(t.Context(), "JOBS_BACKOFF_BASE", "not-a-duration", 0, "system", time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Duration(t.Context(), "JOBS_BACKOFF_BASE", 2*time.Second); got != 2*time.Second {
		t.Errorf("Duration(unparseable) = %v, want fallback 2s", got)
	}
}

func TestStore_Int_And_Int64_And_Bool(t *testing.T) {
	db := newTestDB(t)
	store := configstore.New(db.Config, nil)

	if err := db.Config.Set(t.Context(), "JOBS_MAX_ATTEMPTS", "9", 0, "system", time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Int(t.Context(), "JOBS_MAX_ATTEMPTS", 8); got != 9 {
		t.Errorf("Int = %d, want 9", got)
	}

	if err := db.Config.Set(t.Context(), "IMAP_MAX_MESSAGE_BYTES", "2097152", 0, "system", time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Int64(t.Context(), "IMAP_MAX_MESSAGE_BYTES", 1024); got != 2097152 {
		t.Errorf("Int64 = %d, want 2097152", got)
	}

	if err := db.Config.Set(t.Context(), "IMAP_STORE_FULL_BODY", "true", 0, "system", time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Bool(t.Context(), "IMAP_STORE_FULL_BODY", false); got != true {
		t.Errorf("Bool = %v, want true", got)
	}
}

func TestStore_StringList_SplitsAndHandlesEmpty(t *testing.T) {
	db := newTestDB(t)
	store := configstore.New(db.Config, nil)

	fallback := []string{"https://fallback.example/feed.xml"}
	if got := store.StringList(t.Context(), "RSS_FEED_URLS", fallback); len(got) != 1 || got[0] != fallback[0] {
		t.Errorf("StringList(unset) = %v, want fallback", got)
	}

	if err := db.Config.Set(t.Context(), "RSS_FEED_URLS", "https://a.example/feed.xml,https://b.example/feed.xml", 0, "system", time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := store.StringList(t.Context(), "RSS_FEED_URLS", fallback)
	if len(got) != 2 || got[0] != "https://a.example/feed.xml" || got[1] != "https://b.example/feed.xml" {
		t.Errorf("StringList(db override) = %v", got)
	}

	if err := db.Config.Set(t.Context(), "RSS_FEED_URLS", "", 1, "system", time.Now()); err != nil {
		t.Fatalf("Set(empty): %v", err)
	}
	if got := store.StringList(t.Context(), "RSS_FEED_URLS", fallback); len(got) != 0 {
		t.Errorf("StringList(explicit empty override) = %v, want an empty (not fallback) list", got)
	}
}
