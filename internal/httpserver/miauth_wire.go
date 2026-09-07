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
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Name     *string `json:"name"`
	// Host mirrors userLite's own convention (noteapi_wire.go): nil
	// (host: null) for every local actor, and the same fixed
	// presentation host resolveUserLite gives an Open WebUI model actor
	// (Issue #52) elsewhere. It stays nil for /api/i and the MiAuth
	// check response — both only ever project the local owner — and is
	// only ever non-nil from users/search's Open WebUI model results
	// (Issue #65). docs/compat/aria-v1.5.11.md's "UserDetailedNotMe
	// minimum" lists host as nullable/omittable, so leaving it nil here
	// is a valid response, not a gap.
	Host           *string `json:"host"`
	CreatedAt      string  `json:"createdAt"`
	IsBot          bool    `json:"isBot"`
	IsCat          bool    `json:"isCat"`
	IsLocked       bool    `json:"isLocked"`
	IsSilenced     bool    `json:"isSilenced"`
	IsSuspended    bool    `json:"isSuspended"`
	FollowersCount int     `json:"followersCount"`
	FollowingCount int     `json:"followingCount"`
	NotesCount     int     `json:"notesCount"`
	// Url is always nil: this deployment has no per-user profile URL
	// concept. Its purpose is purely structural — misskey_dart's
	// polymorphic User.fromJson (lib/src/data/base/user.dart) decodes an
	// object as UserDetailed only when the "url" key is present at all
	// (any value, including null); every projection built through this
	// struct (POST /api/i, MiAuth check, users/search) must carry the
	// key so Aria's own `.whereType<UserDetailed>()` call sites never
	// silently drop it (Issue #65).
	Url *string `json:"url"`
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
