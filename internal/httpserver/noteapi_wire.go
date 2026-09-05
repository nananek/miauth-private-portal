package httpserver

import (
	"context"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// userLite is the Misskey-compatible minimal user projection embedded in
// every Note.user (docs/compat/aria-v1.5.11.md's "Minimum Note contract":
// UserLite requires only id and username; host is nullable). host is
// always null: this service has no federation and every actor it can
// project is local (AGENTS.md: no federation).
type userLite struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Host     *string `json:"host"`
}

// note is the Misskey-compatible projection of one domain.Entry. Fields
// beyond the pinned parser's required minimum (id, createdAt, user,
// userId) use the pinned parser's documented defaults (empty maps/lists,
// false localOnly, null optional strings/IDs) rather than being omitted,
// matching fixtures/note.json. visibility is always "public" and
// localOnly is always false: this service has no per-note visibility or
// federation concept (AGENTS.md: single owner, no federation).
// repliesCount is always 0 rather than computed from ListChildren: doing
// so would cost an extra query per note on every timeline/conversation
// page, and the field is not part of the pinned parser's required
// minimum — Aria's thread view calls /api/notes/children directly rather
// than relying on this count (see docs/compat/aria-v1.5.11.md).
type note struct {
	ID             string            `json:"id"`
	CreatedAt      string            `json:"createdAt"`
	Text           *string           `json:"text"`
	CW             *string           `json:"cw"`
	User           userLite          `json:"user"`
	UserID         string            `json:"userId"`
	ReplyID        *string           `json:"replyId"`
	RenoteID       *string           `json:"renoteId"`
	Visibility     string            `json:"visibility"`
	LocalOnly      bool              `json:"localOnly"`
	RenoteCount    int               `json:"renoteCount"`
	RepliesCount   int               `json:"repliesCount"`
	Reactions      map[string]int    `json:"reactions"`
	Emojis         map[string]string `json:"emojis"`
	FileIDs        []string          `json:"fileIds"`
	Files          []any             `json:"files"`
	VisibleUserIDs []string          `json:"visibleUserIds"`
	Mentions       []string          `json:"mentions"`
}

// newNote projects e onto the wire Note type. user is the already-resolved
// projection of e.AuthorActorID (see (*Server).resolveUserLite): building
// it is the caller's job because it may require an owner-profile lookup
// or an actor-type lookup this package deliberately keeps out of the pure
// wire-conversion helpers.
func newNote(e domain.Entry, user userLite) note {
	text := wireText(e)
	return note{
		ID:             e.ID,
		CreatedAt:      e.CreatedAt.UTC().Format(time.RFC3339),
		Text:           &text,
		CW:             nil,
		User:           user,
		UserID:         user.ID,
		ReplyID:        e.ParentEntryID,
		RenoteID:       nil,
		Visibility:     "public",
		LocalOnly:      false,
		RenoteCount:    0,
		RepliesCount:   0,
		Reactions:      map[string]int{},
		Emojis:         map[string]string{},
		FileIDs:        []string{},
		Files:          []any{},
		VisibleUserIDs: []string{},
		Mentions:       []string{},
	}
}

// wireText composes the wire-visible note text. Only llm_reply/
// llm_follow_up get a fixed distinguishing marker: Misskey's Note has
// no "kind" field, so this is the only way Aria's timeline can tell a
// generated reply from a generated follow-up question apart. This is
// presentation-only — domain.Entry.Body and LLMGeneration.Body (the
// generation audit record) are never touched, matching AGENTS.md's
// "keep wire projections separate from domain models". EntryUserPost is
// deliberately excluded: user-authored text is authoritative and must
// never be altered (AGENTS.md).
func wireText(e domain.Entry) string {
	switch e.Kind {
	case domain.EntryLLMReply:
		return "[reply]\n\n" + e.Body
	case domain.EntryLLMFollowUp:
		return "[follow-up question]\n\n" + e.Body
	default:
		return e.Body
	}
}

// newUserLiteFromOwner projects a miauth.OwnerProfile onto userLite.
func newUserLiteFromOwner(owner miauth.OwnerProfile) userLite {
	return userLite{ID: owner.ActorID, Username: owner.Username}
}

// resolveUserLite builds the userLite projection for an entry's
// AuthorActorID. The common case (an entry authored by the requesting
// owner) is resolved from the already-fetched owner profile with no
// extra lookup; any other author (the reserved assistant/system
// presentation actors — AGENTS.md forbids any other login-capable
// local user) falls back to one ResolveAuthor lookup to tell them apart,
// and finally to the actor ID itself as username if even that fails,
// so a wire projection is always produced rather than an internal error
// surfacing mid-response.
func (s *Server) resolveUserLite(ctx context.Context, authorActorID string, owner miauth.OwnerProfile) userLite {
	if authorActorID == owner.ActorID {
		return newUserLiteFromOwner(owner)
	}
	if s.timeline != nil {
		if actor, err := s.timeline.ResolveAuthor(ctx, authorActorID); err == nil {
			switch actor.Type {
			case domain.ActorAssistant:
				return userLite{ID: authorActorID, Username: "assistant"}
			case domain.ActorSystem:
				return userLite{ID: authorActorID, Username: "system"}
			}
		}
	}
	return userLite{ID: authorActorID, Username: authorActorID}
}

// meDetailed is the Misskey-compatible MeDetailed projection POST /api/i
// always returns (docs/compat/aria-v1.5.11.md: the token-login fallback
// and the post-login full-account load both call this endpoint, and the
// two cannot be told apart from the request body alone, so this service
// always returns the MeDetailed superset — its extra required fields
// parse successfully as the fallback's minimal {id, username} shape
// too). isModerator/isAdmin are true: the single owner is this
// deployment's only login-capable actor and is administrator-equivalent
// by construction. alwaysMarkNsfw/carefulBot/autoAcceptFollowed are
// false safe-side defaults; this service implements none of the
// features they gate.
type meDetailed struct {
	userDetailedNotMe
	IsModerator        bool `json:"isModerator"`
	IsAdmin            bool `json:"isAdmin"`
	AlwaysMarkNsfw     bool `json:"alwaysMarkNsfw"`
	CarefulBot         bool `json:"carefulBot"`
	AutoAcceptFollowed bool `json:"autoAcceptFollowed"`
}

func newMeDetailed(owner miauth.OwnerProfile, notesCount int) meDetailed {
	return meDetailed{
		userDetailedNotMe: newUserDetailedNotMe(owner.ActorID, owner.Username, owner.DisplayName, owner.CreatedAt, notesCount),
		IsModerator:       true,
		IsAdmin:           true,
	}
}
