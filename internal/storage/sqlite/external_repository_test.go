package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func mustCreateExternalSource(t *testing.T, db *DB, kind, uri string) domain.ExternalSource {
	t.Helper()
	s := domain.ExternalSource{ID: domain.NewID(), Kind: kind, URI: uri, CreatedAt: time.Now()}
	if err := db.ExternalSources.Create(t.Context(), s); err != nil {
		t.Fatalf("create external source: %v", err)
	}
	return s
}

func TestExternalSourceRepository_CreateGetList(t *testing.T) {
	db := newTestDB(t)
	a := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")
	_ = mustCreateExternalSource(t, db, "imap", "imap://example.com/inbox")

	got, err := db.ExternalSources.Get(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "rss" {
		t.Errorf("Kind = %q, want rss", got.Kind)
	}

	all, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("len(all) = %d, want 1", len(all))
	}
}

// TestExternalSourceRepository_CreateGet_RoundTripsIdentityFields backs
// Issue #77 PR4/ADR-0007's design-A columns: ActorID/Username/Host round
// -trip exactly, and stay nil for an imap-kind source that never sets
// them (mustCreateExternalSource's plain shape, unchanged since before
// PR4).
func TestExternalSourceRepository_CreateGet_RoundTripsIdentityFields(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateDistinctActor(t, db)
	username := "myfeed"
	host := "example.com"
	s := domain.ExternalSource{
		ID: domain.NewID(), Kind: "rss", URI: "https://example.com/rss",
		ActorID: &actorID, Username: &username, Host: &host, CreatedAt: time.Now(),
	}
	if err := db.ExternalSources.Create(t.Context(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActorID == nil || *got.ActorID != actorID {
		t.Errorf("ActorID = %v, want %q", got.ActorID, actorID)
	}
	if got.Username == nil || *got.Username != username {
		t.Errorf("Username = %v, want %q", got.Username, username)
	}
	if got.Host == nil || *got.Host != host {
		t.Errorf("Host = %v, want %q", got.Host, host)
	}

	imapSource := mustCreateExternalSource(t, db, "imap", "imap://example.com/inbox")
	got, err = db.ExternalSources.Get(t.Context(), imapSource.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActorID != nil || got.Username != nil || got.Host != nil {
		t.Errorf("imap source identity fields = %+v, want all nil", got)
	}
}

func TestExternalSourceRepository_GetByURI(t *testing.T) {
	db := newTestDB(t)
	s := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")

	got, err := db.ExternalSources.GetByURI(t.Context(), "rss", "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("GetByURI: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("GetByURI id = %q, want %q", got.ID, s.ID)
	}

	if _, err := db.ExternalSources.GetByURI(t.Context(), "rss", "https://does-not-exist.example/feed.xml"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByURI(unknown) err = %v, want ErrNotFound", err)
	}
	// Same URI, different kind: must not match (kind is part of the key).
	if _, err := db.ExternalSources.GetByURI(t.Context(), "imap", "https://example.com/feed.xml"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByURI(wrong kind) err = %v, want ErrNotFound", err)
	}
}

func TestExternalSourceRepository_GetByActorID(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateDistinctActor(t, db)
	username, host := "myfeed", "example.com"
	s := domain.ExternalSource{
		ID: domain.NewID(), Kind: "rss", URI: "https://example.com/rss",
		ActorID: &actorID, Username: &username, Host: &host, CreatedAt: time.Now(),
	}
	if err := db.ExternalSources.Create(t.Context(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.ExternalSources.GetByActorID(t.Context(), actorID)
	if err != nil {
		t.Fatalf("GetByActorID: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("GetByActorID id = %q, want %q", got.ID, s.ID)
	}

	if _, err := db.ExternalSources.GetByActorID(t.Context(), "no-such-actor"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByActorID(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestExternalSourceRepository_List_FiltersByKind(t *testing.T) {
	db := newTestDB(t)
	mustCreateExternalSource(t, db, "rss", "https://example.com/a.xml")
	mustCreateExternalSource(t, db, "rss", "https://example.com/b.xml")
	mustCreateExternalSource(t, db, "imap", "imap://example.com/inbox")

	rssSources, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(rssSources) != 2 {
		t.Errorf("len(rssSources) = %d, want 2", len(rssSources))
	}

	imapSources, err := db.ExternalSources.List(t.Context(), "imap")
	if err != nil {
		t.Fatal(err)
	}
	if len(imapSources) != 1 {
		t.Errorf("len(imapSources) = %d, want 1", len(imapSources))
	}
}

func TestExternalSourceRepository_Create_RejectsDuplicateKindURI(t *testing.T) {
	db := newTestDB(t)
	mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")

	dup := domain.ExternalSource{ID: domain.NewID(), Kind: "rss", URI: "https://example.com/feed.xml", CreatedAt: time.Now()}
	err := db.ExternalSources.Create(t.Context(), dup)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create() error = %v, want ErrConflict", err)
	}
}

func TestExternalItemRepository_CreateGetByDedupeKeyAndPromote(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	source := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")
	now := time.Now()

	item := domain.ExternalItem{
		ID: domain.NewID(), SourceID: source.ID, ExternalID: "guid-1", FetchedAt: now, DedupeKey: "dedupe-1",
		CreatedAt: now,
	}
	if err := db.ExternalItems.Create(t.Context(), item); err != nil {
		t.Fatal(err)
	}

	got, err := db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EntryID != nil {
		t.Error("EntryID should be nil before promotion")
	}

	entry := mustCreateThreadAndRoot(t, db, actorID, now)
	if err := db.ExternalItems.Promote(t.Context(), item.ID, entry.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	got, err = db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EntryID == nil || *got.EntryID != entry.ID {
		t.Errorf("EntryID = %v, want %q", got.EntryID, entry.ID)
	}
}

func TestExternalItemRepository_Promote_RejectsSecondAttempt(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	source := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")
	now := time.Now()

	item := domain.ExternalItem{
		ID: domain.NewID(), SourceID: source.ID, ExternalID: "guid-1", FetchedAt: now, DedupeKey: "dedupe-1",
		CreatedAt: now,
	}
	if err := db.ExternalItems.Create(t.Context(), item); err != nil {
		t.Fatal(err)
	}

	first := mustCreateThreadAndRoot(t, db, actorID, now)
	if err := db.ExternalItems.Promote(t.Context(), item.ID, first.ID); err != nil {
		t.Fatalf("first promote: %v", err)
	}

	second := mustCreateThreadAndRoot(t, db, actorID, now)
	err := db.ExternalItems.Promote(t.Context(), item.ID, second.ID)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second Promote() error = %v, want ErrConflict", err)
	}
}

func TestExternalSourceRepository_RecordFetchSuccess_SetsCursorAndClearsFailure(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")
	now := time.Now().UTC()

	if err := db.ExternalSources.RecordFetchFailure(t.Context(), source.ID, "timeout", now); err != nil {
		t.Fatalf("record fetch failure: %v", err)
	}

	cursor := `{"etag":"abc"}`
	later := now.Add(time.Minute)
	if err := db.ExternalSources.RecordFetchSuccess(t.Context(), source.ID, &cursor, later); err != nil {
		t.Fatalf("record fetch success: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor == nil || *got.Cursor != cursor {
		t.Errorf("Cursor = %v, want %q", got.Cursor, cursor)
	}
	if got.LastError != nil {
		t.Errorf("LastError = %v, want nil after success", got.LastError)
	}
	if got.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 after success", got.ConsecutiveFailures)
	}
	if got.LastFetchedAt == nil || !got.LastFetchedAt.Equal(later) {
		t.Errorf("LastFetchedAt = %v, want %v", got.LastFetchedAt, later)
	}
}

func TestExternalSourceRepository_RecordFetchSuccess_NilCursorLeavesExistingCursorUnchanged(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")
	now := time.Now().UTC()

	cursor := `{"etag":"abc"}`
	if err := db.ExternalSources.RecordFetchSuccess(t.Context(), source.ID, &cursor, now); err != nil {
		t.Fatalf("first record fetch success: %v", err)
	}

	// A NotModified fetch has no new cursor to record; passing nil must
	// leave the previously stored cursor untouched, not clear it.
	if err := db.ExternalSources.RecordFetchSuccess(t.Context(), source.ID, nil, now.Add(time.Minute)); err != nil {
		t.Fatalf("second record fetch success: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor == nil || *got.Cursor != cursor {
		t.Errorf("Cursor = %v, want unchanged %q", got.Cursor, cursor)
	}
}

func TestExternalSourceRepository_RecordFetchFailure_IncrementsConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")
	now := time.Now().UTC()

	if err := db.ExternalSources.RecordFetchFailure(t.Context(), source.ID, "timeout", now); err != nil {
		t.Fatalf("first failure: %v", err)
	}
	if err := db.ExternalSources.RecordFetchFailure(t.Context(), source.ID, "timeout", now.Add(time.Minute)); err != nil {
		t.Fatalf("second failure: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", got.ConsecutiveFailures)
	}
	if got.LastError == nil || *got.LastError != "timeout" {
		t.Errorf("LastError = %v, want \"timeout\"", got.LastError)
	}
}

func TestExternalSourceRepository_ReconcileFromConfig_CreatesMissingAndSkipsExisting(t *testing.T) {
	db := newTestDB(t)
	existing := mustCreateExternalSource(t, db, "rss", "https://example.com/existing.xml")

	uris := []string{"https://example.com/existing.xml", "https://example.com/new.xml"}
	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), "rss", uris, time.Now()); err != nil {
		t.Fatalf("reconcile from config: %v", err)
	}

	all, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	got, err := db.ExternalSources.Get(t.Context(), existing.ID)
	if err != nil {
		t.Fatalf("existing source must survive ReconcileFromConfig unchanged: %v", err)
	}
	if got.ID != existing.ID {
		t.Errorf("existing source ID changed: got %q, want %q", got.ID, existing.ID)
	}
}

// TestExternalSourceRepository_ReconcileFromConfig_DeactivatesRemovedURI
// backs Issue #76 PR4a's RSS_FEED_URLS reload: a URI dropped from a
// later reconcile round is deactivated (List no longer returns it), but
// its row survives untouched — this service's append-only-history
// convention (ExternalSource.Active's own doc comment).
func TestExternalSourceRepository_ReconcileFromConfig_DeactivatesRemovedURI(t *testing.T) {
	db := newTestDB(t)
	kept := mustCreateExternalSource(t, db, "rss", "https://example.com/kept.xml")
	removed := mustCreateExternalSource(t, db, "rss", "https://example.com/removed.xml")

	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), "rss", []string{kept.URI}, time.Now()); err != nil {
		t.Fatalf("reconcile from config: %v", err)
	}

	active, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != kept.ID {
		t.Fatalf("List after reconcile = %+v, want only %s", active, kept.ID)
	}

	got, err := db.ExternalSources.Get(t.Context(), removed.ID)
	if err != nil {
		t.Fatalf("removed source's row must survive: %v", err)
	}
	if got.Active {
		t.Error("removed source's Active = true, want false")
	}
}

// TestExternalSourceRepository_ReconcileFromConfig_ReactivatesReaddedURI
// covers the round trip: a URI removed (deactivated) in one round and
// present again in a later one must resume being polled, reusing the
// same row rather than creating a duplicate.
func TestExternalSourceRepository_ReconcileFromConfig_ReactivatesReaddedURI(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")

	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), "rss", nil, time.Now()); err != nil {
		t.Fatalf("reconcile (remove): %v", err)
	}
	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), "rss", []string{source.URI}, time.Now()); err != nil {
		t.Fatalf("reconcile (re-add): %v", err)
	}

	active, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != source.ID {
		t.Fatalf("List after re-add = %+v, want the original row %s reactivated, not duplicated", active, source.ID)
	}
}

// TestExternalSourceRepository_List_OmitsInactiveSources backs
// ingest.Scheduler's own reliance on List already filtering to active
// sources only.
func TestExternalSourceRepository_List_OmitsInactiveSources(t *testing.T) {
	db := newTestDB(t)
	_ = mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")

	if err := db.ExternalSources.ReconcileFromConfig(t.Context(), "rss", nil, time.Now()); err != nil {
		t.Fatalf("reconcile (remove): %v", err)
	}

	active, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Errorf("List = %+v, want no active sources", active)
	}
}

func TestExternalItemRepository_Create_RejectsDuplicateDedupeKey(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateExternalSource(t, db, "rss", "https://example.com/feed.xml")
	now := time.Now()

	first := domain.ExternalItem{
		ID: domain.NewID(), SourceID: source.ID, ExternalID: "guid-1", FetchedAt: now, DedupeKey: "same-key",
		CreatedAt: now,
	}
	if err := db.ExternalItems.Create(t.Context(), first); err != nil {
		t.Fatal(err)
	}

	second := domain.ExternalItem{
		ID: domain.NewID(), SourceID: source.ID, ExternalID: "guid-2", FetchedAt: now, DedupeKey: "same-key",
		CreatedAt: now,
	}
	err := db.ExternalItems.Create(t.Context(), second)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Create() error = %v, want ErrConflict", err)
	}
}
