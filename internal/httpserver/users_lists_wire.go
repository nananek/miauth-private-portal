package httpserver

import (
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// userListResponse is the Misskey-compatible projection of one
// domain.UserList (Issue #115, docs/compat/aria-v1.5.11.md's
// users/lists/* section; plan-115 §1/§2.4's traced misskey_dart
// UsersList/UsersListsShowResponse shape — the same type is reused for
// create/update/list/show). LikedCount/IsLiked are always 0/false: real
// Misskey's list-favoriting is a federation-facing feature this
// non-federating deployment does not implement (Issue #115 Non-goals).
type userListResponse struct {
	ID         string   `json:"id"`
	CreatedAt  string   `json:"createdAt"`
	Name       string   `json:"name"`
	UserIDs    []string `json:"userIds"`
	IsPublic   bool     `json:"isPublic"`
	LikedCount int      `json:"likedCount"`
	IsLiked    bool     `json:"isLiked"`
}

// projectUserList projects l alongside its already-fetched member actor
// IDs. memberActorIDs is rendered as [] rather than null when empty,
// matching every other wire array projection in this package (e.g.
// projectNote's reactions map is never a bare null either).
func projectUserList(l domain.UserList, memberActorIDs []string) userListResponse {
	userIDs := memberActorIDs
	if userIDs == nil {
		userIDs = []string{}
	}
	return userListResponse{
		ID:        l.ID,
		CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339),
		Name:      l.Name,
		UserIDs:   userIDs,
		IsPublic:  l.IsPublic,
	}
}

type usersListsCreateRequest struct {
	Name string `json:"name"`
}

type usersListsShowRequest struct {
	ListID string `json:"listId"`
}

// usersListsUpdateRequest mirrors misskey_dart's UsersListsUpdateRequest
// (plan-115 §1's traced field table): Name/IsPublic are each optional,
// so a nil value here must leave that field unchanged rather than
// clearing it (internal/userlist.Service.Update's own contract) — Name
// is never intentionally set back to empty and IsPublic is never
// intentionally cleared to "unset", so there is no absent-vs-null
// distinction to plumb the way handleDriveFoldersUpdate's parentId
// needs (drive_handlers.go), and a plain decodeJSONBody suffices.
type usersListsUpdateRequest struct {
	ListID   string  `json:"listId"`
	Name     *string `json:"name"`
	IsPublic *bool   `json:"isPublic"`
}

type usersListsDeleteRequest struct {
	ListID string `json:"listId"`
}

type usersListsPushRequest struct {
	ListID string `json:"listId"`
	UserID string `json:"userId"`
}

type usersListsPullRequest struct {
	ListID string `json:"listId"`
	UserID string `json:"userId"`
}
