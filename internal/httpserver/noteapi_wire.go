package httpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// userLite is the Misskey-compatible minimal user projection embedded in
// every Note.user (docs/compat/aria-v1.5.11.md's "Minimum Note contract":
// UserLite requires only id and username; host is nullable). host is
// null for every actor except one: an Open WebUI model's VirtualActor
// (Issue #52), which resolveUserLite projects with a non-null,
// deployment-configured presentation host. That is a fixed presentation
// value, not federation — AGENTS.md's "no federation" is unchanged; this
// service still does not discover, resolve, or deliver to any remote
// host. See docs/compat/aria-v1.5.11.md's note on this one exception.
type userLite struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Host     *string `json:"host"`
	// AvatarURL is nil until the projected actor has an AvatarFileID
	// (Issue #77 PR5) — an absolute GET /files/{id} URL, never the bare
	// avatarId key: docs/compat/aria-v1.5.11.md's avatarId-discriminator
	// guardrail (plan-77 v1 §1.4) applies here exactly as it does to
	// userDetailedNotMe.
	AvatarURL *string `json:"avatarUrl"`
}

// avatarURLFromFileID resolves fileID to the absolute GET /files/{id}
// URL internal/httpserver/drive_wire.go's projectDriveFile already uses
// for DriveFile.url, or nil if fileID itself is nil. It is a pure
// function (not a Server method) so noteapi_wire_test.go can exercise
// wire projection without a live localOrigin-configured Server.
func avatarURLFromFileID(localOrigin string, fileID *string) *string {
	if fileID == nil {
		return nil
	}
	u := localOrigin + "/files/" + *fileID
	return &u
}

// VirtualActorResolver projects an Open WebUI model actor into the
// VirtualActor Aria sees, or reports it as not currently projectable
// (not that actor type at all, its model deactivated, or its workspace
// disabled). resolveUserLite treats any error identically — "fall back
// to the plain projection" — so this package needs no internal/openwebui
// error sentinel and, by taking this narrow interface rather than that
// package's concrete Registry type, never imports internal/openwebui at
// all.
type VirtualActorResolver interface {
	ResolveVirtualActor(ctx context.Context, actorID string) (domain.VirtualActor, error)
}

// ExternalSourceResolver resolves an ActorExternalSource actor
// (Issue #77 PR4/ADR-0008's design-A host display) to the
// domain.ExternalSource that owns it, or reports it as not resolvable —
// resolveUserLite treats that identically to VirtualActorResolver's own
// failure case, falling back to the plain projection.
// domain.ExternalSourceRepository already satisfies this narrow
// interface structurally (its GetByActorID method), so cmd/server passes
// db.Repos.ExternalSources directly; this package still never imports a
// storage driver type.
type ExternalSourceResolver interface {
	GetByActorID(ctx context.Context, actorID string) (domain.ExternalSource, error)
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
// than relying on this count (see docs/compat/aria-v1.5.11.md). Reactions
// and MyReaction default to empty/nil from newNote itself (no reaction
// data yet); (*Server).projectNote fills them with real data from
// internal/timeline (Issue #23 PR4) — see its doc comment for why that
// population is split out of this pure conversion function.
type note struct {
	ID        string   `json:"id"`
	CreatedAt string   `json:"createdAt"`
	Text      *string  `json:"text"`
	CW        *string  `json:"cw"`
	User      userLite `json:"user"`
	UserID    string   `json:"userId"`
	ReplyID   *string  `json:"replyId"`
	RenoteID  *string  `json:"renoteId"`
	// URL is nil except for a news entry ingested from a source item
	// that had one (Issue #77 PR4: docs/compat/aria-v1.5.11.md's Minimum
	// Note contract lists "url" as nullable) — the original article's
	// URL, straight from e.ProvenanceURL. Never set for user_post/
	// llm_reply/llm_follow_up/mail (mail has no natural article URL).
	URL          *string           `json:"url"`
	Visibility   string            `json:"visibility"`
	LocalOnly    bool              `json:"localOnly"`
	RenoteCount  int               `json:"renoteCount"`
	RepliesCount int               `json:"repliesCount"`
	Reactions    map[string]int    `json:"reactions"`
	MyReaction   *string           `json:"myReaction"`
	Emojis       map[string]string `json:"emojis"`
	// FileIDs/Files default to an explicit empty array (never omitted —
	// docs/compat/aria-v1.5.11.md's PR0 finding that misskey_dart's
	// @Default([]) makes an absent key safe either way, but this
	// service's own convention is to always include the key) and are
	// populated with real data by (*Server).projectNote once Issue #77
	// PR6's entry_files has anything to report; newNote itself never has
	// database access to look them up.
	FileIDs        []string    `json:"fileIds"`
	Files          []driveFile `json:"files"`
	VisibleUserIDs []string    `json:"visibleUserIds"`
	Mentions       []string    `json:"mentions"`
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
		URL:            e.ProvenanceURL,
		Visibility:     "public",
		LocalOnly:      false,
		RenoteCount:    0,
		RepliesCount:   0,
		Reactions:      map[string]int{},
		MyReaction:     nil,
		Emojis:         map[string]string{},
		FileIDs:        []string{},
		Files:          []driveFile{},
		VisibleUserIDs: []string{},
		Mentions:       []string{},
	}
}

// projectNote builds e's wire Note, including this deployment's real
// reaction data (Issue #23 PR4: note.Reactions/note.myReaction, both
// otherwise fixed at empty/nil by newNote). It is a Server method rather
// than folded into newNote itself so newNote stays a pure, repository-free
// conversion function noteapi_wire_test.go can exercise without a
// timeline service. viewerActorID is always the requesting owner (the
// only local actor RequireScope ever authenticates), used to resolve
// which reaction, if any, is "mine".
func (s *Server) projectNote(ctx context.Context, e domain.Entry, user userLite, viewerActorID string) (note, error) {
	n := newNote(e, user)
	counts, err := s.timeline.ReactionCounts(ctx, e.ID)
	if err != nil {
		return note{}, err
	}
	n.Reactions = counts
	my, err := s.timeline.MyReaction(ctx, e.ID, viewerActorID)
	if err != nil {
		return note{}, err
	}
	n.MyReaction = my

	files, err := s.timeline.AttachedFiles(ctx, e.ID)
	if err != nil {
		return note{}, err
	}
	if len(files) > 0 {
		fileIDs := make([]string, len(files))
		driveFiles := make([]driveFile, len(files))
		for i, f := range files {
			fileIDs[i] = f.ID
			driveFiles[i] = s.projectDriveFile(f)
		}
		n.FileIDs = fileIDs
		n.Files = driveFiles
	}

	// Issues #81 (citation footnotes) + #84 (chat title, viewer link)
	// enrichment: only an Open WebUI-generated reply can have a turn to
	// enrich from, and only when this deployment has one to look it up
	// with at all (s.openWebUITurnLinks is nil while OPENWEBUI_ENABLED
	// is off, the same guard virtualActors/openWebUIBridge already use).
	if e.Kind == domain.EntryLLMReply && s.openWebUITurnLinks != nil {
		text, err := s.enrichOpenWebUIReplyText(ctx, e)
		switch {
		case err == nil:
			n.Text = &text
		case errors.Is(err, domain.ErrNotFound):
			// An Issue #9 plain LLM reply: no Open WebUI turn exists for
			// this entry at all. n.Text stays wireText(e)'s output,
			// unchanged from before this enrichment existed.
		default:
			return note{}, err
		}
	}
	return n, nil
}

// enrichOpenWebUIReplyText extends wireText(e)'s "[reply]\n\n"+body
// output with Issue #84's own generated chat title — once
// s.openWebUITurnLinks reports one, it replaces the fixed "[reply]"
// marker itself — Issue #81's citation footnotes, and (only when
// OPENWEBUI_VIEWER_BASE_URL is configured) an owner-facing "view in Open
// WebUI" link (ADR-0005 D22, D23). Each addition is independent and
// degrades gracefully on its own: a turn with a title but no sources, or
// neither, still enriches whatever it has.
//
// Returns domain.ErrNotFound unchanged when e has no Open WebUI turn at
// all (an Issue #9 plain LLM reply) — the caller's job to fall back to
// wireText(e) for.
func (s *Server) enrichOpenWebUIReplyText(ctx context.Context, e domain.Entry) (string, error) {
	turn, err := s.openWebUITurnLinks.GetByAssistantEntry(ctx, e.ID)
	if err != nil {
		return "", err
	}
	text := wireText(e)
	if turn.RemoteChatTitle != nil && *turn.RemoteChatTitle != "" {
		text = "[reply] " + *turn.RemoteChatTitle + "\n\n" + e.Body
	}
	if len(turn.Sources) > 0 {
		text += "\n\n" + renderSourceFootnotes(turn.Sources)
	}
	if s.openWebUIViewerBaseURL != "" && turn.RemoteChatID != nil {
		text += "\n\n" + s.openWebUIViewerBaseURL + "/c/" + *turn.RemoteChatID
	}
	return text, nil
}

// renderSourceFootnotes renders Issue #81's normalized citations as a
// "[n] label (url)" block, one line per source, in array order —
// matching the "[n]" markers the model's own answer text embeds for the
// one case actually verified against a real instance (a single source).
// For more than one source this ordering is an explicit, documented
// assumption rather than a confirmed mapping — see domain.Source's own
// doc comment (ADR-0005 D22) for what a wrong assumption would look
// like here: a footnote's number disagreeing with the reply's own "[n]"
// text, never a data leak or a failed turn.
func renderSourceFootnotes(sources []domain.Source) string {
	lines := make([]string, len(sources))
	for i, src := range sources {
		label := src.DisplayName
		if label == "" {
			label = src.Kind
		}
		if src.URL != nil && *src.URL != "" {
			label += " (" + *src.URL + ")"
		}
		lines[i] = fmt.Sprintf("[%d] %s", i+1, label)
	}
	return strings.Join(lines, "\n")
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
func newUserLiteFromOwner(localOrigin string, owner miauth.OwnerProfile) userLite {
	return userLite{ID: owner.ActorID, Username: owner.Username, AvatarURL: avatarURLFromFileID(localOrigin, owner.AvatarFileID)}
}

// resolveUserLite builds the userLite projection for an entry's
// AuthorActorID. The common case (an entry authored by the requesting
// owner) is resolved from the already-fetched owner profile with no
// extra lookup; any other author (the reserved assistant/system
// presentation actors, or since Issue #52 an Open WebUI model actor —
// AGENTS.md forbids any other login-capable local user) falls back to
// one ResolveAuthor lookup to tell them apart, and finally to the actor
// ID itself as username if even that fails, so a wire projection is
// always produced rather than an internal error surfacing mid-response.
//
// An Open WebUI model actor additionally needs s.virtualActors to
// resolve successfully (its model active, its workspace enabled) before
// it gets the remote-looking projection; if either check fails — most
// commonly s.virtualActors being nil because OPENWEBUI_ENABLED is off —
// it falls through to the same actor-ID-as-username fallback every
// unresolvable author gets, never a host that names a disabled or
// nonexistent workspace.
func (s *Server) resolveUserLite(ctx context.Context, authorActorID string, owner miauth.OwnerProfile) userLite {
	if authorActorID == owner.ActorID {
		return newUserLiteFromOwner(s.localOrigin, owner)
	}
	if s.timeline != nil {
		if actor, err := s.timeline.ResolveAuthor(ctx, authorActorID); err == nil {
			switch actor.Type {
			case domain.ActorAssistant:
				return userLite{ID: authorActorID, Username: "assistant"}
			case domain.ActorSystem:
				return userLite{ID: authorActorID, Username: "system"}
			case domain.ActorOpenWebUIModel:
				if s.virtualActors != nil {
					if virtual, err := s.virtualActors.ResolveVirtualActor(ctx, authorActorID); err == nil {
						host := virtual.Host
						return userLite{ID: authorActorID, Username: virtual.Slug, Host: &host}
					}
				}
			case domain.ActorExternalSource:
				if s.externalSources != nil {
					if src, err := s.externalSources.GetByActorID(ctx, authorActorID); err == nil && src.Username != nil && src.Host != nil {
						host := *src.Host
						return userLite{
							ID: authorActorID, Username: *src.Username, Host: &host,
							// AvatarURL projects the source's fetched
							// favicon.ico (Issue #77 PR5) — actor.
							// AvatarFileID, not any field on src itself:
							// SetAvatarFileID writes straight to the
							// actors row, mirroring how the owner's own
							// avatar is stored.
							AvatarURL: avatarURLFromFileID(s.localOrigin, actor.AvatarFileID),
						}
					}
				}
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

// statsResponse is the Misskey-compatible projection POST /api/stats
// returns (Issue #23 PR2). The pinned misskey_dart StatsResponse parser
// treats every field as optional, so omitting a field this service has
// no concept for would decode identically to sending it as 0 — this
// service sends explicit values throughout, matching newNote's
// always-present-default convention for fields with no local concept
// (federation/drive), rather than omitting them.
type statsResponse struct {
	NotesCount         int `json:"notesCount"`
	OriginalNotesCount int `json:"originalNotesCount"`
	UsersCount         int `json:"usersCount"`
	OriginalUsersCount int `json:"originalUsersCount"`
	ReactionsCount     int `json:"reactionsCount"`
	Instances          int `json:"instances"`
	DriveUsageLocal    int `json:"driveUsageLocal"`
	DriveUsageRemote   int `json:"driveUsageRemote"`
}

// newStatsResponse builds statsResponse from notesCount (every entry
// this deployment has ever stored, regardless of author or archived/
// hidden state — see EntryRepository.CountAll) and reactionsCount (every
// reaction this deployment has ever stored, across every entry — see
// ReactionRepository.CountAll, added by Issue #23 PR4; previously always
// 0 before that repository existed). usersCount/originalUsersCount are
// always 1: the single owner is this deployment's only registered user,
// and there is no federation to tell local from remote users apart.
// instances/driveUsageLocal/driveUsageRemote are always 0: this service
// has no federation and no drive.
func newStatsResponse(notesCount, reactionsCount int) statsResponse {
	return statsResponse{
		NotesCount:         notesCount,
		OriginalNotesCount: notesCount,
		UsersCount:         1,
		OriginalUsersCount: 1,
		ReactionsCount:     reactionsCount,
	}
}

func newMeDetailed(localOrigin string, owner miauth.OwnerProfile, notesCount int) meDetailed {
	m := meDetailed{
		userDetailedNotMe: newUserDetailedNotMe(owner.ActorID, owner.Username, owner.DisplayName, owner.CreatedAt, notesCount),
		IsModerator:       true,
		IsAdmin:           true,
	}
	m.AvatarURL = avatarURLFromFileID(localOrigin, owner.AvatarFileID)
	return m
}
