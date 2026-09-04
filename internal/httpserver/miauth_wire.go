package httpserver

import "time"

// userDetailedNotMe is the Misskey-compatible projection
// docs/compat/aria-v1.5.11.md's UserDetailedNotMe minimum requires as
// the check success response's `user` field. Every boolean is an honest
// false, since this deployment implements no follower or moderation
// functionality for its single owner. notesCount is real (Issue #7 wires
// it to internal/timeline.Service.CountByAuthor); it was an honest 0
// before Issue #7 implemented any note functionality to count.
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

func newUserDetailedNotMe(actorID, username, displayName string, createdAt time.Time, notesCount int) userDetailedNotMe {
	var name *string
	if displayName != "" {
		dn := displayName
		name = &dn
	}
	return userDetailedNotMe{
		ID:         actorID,
		Username:   username,
		Name:       name,
		CreatedAt:  createdAt.UTC().Format(time.RFC3339),
		NotesCount: notesCount,
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
