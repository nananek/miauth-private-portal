package timeline

import (
	"sync"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// fakeBroadcaster records every entry Publish is called with, for
// Issue #95 PR2's EntryBroadcaster wiring tests. Access is mutex-guarded
// even though every test below calls into the Service synchronously, to
// stay correct if a future test exercises this concurrently.
type fakeBroadcaster struct {
	mu      sync.Mutex
	entries []domain.Entry
}

func (b *fakeBroadcaster) Publish(entry domain.Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, entry)
}

func (b *fakeBroadcaster) published() []domain.Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.Entry(nil), b.entries...)
}

func TestCreateRoot_BroadcastsAfterCommit(t *testing.T) {
	fb := &fakeBroadcaster{}
	ts := newTestServiceWithBroadcaster(t, fb)

	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}

	published := fb.published()
	if len(published) != 1 || published[0].ID != entry.ID {
		t.Errorf("published = %+v, want exactly [%q]", published, entry.ID)
	}
}

func TestCreateRoot_RollbackNeverBroadcasts(t *testing.T) {
	fb := &fakeBroadcaster{}
	ts := newTestServiceWithBroadcaster(t, fb)

	conflictingKey := "dup-key"
	job := newTestJob(ts.clock.Now(), &conflictingKey)
	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "first", &job); err != nil {
		t.Fatalf("seed first job: %v", err)
	}
	fb.entries = nil // only care about the second, conflicting attempt below

	conflictingJob := newTestJob(ts.clock.Now(), &conflictingKey)
	_, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "second", &conflictingJob)
	if err == nil {
		t.Fatal("expected the conflicting idempotency key to roll back CreateRoot, got nil error")
	}

	if published := fb.published(); len(published) != 0 {
		t.Errorf("published = %+v, want none for a rolled-back create", published)
	}
}

func TestCreateReply_BroadcastsAfterCommit(t *testing.T) {
	fb := &fakeBroadcaster{}
	ts := newTestServiceWithBroadcaster(t, fb)

	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	fb.entries = nil // only care about the reply below

	reply, err := ts.CreateReply(t.Context(), root.ID, domain.EntryUserPost, "reply", nil)
	if err != nil {
		t.Fatal(err)
	}

	published := fb.published()
	if len(published) != 1 || published[0].ID != reply.ID {
		t.Errorf("published = %+v, want exactly [%q]", published, reply.ID)
	}
}

func TestCreateGeneratedReply_BroadcastsAfterCommit(t *testing.T) {
	fb := &fakeBroadcaster{}
	ts := newTestServiceWithBroadcaster(t, fb)

	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatal(err)
	}
	fb.entries = nil // only care about the generated reply below

	reply, err := ts.CreateGeneratedReply(t.Context(), root.ID, domain.EntryLLMReply, "generated", gen.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	published := fb.published()
	if len(published) != 1 || published[0].ID != reply.ID {
		t.Errorf("published = %+v, want exactly [%q]", published, reply.ID)
	}
}

func TestCreateGeneratedReplyBy_BroadcastsAfterCommit(t *testing.T) {
	fb := &fakeBroadcaster{}
	ts := newTestServiceWithBroadcaster(t, fb)

	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := ts.db.Actors.GetByType(t.Context(), domain.ActorAssistant)
	if err != nil {
		t.Fatal(err)
	}
	fb.entries = nil // only care about the generated reply below

	reply, err := ts.CreateGeneratedReplyBy(t.Context(), assistant.ID, root.ID, "generated", nil)
	if err != nil {
		t.Fatal(err)
	}

	published := fb.published()
	if len(published) != 1 || published[0].ID != reply.ID {
		t.Errorf("published = %+v, want exactly [%q]", published, reply.ID)
	}
}

func TestCreateExternalEntry_BroadcastsOnlyWhenActuallyCreated(t *testing.T) {
	fb := &fakeBroadcaster{}
	ts := newTestServiceWithBroadcaster(t, fb)
	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "guid-1", DedupeKey: "dedupe-1"}

	first, created, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "first delivery")
	if err != nil {
		t.Fatalf("first CreateExternalEntry: %v", err)
	}
	if !created {
		t.Fatal("first delivery: created = false, want true")
	}
	if published := fb.published(); len(published) != 1 || published[0].ID != first.ID {
		t.Errorf("after first delivery, published = %+v, want exactly [%q]", published, first.ID)
	}

	// A re-delivery of the same dedupe key (retried job, duplicate feed
	// fetch) must not push the same entry to live connections again.
	_, created, err = ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "first delivery")
	if err != nil {
		t.Fatalf("second CreateExternalEntry: %v", err)
	}
	if created {
		t.Fatal("second delivery: created = true, want false")
	}
	if published := fb.published(); len(published) != 1 {
		t.Errorf("after duplicate delivery, published = %+v, want still exactly the first entry", published)
	}
}

func TestCreateRoot_NilBroadcasterIsSafe(t *testing.T) {
	ts := newTestService(t) // no Broadcaster configured
	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "hello", nil); err != nil {
		t.Fatalf("CreateRoot with no broadcaster configured: %v", err)
	}
}
