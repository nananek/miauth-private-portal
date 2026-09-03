package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestUserTagRepository_AddListRemove(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	if err := db.UserTags.Add(t.Context(), entry.ID, "go", now); err != nil {
		t.Fatal(err)
	}
	if err := db.UserTags.Add(t.Context(), entry.ID, "sqlite", now); err != nil {
		t.Fatal(err)
	}

	tags, err := db.UserTags.ListByEntry(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("len(tags) = %d, want 2", len(tags))
	}

	if err := db.UserTags.Remove(t.Context(), entry.ID, "go"); err != nil {
		t.Fatal(err)
	}
	tags, err = db.UserTags.ListByEntry(t.Context(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != "sqlite" {
		t.Errorf("tags after remove = %v, want [sqlite]", tags)
	}
}

func TestUserTagRepository_Add_RejectsDuplicateTag(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	if err := db.UserTags.Add(t.Context(), entry.ID, "go", now); err != nil {
		t.Fatal(err)
	}
	err := db.UserTags.Add(t.Context(), entry.ID, "go", now)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("Add() error = %v, want ErrConflict", err)
	}
}

func TestUserTagRepository_Remove_NotFound(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	now := time.Now()
	entry := mustCreateThreadAndRoot(t, db, actorID, now)

	err := db.UserTags.Remove(t.Context(), entry.ID, "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Remove() error = %v, want ErrNotFound", err)
	}
}
