package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/jobs"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

func newTestDB(t *testing.T) *sqlite.DB {
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
	return db
}

func mustCreateSource(t *testing.T, db *sqlite.DB, kind, uri string) domain.ExternalSource {
	t.Helper()
	s := domain.ExternalSource{ID: domain.NewID(), Kind: kind, URI: uri, CreatedAt: time.Now().UTC()}
	if err := db.ExternalSources.Create(t.Context(), s); err != nil {
		t.Fatalf("create external source: %v", err)
	}
	return s
}

func newTestJob(payload string) domain.Job {
	now := time.Now().UTC()
	return domain.Job{
		ID: domain.NewID(), JobType: JobType, Payload: payload, PayloadVersion: 1,
		State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now,
	}
}

type fakeAdapter struct {
	kind string
	fn   func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error)
	// calls records every cursor Fetch was called with, for assertions
	// on what Service passed through.
	calls []*string
}

func (f *fakeAdapter) Kind() string { return f.kind }

func (f *fakeAdapter) Fetch(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
	f.calls = append(f.calls, cursor)
	return f.fn(ctx, source, cursor)
}

func TestHandle_Success_CreatesEntriesAndAdvancesCursor(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateSource(t, db, "rss", "https://example.com/feed.xml")
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	adapter := &fakeAdapter{kind: "rss", fn: func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
		return FetchResult{
			Items: []FetchedItem{
				{ExternalID: "guid-1", DedupeKey: "dedupe-1", Title: "One", Body: "body one"},
				{ExternalID: "guid-2", DedupeKey: "dedupe-2", Title: "Two", Body: "body two"},
			},
			NextCursor: `{"etag":"v1"}`,
		}, nil
	}}

	svc := NewService(db.Repos, timelineSvc, nil)
	svc.RegisterAdapter(adapter)

	payload, err := NewJobPayload(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(t.Context(), newTestJob(payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor == nil || *got.Cursor != `{"etag":"v1"}` {
		t.Errorf("Cursor = %v, want the adapter's NextCursor", got.Cursor)
	}
	if got.LastError != nil {
		t.Errorf("LastError = %v, want nil after success", got.LastError)
	}

	for _, dedupeKey := range []string{"dedupe-1", "dedupe-2"} {
		item, err := db.ExternalItems.GetByDedupeKey(t.Context(), dedupeKey)
		if err != nil {
			t.Fatalf("item %s not created: %v", dedupeKey, err)
		}
		if item.EntryID == nil {
			t.Errorf("item %s was not promoted to an entry", dedupeKey)
		}
	}
}

func TestHandle_NotModified_RecordsSuccessWithoutCreatingItems(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateSource(t, db, "rss", "https://example.com/feed.xml")
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	adapter := &fakeAdapter{kind: "rss", fn: func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
		return FetchResult{NotModified: true}, nil
	}}

	svc := NewService(db.Repos, timelineSvc, nil)
	svc.RegisterAdapter(adapter)

	payload, err := NewJobPayload(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(t.Context(), newTestJob(payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, err := db.ExternalSources.Get(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastFetchedAt == nil {
		t.Error("LastFetchedAt = nil, want set after a NotModified fetch")
	}
	if got.Cursor != nil {
		t.Errorf("Cursor = %v, want nil (NotModified carries no new cursor)", got.Cursor)
	}
}

func TestHandle_MalformedPayloadIsPermanent(t *testing.T) {
	db := newTestDB(t)
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})
	svc := NewService(db.Repos, timelineSvc, nil)

	err := svc.Handle(t.Context(), newTestJob("not valid json"))
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Errorf("err = %v, want jobs.PermanentError", err)
	}
}

func TestHandle_MissingSourceIDIsPermanent(t *testing.T) {
	db := newTestDB(t)
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})
	svc := NewService(db.Repos, timelineSvc, nil)

	err := svc.Handle(t.Context(), newTestJob(`{"sourceId":""}`))
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Errorf("err = %v, want jobs.PermanentError", err)
	}
}

func TestHandle_SourceNotFoundIsPermanent(t *testing.T) {
	db := newTestDB(t)
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})
	svc := NewService(db.Repos, timelineSvc, nil)

	payload, err := NewJobPayload("does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	err = svc.Handle(t.Context(), newTestJob(payload))
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Errorf("err = %v, want jobs.PermanentError", err)
	}
}

func TestHandle_UnregisteredAdapterKindIsPermanent(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateSource(t, db, "imap", "imap://example.com/inbox")
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})
	svc := NewService(db.Repos, timelineSvc, nil) // no adapter registered at all

	payload, err := NewJobPayload(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.Handle(t.Context(), newTestJob(payload))
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Errorf("err = %v, want jobs.PermanentError", err)
	}
}

func TestHandle_TransientFetchErrorIsRetryableAndRecordsFailure(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateSource(t, db, "rss", "https://example.com/feed.xml")
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	adapter := &fakeAdapter{kind: "rss", fn: func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
		return FetchResult{}, NewFetchError(CategoryTimeout, context.DeadlineExceeded)
	}}
	svc := NewService(db.Repos, timelineSvc, nil)
	svc.RegisterAdapter(adapter)

	payload, err := NewJobPayload(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.Handle(t.Context(), newTestJob(payload))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var permanent *jobs.PermanentError
	if errors.As(err, &permanent) {
		t.Errorf("err = %v, want a plain retryable error, not jobs.PermanentError", err)
	}

	got, getErr := db.ExternalSources.Get(t.Context(), source.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", got.ConsecutiveFailures)
	}
	if got.LastError == nil || *got.LastError != string(CategoryTimeout) {
		t.Errorf("LastError = %v, want %q", got.LastError, CategoryTimeout)
	}
}

func TestHandle_PermanentFetchErrorFailsJobPermanently(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateSource(t, db, "rss", "https://example.com/feed.xml")
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	adapter := &fakeAdapter{kind: "rss", fn: func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
		return FetchResult{}, NewFetchError(CategoryMalformed, errors.New("bad xml"))
	}}
	svc := NewService(db.Repos, timelineSvc, nil)
	svc.RegisterAdapter(adapter)

	payload, err := NewJobPayload(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.Handle(t.Context(), newTestJob(payload))
	var permanent *jobs.PermanentError
	if !errors.As(err, &permanent) {
		t.Errorf("err = %v, want jobs.PermanentError for a malformed (permanent) category", err)
	}
}

// TestHandle_DuplicateDeliveryDoesNotDuplicateEntries simulates a job
// retried after its first attempt already committed every item's
// CreateExternalEntry but crashed before RecordFetchSuccess persisted
// (or, equivalently, the same items reappearing in a later fetch because
// the source never advances past them): re-processing the same
// FetchedItems must not create a second entry for any of them.
func TestHandle_DuplicateDeliveryDoesNotDuplicateEntries(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateSource(t, db, "rss", "https://example.com/feed.xml")
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	items := []FetchedItem{{ExternalID: "guid-1", DedupeKey: "dedupe-1", Title: "One", Body: "body one"}}
	adapter := &fakeAdapter{kind: "rss", fn: func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
		return FetchResult{Items: items, NextCursor: `{"etag":"v1"}`}, nil
	}}
	svc := NewService(db.Repos, timelineSvc, nil)
	svc.RegisterAdapter(adapter)

	payload, err := NewJobPayload(source.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Handle(t.Context(), newTestJob(payload)); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	firstItem, err := db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}

	// A second delivery of a job carrying the exact same payload (the
	// adapter would return the same items again because the source's
	// cursor has not yet been consulted to skip them, or because
	// RecordFetchSuccess never persisted after the first attempt).
	if err := svc.Handle(t.Context(), newTestJob(payload)); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	secondItem, err := db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}
	if secondItem.EntryID == nil || firstItem.EntryID == nil || *secondItem.EntryID != *firstItem.EntryID {
		t.Errorf("second delivery promoted a different entry: first=%v second=%v", firstItem.EntryID, secondItem.EntryID)
	}

	timelineEntries, err := timelineSvc.GetTimeline(t.Context(), domain.Page{Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(timelineEntries) != 1 {
		t.Errorf("timeline has %d entries after duplicate delivery, want 1", len(timelineEntries))
	}
}
