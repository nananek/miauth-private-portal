package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestScheduler_TickEnqueuesOneJobPerSource(t *testing.T) {
	db := newTestDB(t)
	mustCreateSource(t, db, "rss", "https://example.com/a.xml")
	mustCreateSource(t, db, "rss", "https://example.com/b.xml")

	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{Kind: "rss", PollInterval: time.Hour}, nil)
	scheduler.tick(t.Context())

	jobs, err := db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}
	for _, j := range jobs {
		if j.JobType != JobType {
			t.Errorf("JobType = %q, want %q", j.JobType, JobType)
		}
		if j.SourceEntryID != nil {
			t.Errorf("SourceEntryID = %v, want nil (ingestion jobs are not tied to a timeline entry)", j.SourceEntryID)
		}
	}
}

// TestScheduler_TickWithinSameWindowDoesNotDoubleEnqueue documents the
// idempotency-key collision guard: two ticks landing in the same
// truncated poll-interval window (a fast restart, or this test calling
// tick twice back to back) must not enqueue a second job for the same
// source.
func TestScheduler_TickWithinSameWindowDoesNotDoubleEnqueue(t *testing.T) {
	db := newTestDB(t)
	mustCreateSource(t, db, "rss", "https://example.com/a.xml")

	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{Kind: "rss", PollInterval: time.Hour}, nil)
	scheduler.tick(t.Context())
	scheduler.tick(t.Context())

	jobs, err := db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("len(jobs) = %d, want 1 after two ticks in the same window", len(jobs))
	}
}

// TestScheduler_TickOnlyEnqueuesConfiguredKind documents the fix for the
// double-enqueue bug this ticket's plan called out: two Scheduler
// instances, each scoped to its own Kind with its own PollInterval, must
// each enqueue jobs only for sources of their own kind, never for the
// other's.
func TestScheduler_TickOnlyEnqueuesConfiguredKind(t *testing.T) {
	db := newTestDB(t)
	mustCreateSource(t, db, "rss", "https://example.com/a.xml")
	mustCreateSource(t, db, "rss", "https://example.com/b.xml")
	mustCreateSource(t, db, "imap", "imap://example.com/inbox")

	rssScheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{Kind: "rss", PollInterval: time.Hour}, nil)
	imapScheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{Kind: "imap", PollInterval: time.Minute}, nil)

	rssScheduler.tick(t.Context())
	imapScheduler.tick(t.Context())

	jobs, err := db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("len(jobs) = %d, want 3 (2 rss + 1 imap, no double-enqueue)", len(jobs))
	}
}

// TestScheduler_RunStopsOnCancel is a shutdown-path regression test: Run
// must return promptly (rather than hang on its ticker) once ctx is
// cancelled.
func TestScheduler_RunStopsOnCancel(t *testing.T) {
	db := newTestDB(t)
	mustCreateSource(t, db, "rss", "https://example.com/a.xml")

	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{Kind: "rss", PollInterval: time.Hour}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := scheduler.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestScheduler_Tick_ReconcilesDesiredURIsBeforeListing backs Issue #76
// PR4a's RSS_FEED_URLS reload: a source no longer among DesiredURIs is
// deactivated (and so no longer enqueued) in the very tick that notices
// its removal, without a restart.
func TestScheduler_Tick_ReconcilesDesiredURIsBeforeListing(t *testing.T) {
	db := newTestDB(t)
	mustCreateSource(t, db, "rss", "https://example.com/old.xml")

	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{
		Kind:         "rss",
		PollInterval: time.Hour,
		DesiredURIs:  func(context.Context) []string { return []string{"https://example.com/new.xml"} },
	}, nil)
	scheduler.tick(t.Context())

	jobs, err := db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1 (only the reconciled new source; the old one was deactivated)", len(jobs))
	}

	sources, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].URI != "https://example.com/new.xml" {
		t.Fatalf("List = %+v, want only the new URI active", sources)
	}
}

// TestScheduler_Tick_NilDesiredURIsSkipsReconciliation preserves this
// type's pre-Issue #76 behavior for a caller unaware of the DB overlay:
// a source is never touched by tick unless DesiredURIs is explicitly
// set.
func TestScheduler_Tick_NilDesiredURIsSkipsReconciliation(t *testing.T) {
	db := newTestDB(t)
	mustCreateSource(t, db, "rss", "https://example.com/a.xml")

	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{Kind: "rss", PollInterval: time.Hour}, nil)
	scheduler.tick(t.Context())

	sources, err := db.ExternalSources.List(t.Context(), "rss")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("List = %+v, want the pre-existing source untouched", sources)
	}
}

func TestScheduler_ReloadInterval_UpdatesIntervalAndResetsTicker(t *testing.T) {
	db := newTestDB(t)
	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{
		Kind:               "rss",
		PollInterval:       time.Hour,
		ReloadPollInterval: func(context.Context) time.Duration { return 5 * time.Minute },
	}, nil)
	scheduler.ticker = time.NewTicker(time.Hour)
	defer scheduler.ticker.Stop()

	scheduler.reloadInterval(t.Context())

	if scheduler.interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", scheduler.interval)
	}
}

func TestScheduler_ReloadInterval_NilReloadFuncIsNoop(t *testing.T) {
	db := newTestDB(t)
	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{Kind: "rss", PollInterval: time.Hour}, nil)
	scheduler.ticker = time.NewTicker(time.Hour)
	defer scheduler.ticker.Stop()

	scheduler.reloadInterval(t.Context())

	if scheduler.interval != time.Hour {
		t.Errorf("interval = %v, want unchanged 1h", scheduler.interval)
	}
}

// TestScheduler_ReloadInterval_NonPositiveResultIsIgnored guards against
// a misbehaving ReloadPollInterval (for example a Store read that
// somehow returns 0) ever installing a busy-looping ticker interval.
func TestScheduler_ReloadInterval_NonPositiveResultIsIgnored(t *testing.T) {
	db := newTestDB(t)
	scheduler := NewScheduler(db.ExternalSources, db.Jobs, SchedulerConfig{
		Kind:               "rss",
		PollInterval:       time.Hour,
		ReloadPollInterval: func(context.Context) time.Duration { return 0 },
	}, nil)
	scheduler.ticker = time.NewTicker(time.Hour)
	defer scheduler.ticker.Stop()

	scheduler.reloadInterval(t.Context())

	if scheduler.interval != time.Hour {
		t.Errorf("interval = %v, want unchanged 1h", scheduler.interval)
	}
}
