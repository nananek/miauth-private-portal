package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// handleMiAuthStart handles GET /miauth/{session}, Aria's entry point
// for adding an account. It creates or resumes an unapproved local
// session for an operator to inspect and approve through SSH. The
// response is an interactive browser flow, not parsed by Aria as JSON
// (docs/compat/aria-v1.5.11.md), so failures render a minimal plain-text
// page rather than a Misskey-compatible error body.
func (s *Server) handleMiAuthStart(w http.ResponseWriter, r *http.Request) {
	routeSessionID := r.PathValue("session")
	permission := r.URL.Query().Get("permission")

	var clientCallback *string
	if cb := r.URL.Query().Get("callback"); cb != "" {
		clientCallback = &cb
	}

	err := s.miauth.StartLocalSession(r.Context(), routeSessionID, permission, clientCallback)
	if err != nil {
		// ErrClientCallbackNotAllowed and ErrSessionUnavailable are both
		// rendered identically: neither reveals which case applies, and
		// there is nothing sensitive in either to protect beyond that.
		if !errors.Is(err, miauth.ErrClientCallbackNotAllowed) && !errors.Is(err, miauth.ErrSessionUnavailable) {
			s.logger.Error("miauth start failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		}
		writePlainTextPage(w, http.StatusBadRequest, "This sign-in request cannot be started.")
		return
	}

	if clientCallback != nil {
		redirectURL, err := clientCallbackURL(*clientCallback, routeSessionID)
		if err != nil {
			// The configured callback was exact-match validated at startup.
			// Avoid logging its raw value here in case an operator included
			// sensitive query data in it despite that recommendation.
			s.logger.Error("miauth client callback construction failed", "request_id", logging.RequestIDFromContext(r.Context()))
			writePlainTextPage(w, http.StatusInternalServerError, "This sign-in request cannot be started.")
			return
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}
	writePlainTextPage(w, http.StatusOK, "Waiting for the operator to approve this sign-in via SSH. You can close this page; Aria will continue automatically once approved.")
}

func clientCallbackURL(callback, routeSessionID string) (string, error) {
	u, err := url.Parse(callback)
	if err != nil {
		return "", err
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", err
	}
	query.Set("session", routeSessionID)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// handleMiAuthCheck handles POST /api/miauth/{session}/check.
//
// Every non-success outcome (not found, still pending, denied, expired,
// already consumed/replayed) responds identically with 200
// {"ok":false}, deliberately not 4xx: docs/compat/aria-v1.5.11.md
// leaves the pending response's exact status as 要実機確認
// (needs real-instance verification), and the pinned Aria client's
// direct Dio call for this endpoint is not confirmed to survive a
// non-2xx before attempting to parse JSON — 200 is the safe choice for
// a value Aria polls repeatedly while the owner is still completing the
// browser flow.
func (s *Server) handleMiAuthCheck(w http.ResponseWriter, r *http.Request) {
	routeSessionID := r.PathValue("session")

	result, err := s.miauth.Check(r.Context(), routeSessionID)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		if !errors.Is(err, miauth.ErrCheckNotReady) {
			s.logger.Error("miauth check failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		}
		_ = json.NewEncoder(w).Encode(checkFailureResponse{OK: false})
		return
	}
	_ = json.NewEncoder(w).Encode(checkSuccessResponse{
		OK:    true,
		Token: result.Token,
		User: newUserDetailedNotMe(
			result.OwnerActorID, result.OwnerUsername, result.OwnerDisplayName, result.OwnerCreatedAt,
			s.notesCountForOwner(r.Context(), result.OwnerActorID),
		),
	})
}

// notesCountForOwner returns the owner's total entry count for a
// notesCount projection, or 0 if this Server has no TimelineService
// wired (every httpserver test predating Issue #7) or the count lookup
// fails. A failure here is best-effort bookkeeping, not authentication or
// authorization, so it must never turn an otherwise successful response
// into an error.
func (s *Server) notesCountForOwner(ctx context.Context, ownerActorID string) int {
	if s.timeline == nil {
		return 0
	}
	n, err := s.timeline.CountByAuthor(ctx, ownerActorID)
	if err != nil {
		s.logger.Error("count owner entries failed", "request_id", logging.RequestIDFromContext(ctx), "error", err.Error())
		return 0
	}
	return n
}

func writePlainTextPage(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
