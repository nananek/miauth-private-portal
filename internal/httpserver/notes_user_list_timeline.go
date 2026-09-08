package httpserver

import (
	"errors"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

// notesUserListTimelineRequest mirrors misskey_dart's
// UserListTimelineRequest (plan-115 §1's traced field table). Only
// ListID, Limit, and UntilID have any effect, mirroring
// notesTimelineRequest's own SinceID/SinceDate/UntilDate/WithRenotes/
// WithFiles/AllowPartial handling: no traced Aria call site
// (lib/view/page/list/list_page.dart) sends the others, so they are
// accepted and ignored rather than rejecting an otherwise-valid request.
type notesUserListTimelineRequest struct {
	ListID       string  `json:"listId"`
	Limit        *int    `json:"limit"`
	UntilID      *string `json:"untilId"`
	SinceID      *string `json:"sinceId"`
	WithRenotes  *bool   `json:"withRenotes"`
	WithFiles    *bool   `json:"withFiles"`
	AllowPartial *bool   `json:"allowPartial"`
}

// handleNotesUserListTimeline handles POST /api/notes/user-list-timeline
// (Issue #115 PR3): the same newest-first, untilId-paginated contract
// handleNotesTimeline gives the home timeline, restricted to entries
// authored by listId's current members (userlist.Service.MemberActorIDs,
// via timeline.Service.GetTimelineByAuthorsDesc — Issue #114's
// ListByAuthorsDesc generalization). A list with zero members returns an
// empty page rather than erroring or falling back to the full timeline.
func (s *Server) handleNotesUserListTimeline(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesUserListTimelineRequest](r)
	if !ok || req.ListID == "" {
		writeInvalidParam(w, "listId is required")
		return
	}

	if _, err := s.userLists.Get(r.Context(), req.ListID); err != nil {
		s.writeUserListError(w, r, "get user list for timeline", err)
		return
	}
	memberActorIDs, err := s.userLists.MemberActorIDs(r.Context(), req.ListID)
	if err != nil {
		s.writeUserListError(w, r, "list user list members for timeline", err)
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
				// Mirrors handleNotesTimeline's own stale/unknown untilId
				// handling: an empty page, not an error, so a paginating
				// client stops rather than looping.
				writeJSON(w, http.StatusOK, []note{})
				return
			}
			s.logger.Error("resolve user list timeline untilId failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		before = &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}
	}

	entries, err := s.timeline.GetTimelineByAuthorsDesc(r.Context(), memberActorIDs, before, limit, false)
	if err != nil {
		s.logger.Error("get user list timeline failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	notes := make([]note, 0, len(entries))
	for _, e := range entries {
		n, err := s.projectNote(r.Context(), e, s.resolveUserLite(r.Context(), e.AuthorActorID, owner), owner.ActorID)
		if err != nil {
			s.logger.Error("project user list timeline note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		notes = append(notes, n)
	}
	writeJSON(w, http.StatusOK, notes)
}
