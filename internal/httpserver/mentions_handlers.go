package httpserver

import (
	"errors"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

// notesMentionsRequest is POST /api/notes/mentions' request shape
// (docs/compat/aria-v1.5.11.md's "POST /api/notes/mentions" section).
// sinceId/sinceDate/untilDate/following are accepted-and-ignored the
// same way handleNotesTimeline ignores them: this service has no
// date-range or "following" concept to honor them with. Visibility is
// the one field this endpoint reads beyond limit/untilId, to special-
// case Aria's "Direct" tab (see handleNotesMentions).
type notesMentionsRequest struct {
	Limit      *int    `json:"limit"`
	UntilID    *string `json:"untilId"`
	Visibility *string `json:"visibility"`
}

// handleNotesMentions handles POST /api/notes/mentions (Issue #23 PR5):
// Aria's optional home-timeline "Mention" and "Direct" tabs. single-owner
// means this can only ever surface the owner's own user_post entries
// that contain "@" + their own username (see
// timeline.Service.recordSelfMentionIfAny) — there is no other
// login-capable local actor to be mentioned by. The "Direct" tab sends
// visibility: "specified"; since this service has no per-note
// visibility concept beyond "public" (every note it ever creates is
// "public" — see the wire note type), that variant always resolves to
// an empty page here without running the underlying query
// (docs/compat/aria-v1.5.11.md). Pagination otherwise mirrors
// handleNotesTimeline: untilId resolves through GetEntry the same way,
// newest-first, limit defaults/clamps the same way. No new scope is
// needed: read:notes already covers it.
func (s *Server) handleNotesMentions(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesMentionsRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}

	if req.Visibility != nil && *req.Visibility == "specified" {
		writeJSON(w, http.StatusOK, []note{})
		return
	}

	limit := defaultTimelineLimit
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
	}
	if limit > maxTimelineLimit {
		limit = maxTimelineLimit
	}

	var before *domain.Cursor
	if req.UntilID != nil && *req.UntilID != "" {
		anchor, err := s.timeline.GetEntry(r.Context(), *req.UntilID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// Mirrors handleNotesTimeline's unknown-untilId handling:
				// a stale/unknown anchor has nothing older to page to.
				writeJSON(w, http.StatusOK, []note{})
				return
			}
			s.logger.Error("resolve mentions untilId failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		before = &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}
	}

	actorID := LocalActorIDFromContext(r.Context())
	entries, err := s.timeline.ListMentions(r.Context(), actorID, before, limit)
	if err != nil {
		s.logger.Error("list mentions failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), actorID)
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	notes := make([]note, 0, len(entries))
	for _, e := range entries {
		n, err := s.projectNote(r.Context(), e, s.resolveUserLite(r.Context(), e.AuthorActorID, owner), owner.ActorID)
		if err != nil {
			s.logger.Error("project mention note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		notes = append(notes, n)
	}
	writeJSON(w, http.StatusOK, notes)
}
