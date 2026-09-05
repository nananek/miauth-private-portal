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

// TestHandle_ImapItem_NeverEnqueuesLLMJobs pins Issue #12's plan section
// 9 requirement: mail ingested through this framework must never
// automatically reach an LLM prompt. internal/ingest.Service imports
// neither internal/llmreply nor internal/llmclassify at all (only
// internal/httpserver's handleNotesCreate, for EntryUserPost, ever
// enqueues those job types), so this asserts the observable consequence
// instead: ingesting an IMAP-sourced item never enqueues any job as a
// side effect.
func TestHandle_ImapItem_NeverEnqueuesLLMJobs(t *testing.T) {
	db := newTestDB(t)
	source := mustCreateSource(t, db, "imap", "imap://mail.example.com:993/INBOX")
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	adapter := &fakeAdapter{kind: "imap", fn: func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
		return FetchResult{
			Items:      []FetchedItem{{ExternalID: "<msg@example.com>", DedupeKey: "dedupe-mail-1", Body: "From: a@example.com\nSubject: Hi\n\nBody"}},
			NextCursor: `{"uidValidity":1,"lastUid":1}`,
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

	item, err := db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-mail-1")
	if err != nil {
		t.Fatal(err)
	}
	if item.EntryID == nil {
		t.Fatal("mail item was not promoted to an entry")
	}
	entry, err := db.Entries.Get(t.Context(), *item.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Kind != domain.EntryMail {
		t.Errorf("Entry.Kind = %q, want %q", entry.Kind, domain.EntryMail)
	}

	gotJobs, err := db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotJobs) != 0 {
		t.Errorf("Jobs.List() = %+v, want no job enqueued as a side effect of ingesting a mail item", gotJobs)
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

// TestComposeExternalBody pins Issue #13 AC5's provenance-marker design:
// an item with a non-empty Title (RSS/Atom today) gets a "[<kind>[:
// <source display name>]] <title>" header, optionally followed by its
// ProvenanceURL, folded onto its Body; an item with no Title (every
// internal/mailfetch item — see FetchedItem's own doc comment) passes
// through unchanged, since internal/mailfetch already folds its own
// From/Subject/Date header block into Body itself.
func TestComposeExternalBody(t *testing.T) {
	displayName := "Example Blog"
	provenanceURL := "https://example.com/article"

	tests := []struct {
		name   string
		source domain.ExternalSource
		kind   domain.EntryKind
		item   FetchedItem
		want   string
	}{
		{
			name:   "no title passes through unchanged (mail items never set Title)",
			source: domain.ExternalSource{Kind: "imap"},
			kind:   domain.EntryMail,
			item:   FetchedItem{Body: "From: a@example.com\nSubject: Hi\n\nBody"},
			want:   "From: a@example.com\nSubject: Hi\n\nBody",
		},
		{
			name:   "title with source display name and provenance URL",
			source: domain.ExternalSource{Kind: "rss", DisplayName: &displayName},
			kind:   domain.EntryNews,
			item:   FetchedItem{Title: "Article Title", ProvenanceURL: &provenanceURL, Body: "summary text"},
			want:   "[news: Example Blog] Article Title\nhttps://example.com/article\n\nsummary text",
		},
		{
			name:   "title without source display name",
			source: domain.ExternalSource{Kind: "rss"},
			kind:   domain.EntryNews,
			item:   FetchedItem{Title: "Article Title", Body: "summary text"},
			want:   "[news] Article Title\n\nsummary text",
		},
		{
			name:   "title without provenance URL",
			source: domain.ExternalSource{Kind: "rss", DisplayName: &displayName},
			kind:   domain.EntryNews,
			item:   FetchedItem{Title: "Article Title", Body: "summary text"},
			want:   "[news: Example Blog] Article Title\n\nsummary text",
		},
		{
			name:   "empty source display name treated as absent",
			source: domain.ExternalSource{Kind: "rss", DisplayName: new(string)},
			kind:   domain.EntryNews,
			item:   FetchedItem{Title: "Article Title", Body: "summary text"},
			want:   "[news] Article Title\n\nsummary text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := composeExternalBody(tt.source, tt.kind, tt.item); got != tt.want {
				t.Errorf("composeExternalBody() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHandle_RSSItem_StoresComposedBodyWithTitleMarker is
// TestHandle_Success_CreatesEntriesAndAdvancesCursor's end-to-end
// counterpart for AC5: it asserts the entry actually persisted by
// Handle carries the composed provenance-marker body, not the adapter's
// raw item.Body.
func TestHandle_RSSItem_StoresComposedBodyWithTitleMarker(t *testing.T) {
	db := newTestDB(t)
	displayName := "Example Blog"
	source := domain.ExternalSource{ID: domain.NewID(), Kind: "rss", URI: "https://example.com/feed.xml", DisplayName: &displayName, CreatedAt: time.Now().UTC()}
	if err := db.ExternalSources.Create(t.Context(), source); err != nil {
		t.Fatalf("create external source: %v", err)
	}
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	provenanceURL := "https://example.com/article"
	adapter := &fakeAdapter{kind: "rss", fn: func(ctx context.Context, source domain.ExternalSource, cursor *string) (FetchResult, error) {
		return FetchResult{
			Items: []FetchedItem{
				{ExternalID: "guid-1", DedupeKey: "dedupe-1", Title: "Article Title", ProvenanceURL: &provenanceURL, Body: "summary text"},
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

	item, err := db.ExternalItems.GetByDedupeKey(t.Context(), "dedupe-1")
	if err != nil {
		t.Fatal(err)
	}
	if item.EntryID == nil {
		t.Fatal("item was not promoted to an entry")
	}
	entry, err := db.Entries.Get(t.Context(), *item.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	want := "[news: Example Blog] Article Title\nhttps://example.com/article\n\nsummary text"
	if entry.Body != want {
		t.Errorf("entry.Body = %q, want %q", entry.Body, want)
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
