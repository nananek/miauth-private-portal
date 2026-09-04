package httpserver

import (
	"time"

	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// userDetailedNotMe is the Misskey-compatible projection
// docs/compat/aria-v1.5.11.md's UserDetailedNotMe minimum requires as
// the check success response's `user` field. Only the fields backed by
// real data are populated (id, username, name, createdAt); every
// boolean is an honest false and every count an honest 0 rather than
// fabricated data, since Issue #5 implements no notes, follower, or
// moderation functionality — those all remain 0/false until a later
// issue actually implements the behavior they describe.
type userDetailedNotMe struct {
	ID             string  `json:"id"`
	Username       string  `json:"username"`
	Name           *string `json:"name"`
	CreatedAt      string  `json:"createdAt"`
	IsBot          bool    `json:"isBot"`
	IsCat          bool    `json:"isCat"`
	IsLocked       bool    `json:"isLocked"`
	IsSilenced     bool    `json:"isSilenced"`
	IsSuspended    bool    `json:"isSuspended"`
	FollowersCount int     `json:"followersCount"`
	FollowingCount int     `json:"followingCount"`
	NotesCount     int     `json:"notesCount"`
}

func newUserDetailedNotMe(result miauth.CheckResult) userDetailedNotMe {
	var name *string
	if result.OwnerDisplayName != "" {
		displayName := result.OwnerDisplayName
		name = &displayName
	}
	return userDetailedNotMe{
		ID:        result.OwnerActorID,
		Username:  result.OwnerUsername,
		Name:      name,
		CreatedAt: result.OwnerCreatedAt.UTC().Format(time.RFC3339),
	}
}

// checkSuccessResponse is POST /api/miauth/{session}/check's documented
// success shape.
type checkSuccessResponse struct {
	OK    bool              `json:"ok"`
	Token string            `json:"token"`
	User  userDetailedNotMe `json:"user"`
}

// checkFailureResponse is the uniform shape this service returns for
// every non-success check() outcome (not found, pending, denied,
// expired, replay) — see handleMiAuthCheck's doc comment for why these
// are deliberately not distinguished on the wire.
type checkFailureResponse struct {
	OK bool `json:"ok"`
}
