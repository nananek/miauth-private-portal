package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func mustCreateUserList(t *testing.T, db *DB, name string, at time.Time) domain.UserList {
	t.Helper()
	l := domain.UserList{ID: domain.NewID(), Name: name, CreatedAt: at, UpdatedAt: at}
	if err := db.UserLists.Create(t.Context(), l); err != nil {
		t.Fatalf("create user list %q: %v", name, err)
	}
	return l
}

func TestUserListRepository_CreateGet_RoundTripsEveryField(t *testing.T) {
	db := newTestDB(t)
	createdAt := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	l := domain.UserList{ID: domain.NewID(), Name: "AI", IsPublic: true, CreatedAt: createdAt, UpdatedAt: createdAt}
	if err := db.UserLists.Create(t.Context(), l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.UserLists.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != l.ID || got.Name != "AI" || !got.IsPublic || !got.CreatedAt.Equal(createdAt) || !got.UpdatedAt.Equal(createdAt) {
		t.Errorf("Get round-trip mismatch: got %+v, want fields from %+v", got, l)
	}
}

func TestUserListRepository_Create_DefaultsIsPublicFalse(t *testing.T) {
	db := newTestDB(t)
	l := mustCreateUserList(t, db, "system", time.Now())
	got, err := db.UserLists.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.IsPublic {
		t.Errorf("IsPublic = true, want false by default")
	}
}

func TestUserListRepository_Get_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.UserLists.Get(t.Context(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUserListRepository_ListAll_StableOrder(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	first := mustCreateUserList(t, db, "first", base)
	second := mustCreateUserList(t, db, "second", base.Add(time.Minute))

	got, err := db.UserLists.ListAll(t.Context())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("ListAll = %v, want [first, second]", userListIDs(got))
	}
}

func userListIDs(lists []domain.UserList) []string {
	out := make([]string, len(lists))
	for i, l := range lists {
		out[i] = l.ID
	}
	return out
}

func TestUserListRepository_Update_PartialChangeLeavesOtherFieldUnchanged(t *testing.T) {
	db := newTestDB(t)
	createdAt := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := domain.UserList{ID: domain.NewID(), Name: "original", IsPublic: false, CreatedAt: createdAt, UpdatedAt: createdAt}
	if err := db.UserLists.Create(t.Context(), l); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newName := "renamed"
	updatedAt := createdAt.Add(time.Hour)
	got, err := db.UserLists.Update(t.Context(), l.ID, &newName, nil, updatedAt)
	if err != nil {
		t.Fatalf("Update(name only): %v", err)
	}
	if got.Name != "renamed" || got.IsPublic {
		t.Errorf("got = %+v, want Name=renamed, IsPublic=false unchanged", got)
	}
	if !got.UpdatedAt.Equal(updatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, updatedAt)
	}

	isPublic := true
	got, err = db.UserLists.Update(t.Context(), l.ID, nil, &isPublic, updatedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("Update(isPublic only): %v", err)
	}
	if got.Name != "renamed" || !got.IsPublic {
		t.Errorf("got = %+v, want Name unchanged=renamed, IsPublic=true", got)
	}
}

func TestUserListRepository_Update_UnknownIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	name := "x"
	_, err := db.UserLists.Update(t.Context(), "does-not-exist", &name, nil, time.Now())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUserListRepository_Delete_RemovesListAndMembers(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	l := mustCreateUserList(t, db, "to-delete", time.Now())
	if err := db.UserLists.AddMember(t.Context(), l.ID, actorID, time.Now()); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if err := db.UserLists.Delete(t.Context(), l.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.UserLists.Get(t.Context(), l.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}

	// Re-creating a list with the same members table (a fresh list) and
	// confirming no orphaned membership row survived is done indirectly:
	// AddMember against a brand new list of the same ID would otherwise
	// collide on the (list_id, actor_id) primary key if the old
	// membership row were not actually gone.
	l2 := domain.UserList{ID: l.ID, Name: "recreated", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.UserLists.Create(t.Context(), l2); err != nil {
		t.Fatalf("recreate list with same id: %v", err)
	}
	members, err := db.UserLists.MemberActorIDs(t.Context(), l2.ID)
	if err != nil {
		t.Fatalf("MemberActorIDs: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("MemberActorIDs after recreate = %v, want empty (no orphaned membership)", members)
	}
}

func TestUserListRepository_Delete_UnknownIDIsNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.UserLists.Delete(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUserListRepository_AddMember_IdempotentOnDuplicate(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	l := mustCreateUserList(t, db, "l", time.Now())

	if err := db.UserLists.AddMember(t.Context(), l.ID, actorID, time.Now()); err != nil {
		t.Fatalf("AddMember (first): %v", err)
	}
	if err := db.UserLists.AddMember(t.Context(), l.ID, actorID, time.Now()); err != nil {
		t.Fatalf("AddMember (duplicate): %v", err)
	}

	members, err := db.UserLists.MemberActorIDs(t.Context(), l.ID)
	if err != nil {
		t.Fatalf("MemberActorIDs: %v", err)
	}
	if len(members) != 1 || members[0] != actorID {
		t.Fatalf("MemberActorIDs = %v, want [%s]", members, actorID)
	}
}

func TestUserListRepository_AddMember_RejectsUnknownActor(t *testing.T) {
	db := newTestDB(t)
	l := mustCreateUserList(t, db, "l", time.Now())
	err := db.UserLists.AddMember(t.Context(), l.ID, "no-such-actor", time.Now())
	if err == nil {
		t.Error("expected an error for an unknown actor_id")
	}
}

func TestUserListRepository_RemoveMember_IdempotentOnNonMember(t *testing.T) {
	db := newTestDB(t)
	actorID := mustCreateActor(t, db)
	l := mustCreateUserList(t, db, "l", time.Now())

	if err := db.UserLists.RemoveMember(t.Context(), l.ID, actorID); err != nil {
		t.Fatalf("RemoveMember on non-member: %v", err)
	}
}

func TestUserListRepository_MemberActorIDs_OrderedByAddedAt(t *testing.T) {
	db := newTestDB(t)
	l := mustCreateUserList(t, db, "l", time.Now())
	actor1 := mustCreateDistinctActor(t, db)
	actor2 := mustCreateDistinctActor(t, db)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := db.UserLists.AddMember(t.Context(), l.ID, actor2, base.Add(time.Minute)); err != nil {
		t.Fatalf("AddMember actor2: %v", err)
	}
	if err := db.UserLists.AddMember(t.Context(), l.ID, actor1, base); err != nil {
		t.Fatalf("AddMember actor1: %v", err)
	}

	members, err := db.UserLists.MemberActorIDs(t.Context(), l.ID)
	if err != nil {
		t.Fatalf("MemberActorIDs: %v", err)
	}
	if len(members) != 2 || members[0] != actor1 || members[1] != actor2 {
		t.Fatalf("MemberActorIDs = %v, want [%s, %s] (added-at order)", members, actor1, actor2)
	}

	if err := db.UserLists.RemoveMember(t.Context(), l.ID, actor1); err != nil {
		t.Fatalf("RemoveMember actor1: %v", err)
	}
	members, err = db.UserLists.MemberActorIDs(t.Context(), l.ID)
	if err != nil {
		t.Fatalf("MemberActorIDs after remove: %v", err)
	}
	if len(members) != 1 || members[0] != actor2 {
		t.Fatalf("MemberActorIDs after remove = %v, want [%s]", members, actor2)
	}
}

func TestUserListRepository_MemberActorIDs_UnknownListReturnsEmpty(t *testing.T) {
	db := newTestDB(t)
	members, err := db.UserLists.MemberActorIDs(t.Context(), "does-not-exist")
	if err != nil {
		t.Fatalf("MemberActorIDs: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("MemberActorIDs(unknown list) = %v, want empty", members)
	}
}
