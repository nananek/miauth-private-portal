package httpserver

import (
	"errors"
	"net/http"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

// defaultNotificationsLimit is POST /api/i/notifications' page size:
// docs/compat/aria-v1.5.11.md's traced call site always sends limit: 20,
// the same value handleNotesReactions uses for its own paginated sheet.
// maxTimelineLimit (noteapi_handlers.go) bounds it the same way it bounds
// notes/timeline, notes/children, and notes/reactions.
const defaultNotificationsLimit = 20

// notificationsRequest is POST /api/i/notifications' request shape
// (docs/compat/aria-v1.5.11.md's "POST /api/i/notifications" section).
// sinceId/sinceDate/untilDate/following/unreadOnly/markAsRead/
// includeTypes/excludeTypes all exist in the pinned request model, but
// Aria's traced call site never sets any of them, so this service does
// not accept them (there is nothing to ignore-and-accept when the field
// is never sent in practice).
type notificationsRequest struct {
	Limit   *int    `json:"limit"`
	UntilID *string `json:"untilId"`
}

// notificationResponse is one INotificationsResponse element
// (docs/compat/aria-v1.5.11.md). Only the fields this deployment's two
// notification types actually need are populated: id/createdAt/type
// always, note for "reply" (rendered by notification_widget.dart via
// NoteWidget(noteId: note.id)), and body for "app" (rendered as free
// text; header is left unset — see (*Server).newNotificationResponse's
// doc comment for why). Every other INotificationsResponse field
// (reaction, achievement, role, ...) belongs to notification types this
// service never emits, so they are omitted rather than sent as
// always-null padding — unlike note's always-present-default convention,
// which matches how real Misskey renders every Note regardless of kind,
// real Misskey's own notification wire format already varies fields by
// type, so omitting irrelevant ones here matches that behavior rather
// than diverging from it.
type notificationResponse struct {
	ID        string  `json:"id"`
	CreatedAt string  `json:"createdAt"`
	Type      string  `json:"type"`
	Note      *note   `json:"note,omitempty"`
	Body      *string `json:"body,omitempty"`
}

// newNotificationResponse projects n onto the wire type, given its
// already-visibility-checked related entry and (for a "reply"
// notification) the already-resolved wire Note. relatedNote is nil for a
// NotificationApp notification. For NotificationApp, Body reuses the
// related entry's Body verbatim rather than splitting out a separate
// Header: internal/ingest.Service's composeExternalBody has already
// folded the source kind/title/provenance URL into Body's own leading
// lines (see docs/compat/aria-v1.5.11.md's "Note.text provenance
// markers" section), so Body alone already carries the same information
// a synthesized Header would duplicate.
func newNotificationResponse(n domain.Notification, relatedNote *note, relatedEntry domain.Entry) notificationResponse {
	resp := notificationResponse{
		ID:        n.ID,
		CreatedAt: n.CreatedAt.UTC().Format(time.RFC3339),
		Type:      string(n.Type),
	}
	switch n.Type {
	case domain.NotificationReply:
		resp.Note = relatedNote
	case domain.NotificationApp:
		body := relatedEntry.Body
		resp.Body = &body
	}
	return resp
}

// handleAPINotifications handles POST /api/i/notifications (Issue #23
// PR6): the Notifications tab, always reachable in Aria's navigation.
// Aria's grouped-notifications setting has no effect here (this
// service's implementedEndpoints never advertises "i/notifications-
// grouped"), so it always falls back to this plain call. Pagination
// mirrors handleNotesReactions: untilId resolves through the
// notification's own (created_at, id) via GetNotification, newest-first,
// limit defaults/clamps the same way.
func (s *Server) handleAPINotifications(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notificationsRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}

	limit := defaultNotificationsLimit
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
	}
	if limit > maxTimelineLimit {
		limit = maxTimelineLimit
	}

	var before *domain.Cursor
	if req.UntilID != nil && *req.UntilID != "" {
		anchor, err := s.timeline.GetNotification(r.Context(), *req.UntilID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// Mirrors handleNotesReactions' unknown-untilId handling:
				// a stale/unknown anchor has nothing older to page to.
				writeJSON(w, http.StatusOK, []notificationResponse{})
				return
			}
			s.logger.Error("resolve notifications untilId failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		before = &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}
	}

	notifications, err := s.timeline.ListNotifications(r.Context(), before, limit)
	if err != nil {
		s.logger.Error("list notifications failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	out := make([]notificationResponse, 0, len(notifications))
	for _, n := range notifications {
		entry, err := s.timeline.GetEntry(r.Context(), n.RelatedEntryID)
		if err != nil {
			s.logger.Error("get notification related entry failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}

		var relatedNote *note
		if n.Type == domain.NotificationReply {
			projected, err := s.projectNote(r.Context(), entry, s.resolveUserLite(r.Context(), entry.AuthorActorID, owner), owner.ActorID)
			if err != nil {
				s.logger.Error("project notification note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
				writeInternalError(w)
				return
			}
			relatedNote = &projected
		}

		out = append(out, newNotificationResponse(n, relatedNote, entry))
	}
	writeJSON(w, http.StatusOK, out)
}
