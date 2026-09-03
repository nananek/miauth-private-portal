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
