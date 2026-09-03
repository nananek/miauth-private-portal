package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestThreadRepository_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	id := domain.NewID()

	if err := db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := db.Threads.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
}

func TestThreadRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Threads.Get(t.Context(), "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestThreadRepository_Touch(t *testing.T) {
	db := newTestDB(t)
	created := time.Now()
	id := domain.NewID()
	if err := db.Threads.Create(t.Context(), domain.Thread{ID: id, CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatal(err)
	}

	later := created.Add(time.Hour)
	if err := db.Threads.Touch(t.Context(), id, later); err != nil {
		t.Fatalf("touch: %v", err)
	}

	got, err := db.Threads.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, later)
	}
}

func TestThreadRepository_Touch_NotFound(t *testing.T) {
	db := newTestDB(t)
	err := db.Threads.Touch(t.Context(), "missing", time.Now())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Touch() error = %v, want ErrNotFound", err)
	}
}
