package httpserver

import (
	"net/http"
	"testing"
)

// listsToken issues a fresh token carrying every scope users/lists/*
// needs (read:account for list/show, write:account for
// create/update/delete/push/pull), mirroring i_update_handlers_test.go's
// own per-test mustIssueToken calls rather than newNoteAPITestServer's
// default token, which only carries write:notes/read:account.
func listsToken(t *testing.T, ts *noteAPITestServer, sessionID string) string {
	t.Helper()
	token, _ := mustIssueToken(t, ts.Server, sessionID, "read:account,write:account")
	return token
}

func TestUsersListsCreateShowUpdateDelete_FullLifecycle(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-lifecycle")

	createRec := ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "AI"})
	if createRec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", createRec.Code, createRec.Body.String())
	}
	var created userListResponse
	mustDecode(t, createRec, &created)
	if created.ID == "" || created.CreatedAt == "" {
		t.Fatalf("created = %+v, want non-empty ID/CreatedAt", created)
	}
	if created.Name != "AI" || created.IsPublic || len(created.UserIDs) != 0 || created.LikedCount != 0 || created.IsLiked {
		t.Errorf("created = %+v, want Name=AI, IsPublic=false, empty UserIDs, LikedCount=0, IsLiked=false", created)
	}

	showRec := ts.post(t, "/api/users/lists/show", map[string]any{"i": token, "listId": created.ID})
	if showRec.Code != http.StatusOK {
		t.Fatalf("show: %d %s", showRec.Code, showRec.Body.String())
	}
	var shown userListResponse
	mustDecode(t, showRec, &shown)
	if shown.ID != created.ID || shown.Name != created.Name || shown.CreatedAt != created.CreatedAt ||
		shown.IsPublic != created.IsPublic || len(shown.UserIDs) != len(created.UserIDs) {
		t.Errorf("show = %+v, want %+v", shown, created)
	}

	updateRec := ts.post(t, "/api/users/lists/update", map[string]any{
		"i": token, "listId": created.ID, "name": "AI models", "isPublic": true,
	})
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", updateRec.Code, updateRec.Body.String())
	}
	var updated userListResponse
	mustDecode(t, updateRec, &updated)
	if updated.Name != "AI models" || !updated.IsPublic {
		t.Errorf("updated = %+v, want Name=AI models, IsPublic=true", updated)
	}
	if updated.ID != created.ID || updated.CreatedAt != created.CreatedAt {
		t.Errorf("updated ID/CreatedAt changed: got %+v, want ID/CreatedAt from %+v", updated, created)
	}

	deleteRec := ts.post(t, "/api/users/lists/delete", map[string]any{"i": token, "listId": created.ID})
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", deleteRec.Code, deleteRec.Body.String())
	}

	afterDeleteRec := ts.post(t, "/api/users/lists/show", map[string]any{"i": token, "listId": created.ID})
	assertWireError(t, afterDeleteRec, http.StatusBadRequest, "NO_SUCH_LIST")
}

func TestUsersListsCreate_RequiresName(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-create-requires-name")

	rec := ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": ""})
	assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
}

func TestUsersListsShow_UnknownListIsNoSuchList(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-show-unknown")

	rec := ts.post(t, "/api/users/lists/show", map[string]any{"i": token, "listId": "does-not-exist"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_LIST")
}

func TestUsersListsList_ReturnsEveryCreatedList(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-list")

	names := []string{"first", "second"}
	for _, name := range names {
		rec := ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": name})
		if rec.Code != http.StatusOK {
			t.Fatalf("create %q: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	listRec := ts.post(t, "/api/users/lists/list", map[string]any{"i": token})
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listRec.Code, listRec.Body.String())
	}
	var lists []userListResponse
	mustDecode(t, listRec, &lists)
	if len(lists) != 2 || lists[0].Name != "first" || lists[1].Name != "second" {
		t.Fatalf("list = %+v, want [first, second]", lists)
	}
}

func TestUsersListsPush_AddsKnownActorToUserIds(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-push")

	createRec := ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "l"})
	var created userListResponse
	mustDecode(t, createRec, &created)

	pushRec := ts.post(t, "/api/users/lists/push", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID})
	if pushRec.Code != http.StatusNoContent {
		t.Fatalf("push: %d %s", pushRec.Code, pushRec.Body.String())
	}

	showRec := ts.post(t, "/api/users/lists/show", map[string]any{"i": token, "listId": created.ID})
	var shown userListResponse
	mustDecode(t, showRec, &shown)
	if len(shown.UserIDs) != 1 || shown.UserIDs[0] != ts.ownerID {
		t.Fatalf("UserIDs = %v, want [%s]", shown.UserIDs, ts.ownerID)
	}
}

func TestUsersListsPush_RejectsUnknownActor(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-push-unknown")

	createRec := ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "l"})
	var created userListResponse
	mustDecode(t, createRec, &created)

	pushRec := ts.post(t, "/api/users/lists/push", map[string]any{"i": token, "listId": created.ID, "userId": "no-such-actor"})
	assertWireError(t, pushRec, http.StatusBadRequest, "NO_SUCH_USER")

	showRec := ts.post(t, "/api/users/lists/show", map[string]any{"i": token, "listId": created.ID})
	var shown userListResponse
	mustDecode(t, showRec, &shown)
	if len(shown.UserIDs) != 0 {
		t.Errorf("UserIDs = %v, want empty after a rejected push", shown.UserIDs)
	}
}

func TestUsersListsPush_UnknownListIsNoSuchList(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-push-unknown-list")

	rec := ts.post(t, "/api/users/lists/push", map[string]any{"i": token, "listId": "does-not-exist", "userId": ts.ownerID})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_LIST")
}

func TestUsersListsPull_RemovesActor(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-pull")

	createRec := ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "l"})
	var created userListResponse
	mustDecode(t, createRec, &created)

	if rec := ts.post(t, "/api/users/lists/push", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID}); rec.Code != http.StatusNoContent {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}

	pullRec := ts.post(t, "/api/users/lists/pull", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID})
	if pullRec.Code != http.StatusNoContent {
		t.Fatalf("pull: %d %s", pullRec.Code, pullRec.Body.String())
	}

	showRec := ts.post(t, "/api/users/lists/show", map[string]any{"i": token, "listId": created.ID})
	var shown userListResponse
	mustDecode(t, showRec, &shown)
	if len(shown.UserIDs) != 0 {
		t.Errorf("UserIDs = %v, want empty after pull", shown.UserIDs)
	}
}

func TestUsersListsPull_IdempotentOnNonMember(t *testing.T) {
	ts := newNoteAPITestServer(t)
	token := listsToken(t, ts, "lists-pull-non-member")

	createRec := ts.post(t, "/api/users/lists/create", map[string]any{"i": token, "name": "l"})
	var created userListResponse
	mustDecode(t, createRec, &created)

	pullRec := ts.post(t, "/api/users/lists/pull", map[string]any{"i": token, "listId": created.ID, "userId": ts.ownerID})
	if pullRec.Code != http.StatusNoContent {
		t.Fatalf("pull on non-member: %d %s", pullRec.Code, pullRec.Body.String())
	}
}

func TestUsersListsCreate_RejectsMissingWriteAccountScope(t *testing.T) {
	ts := newNoteAPITestServer(t)
	readOnlyToken, _ := mustIssueToken(t, ts.Server, "lists-read-only-scope", "read:account")

	rec := ts.post(t, "/api/users/lists/create", map[string]any{"i": readOnlyToken, "name": "AI"})
	assertWireError(t, rec, http.StatusUnauthorized, "AUTHENTICATION_FAILED")
}
