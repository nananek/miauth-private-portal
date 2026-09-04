package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestWithinTx_CommitsOnSuccess(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entryID := domain.NewID()

	err := db.WithinTx(t.Context(), func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Threads.Create(ctx, domain.Thread{ID: entryID, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return repos.Entries.Create(ctx, domain.Entry{
			ID: entryID, ThreadID: entryID, Kind: domain.EntryUserPost, AuthorActorID: actorID,
			Body: "hello", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}

	if _, err := db.Entries.Get(t.Context(), entryID); err != nil {
		t.Errorf("entry should exist after commit: %v", err)
	}
}

func TestWithinTx_RollsBackOnError(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entryID := domain.NewID()
	sentinelErr := errors.New("boom")

	err := db.WithinTx(t.Context(), func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Threads.Create(ctx, domain.Thread{ID: entryID, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := repos.Entries.Create(ctx, domain.Entry{
			ID: entryID, ThreadID: entryID, Kind: domain.EntryUserPost, AuthorActorID: actorID,
			Body: "hello", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		return sentinelErr
	})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("WithinTx error = %v, want %v", err, sentinelErr)
	}

	if _, err := db.Entries.Get(t.Context(), entryID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("entry should not exist after rollback, got err = %v", err)
	}
	if _, err := db.Threads.Get(t.Context(), entryID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("thread should not exist after rollback, got err = %v", err)
	}
}

// TestWithinTx_EntryAndJobIntentCommitAtomically evidences AGENTS.md's
// "commit the post and durable job intent atomically" requirement: a
// conflicting job insert inside the transaction must also undo the
// entry created earlier in the same callback.
func TestWithinTx_EntryAndJobIntentCommitAtomically(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entryID := domain.NewID()
	dupKey := "dup-key"

	if err := db.Jobs.Enqueue(t.Context(), domain.Job{
		ID: domain.NewID(), JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
		IdempotencyKey: &dupKey, NextRunAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	err := db.WithinTx(t.Context(), func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Threads.Create(ctx, domain.Thread{ID: entryID, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := repos.Entries.Create(ctx, domain.Entry{
			ID: entryID, ThreadID: entryID, Kind: domain.EntryUserPost, AuthorActorID: actorID,
			Body: "hello", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		return repos.Jobs.Enqueue(ctx, domain.Job{
			ID: domain.NewID(), JobType: "test", Payload: "{}", PayloadVersion: 1, State: domain.JobPending,
			IdempotencyKey: &dupKey, NextRunAt: now, CreatedAt: now, UpdatedAt: now,
		})
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("WithinTx error = %v, want ErrConflict", err)
	}

	if _, err := db.Entries.Get(t.Context(), entryID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("entry should have been rolled back alongside the failed job insert, got err = %v", err)
	}
}

func TestWithinTx_RollsBackOnPanic(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entryID := domain.NewID()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected WithinTx to re-panic")
			}
		}()
		_ = db.WithinTx(t.Context(), func(ctx context.Context, repos domain.Repos) error {
			if err := repos.Threads.Create(ctx, domain.Thread{ID: entryID, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := repos.Entries.Create(ctx, domain.Entry{
				ID: entryID, ThreadID: entryID, Kind: domain.EntryUserPost, AuthorActorID: actorID,
				Body: "hello", ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if _, err := db.Entries.Get(t.Context(), entryID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("entry should not exist after a panicking transaction, got err = %v", err)
	}
}
