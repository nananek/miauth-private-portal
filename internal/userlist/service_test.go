package userlist

import (
	"errors"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestCreate_AssignsTimestampsFromClock(t *testing.T) {
	ts := newTestService(t)

	l, err := ts.Create(t.Context(), "AI")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if l.Name != "AI" || l.IsPublic {
		t.Errorf("l = %+v, want Name=AI, IsPublic=false", l)
	}
	if !l.CreatedAt.Equal(ts.clock.Now()) || !l.UpdatedAt.Equal(ts.clock.Now()) {
		t.Errorf("timestamps = (%v, %v), want %v", l.CreatedAt, l.UpdatedAt, ts.clock.Now())
	}

	got, err := ts.Get(t.Context(), l.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != l {
		t.Errorf("Get = %+v, want %+v", got, l)
	}
}

func TestGet_UnknownIDIsNotFound(t *testing.T) {
	ts := newTestService(t)
	if _, err := ts.Get(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListAll_ReturnsEveryCreatedList(t *testing.T) {
	ts := newTestService(t)
	first, err := ts.Create(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	second, err := ts.Create(t.Context(), "second")
	if err != nil {
		t.Fatal(err)
	}

	got, err := ts.ListAll(t.Context())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("ListAll = %v, want [first, second]", got)
	}
}

func TestUpdate_PartialChangeBumpsUpdatedAt(t *testing.T) {
	ts := newTestService(t)
	l, err := ts.Create(t.Context(), "original")
	if err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Hour)

	newName := "renamed"
	got, err := ts.Update(t.Context(), l.ID, &newName, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "renamed" || got.IsPublic {
		t.Errorf("got = %+v, want Name=renamed, IsPublic unchanged (false)", got)
	}
	if !got.UpdatedAt.Equal(ts.clock.Now()) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, ts.clock.Now())
	}
	if !got.CreatedAt.Equal(l.CreatedAt) {
		t.Errorf("CreatedAt changed: got %v, want unchanged %v", got.CreatedAt, l.CreatedAt)
	}

	isPublic := true
	got, err = ts.Update(t.Context(), l.ID, nil, &isPublic)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "renamed" || !got.IsPublic {
		t.Errorf("got = %+v, want Name unchanged=renamed, IsPublic=true", got)
	}
}

func TestUpdate_UnknownIDIsNotFound(t *testing.T) {
	ts := newTestService(t)
	name := "x"
	if _, err := ts.Update(t.Context(), "does-not-exist", &name, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDelete_RemovesListAndItsMembership(t *testing.T) {
	ts := newTestService(t)
	l, err := ts.Create(t.Context(), "to-delete")
	if err != nil {
		t.Fatal(err)
	}
	actorID := mustCreateMemberActor(t, ts.db)
	if err := ts.AddMember(t.Context(), l.ID, actorID); err != nil {
		t.Fatal(err)
	}

	if err := ts.Delete(t.Context(), l.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ts.Get(t.Context(), l.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestDelete_UnknownIDIsNotFound(t *testing.T) {
	ts := newTestService(t)
	if err := ts.Delete(t.Context(), "does-not-exist"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAddMember_RejectsUnknownList(t *testing.T) {
	ts := newTestService(t)
	actorID := mustCreateMemberActor(t, ts.db)
	if err := ts.AddMember(t.Context(), "does-not-exist", actorID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAddMember_IdempotentOnDuplicate(t *testing.T) {
	ts := newTestService(t)
	l, err := ts.Create(t.Context(), "l")
	if err != nil {
		t.Fatal(err)
	}
	actorID := mustCreateMemberActor(t, ts.db)

	if err := ts.AddMember(t.Context(), l.ID, actorID); err != nil {
		t.Fatalf("AddMember (first): %v", err)
	}
	if err := ts.AddMember(t.Context(), l.ID, actorID); err != nil {
		t.Fatalf("AddMember (duplicate): %v", err)
	}

	members, err := ts.MemberActorIDs(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0] != actorID {
		t.Fatalf("MemberActorIDs = %v, want [%s]", members, actorID)
	}
}

func TestRemoveMember_RejectsUnknownList(t *testing.T) {
	ts := newTestService(t)
	actorID := mustCreateMemberActor(t, ts.db)
	if err := ts.RemoveMember(t.Context(), "does-not-exist", actorID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRemoveMember_IdempotentOnNonMember(t *testing.T) {
	ts := newTestService(t)
	l, err := ts.Create(t.Context(), "l")
	if err != nil {
		t.Fatal(err)
	}
	actorID := mustCreateMemberActor(t, ts.db)

	if err := ts.RemoveMember(t.Context(), l.ID, actorID); err != nil {
		t.Errorf("RemoveMember on non-member: %v", err)
	}
}

func TestAddRemoveMember_MemberActorIDsReflectsCurrentMembership(t *testing.T) {
	ts := newTestService(t)
	l, err := ts.Create(t.Context(), "l")
	if err != nil {
		t.Fatal(err)
	}
	actor1 := mustCreateMemberActor(t, ts.db)
	actor2 := mustCreateMemberActor(t, ts.db)

	if err := ts.AddMember(t.Context(), l.ID, actor1); err != nil {
		t.Fatal(err)
	}
	ts.clock.Advance(time.Minute)
	if err := ts.AddMember(t.Context(), l.ID, actor2); err != nil {
		t.Fatal(err)
	}

	members, err := ts.MemberActorIDs(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0] != actor1 || members[1] != actor2 {
		t.Fatalf("MemberActorIDs = %v, want [%s, %s]", members, actor1, actor2)
	}

	if err := ts.RemoveMember(t.Context(), l.ID, actor1); err != nil {
		t.Fatal(err)
	}
	members, err = ts.MemberActorIDs(t.Context(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0] != actor2 {
		t.Fatalf("MemberActorIDs after remove = %v, want [%s]", members, actor2)
	}
}
