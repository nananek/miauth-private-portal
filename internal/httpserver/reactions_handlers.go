package httpserver

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

// defaultReactionsListLimit is the paginated "who reacted" sheet's page
// size: docs/compat/aria-v1.5.11.md's traced call site always sends
// limit: 20. maxTimelineLimit (noteapi_handlers.go) bounds it the same
// way it bounds notes/timeline and notes/children.
const defaultReactionsListLimit = 20

// isCustomEmojiShortcode reports whether reaction is a Misskey custom-emoji
// shortcode (":name:" or ":name@host:") rather than a plain Unicode
// emoji. Custom emoji/drive are this deployment's Non-goals
// (docs/compat/aria-v1.5.11.md's "POST /api/notes/reactions/create"
// section), so a shortcode must be rejected with UNSUPPORTED_FEATURE the
// same way /api/notes/create rejects fields it does not support, never
// silently stored or misinterpreted as literal emoji text.
func isCustomEmojiShortcode(reaction string) bool {
	return len(reaction) >= 2 && strings.HasPrefix(reaction, ":") && strings.HasSuffix(reaction, ":")
}

type notesReactionsCreateRequest struct {
	NoteID   string  `json:"noteId"`
	Reaction *string `json:"reaction"`
}

// handleNotesReactionsCreate handles POST /api/notes/reactions/create
// (Issue #23 PR4). Per plan-issue-23 §1 PR4, the target note is not
// restricted to the owner's own posts: any visible entry, regardless of
// author (including assistant/system-authored llm_reply/news/mail
// entries), can be reacted to — only entryVisible gates it, matching
// Aria's own note_footer.dart, which places no isMe-style guard on
// reacting to your own or an assistant/system note (see
// docs/compat/aria-v1.5.11.md's trace).
func (s *Server) handleNotesReactionsCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesReactionsCreateRequest](r)
	if !ok || req.NoteID == "" {
		writeInvalidParam(w, "noteId is required")
		return
	}
	if req.Reaction == nil || *req.Reaction == "" {
		writeInvalidParam(w, "reaction is required")
		return
	}
	if isCustomEmojiShortcode(*req.Reaction) {
		writeUnsupportedFeature(w, "reaction")
		return
	}

	entry, err := s.timeline.GetEntry(r.Context(), req.NoteID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeNoSuchNote(w)
			return
		}
		s.logger.Error("get note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	if !entryVisible(entry) {
		writeNoSuchNote(w)
		return
	}

	if err := s.timeline.SetReaction(r.Context(), entry.ID, LocalActorIDFromContext(r.Context()), *req.Reaction); err != nil {
		s.logger.Error("create reaction failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	// Aria decodes any 2xx response here (docs/compat/aria-v1.5.11.md's
	// POST /api/notes/reactions/create section): no typed body needed.
	writeJSON(w, http.StatusOK, struct{}{})
}

type notesReactionsDeleteRequest struct {
	NoteID string `json:"noteId"`
}

// handleNotesReactionsDelete handles POST /api/notes/reactions/delete
// (Issue #23 PR4). Unlike handleNotesReactionsCreate, removing an absent
// reaction is not an error (RemoveReaction is idempotent): Aria's
// changeReaction flow calls this unconditionally before creating the new
// reaction (docs/compat/aria-v1.5.11.md), and there is no observed wire
// reason to distinguish "removed" from "was already absent" here.
func (s *Server) handleNotesReactionsDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesReactionsDeleteRequest](r)
	if !ok || req.NoteID == "" {
		writeInvalidParam(w, "noteId is required")
		return
	}

	entry, err := s.timeline.GetEntry(r.Context(), req.NoteID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeNoSuchNote(w)
			return
		}
		s.logger.Error("get note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	if !entryVisible(entry) {
		writeNoSuchNote(w)
		return
	}

	if err := s.timeline.RemoveReaction(r.Context(), entry.ID, LocalActorIDFromContext(r.Context())); err != nil {
		s.logger.Error("delete reaction failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

type notesReactionsRequest struct {
	NoteID  string  `json:"noteId"`
	Type    *string `json:"type"`
	Limit   *int    `json:"limit"`
	UntilID *string `json:"untilId"`
}

// reactionUser is the "who reacted" list's per-row projection
// (docs/compat/aria-v1.5.11.md's "POST /api/notes/reactions" section):
// id/createdAt/user are required, type is nullable in the pinned parser
// but always populated here since this service always knows the emoji.
type reactionUser struct {
	ID        string   `json:"id"`
	CreatedAt string   `json:"createdAt"`
	User      userLite `json:"user"`
	Type      *string  `json:"type"`
}

// handleNotesReactions handles POST /api/notes/reactions — the paginated
// "who reacted" sheet (Issue #23 PR4). docs/compat/aria-v1.5.11.md
// corrects plan-issue-23's assumed wire path: this is not
// "/api/notes/reactions/list". type filters to one specific emoji when
// present (Aria's sheet is per-reaction-emoji); when absent, every
// reaction on the note is returned regardless of emoji, matching real
// Misskey's own optional filter semantics.
func (s *Server) handleNotesReactions(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesReactionsRequest](r)
	if !ok || req.NoteID == "" {
		writeInvalidParam(w, "noteId is required")
		return
	}

	entry, err := s.timeline.GetEntry(r.Context(), req.NoteID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeNoSuchNote(w)
			return
		}
		s.logger.Error("get note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	if !entryVisible(entry) {
		writeNoSuchNote(w)
		return
	}

	limit := defaultReactionsListLimit
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
	}
	if limit > maxTimelineLimit {
		limit = maxTimelineLimit
	}

	var before *domain.Cursor
	if req.UntilID != nil && *req.UntilID != "" {
		anchor, err := s.timeline.GetReaction(r.Context(), *req.UntilID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// Mirrors handleNotesTimeline's unknown-untilId handling: a
				// stale/unknown anchor has nothing older to page to, so an
				// empty page is the pagination-loop-safe response.
				writeJSON(w, http.StatusOK, []reactionUser{})
				return
			}
			s.logger.Error("resolve reactions untilId failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		before = &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}
	}

	reactions, err := s.timeline.ListReactions(r.Context(), entry.ID, req.Type, before, limit)
	if err != nil {
		s.logger.Error("list reactions failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	out := make([]reactionUser, 0, len(reactions))
	for _, react := range reactions {
		emoji := react.Emoji
		out = append(out, reactionUser{
			ID:        react.ID,
			CreatedAt: react.CreatedAt.UTC().Format(time.RFC3339),
			User:      s.resolveUserLite(r.Context(), react.ReactorActorID, owner),
			Type:      &emoji,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
