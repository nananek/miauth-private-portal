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

	all, err := db.ExternalSources.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("len(all) = %d, want 2", len(all))
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

func TestExternalSourceRepository_EnsureFromConfig_CreatesMissingAndSkipsExisting(t *testing.T) {
	db := newTestDB(t)
	existing := mustCreateExternalSource(t, db, "rss", "https://example.com/existing.xml")

	sources := []domain.ExternalSource{
		{ID: domain.NewID(), Kind: "rss", URI: "https://example.com/existing.xml", CreatedAt: time.Now()},
		{ID: domain.NewID(), Kind: "rss", URI: "https://example.com/new.xml", CreatedAt: time.Now()},
	}
	if err := db.ExternalSources.EnsureFromConfig(t.Context(), sources); err != nil {
		t.Fatalf("ensure from config: %v", err)
	}

	all, err := db.ExternalSources.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	got, err := db.ExternalSources.Get(t.Context(), existing.ID)
	if err != nil {
		t.Fatalf("existing source must survive EnsureFromConfig unchanged: %v", err)
	}
	if got.ID != existing.ID {
		t.Errorf("existing source ID changed: got %q, want %q", got.ID, existing.ID)
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
