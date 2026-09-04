package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
)

const (
	// miauthServiceName is the `name` this service presents to the
	// upstream Misskey MiAuth page as the requesting application.
	miauthServiceName = "miauth-private-portal"
	// upstreamMinimalPermission is this service's own minimal
	// server-side scope for owner verification against IDENTITY_ORIGIN.
	// ADR-0001 §4 forbids forwarding Aria's broad requested permission
	// list to the upstream identity provider unchanged; this is that
	// separate, minimal allowlist. read:account is enough to identify
	// the verified user.
	upstreamMinimalPermission = "read:account"
)

// handleMiAuthStart handles GET /miauth/{session}, Aria's entry point
// for adding an account. It starts or idempotently resumes the local
// session and its linked upstream verification session, then redirects
// the browser to the fixed IDENTITY_ORIGIN MiAuth page. The response is
// an interactive browser flow, not parsed by Aria as JSON
// (docs/compat/aria-v1.5.11.md), so failures render a minimal plain-text
// page rather than a Misskey-compatible error body.
func (s *Server) handleMiAuthStart(w http.ResponseWriter, r *http.Request) {
	routeSessionID := r.PathValue("session")
	permission := r.URL.Query().Get("permission")

	var clientCallback *string
	if cb := r.URL.Query().Get("callback"); cb != "" {
		clientCallback = &cb
	}

	started, err := s.miauth.StartLocalSession(r.Context(), routeSessionID, permission, clientCallback)
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

	http.Redirect(w, r, s.upstreamMiAuthURL(started), http.StatusFound)
}

// handleMiAuthBootstrapStart handles GET /miauth/bootstrap/{gate}, the
// operator-only entry point reached only by someone who already
// possesses a gate value printed by cmd/bootstrapctl. It is otherwise
// identical to handleMiAuthStart's redirect construction.
func (s *Server) handleMiAuthBootstrapStart(w http.ResponseWriter, r *http.Request) {
	gateID := r.PathValue("gate")

	started, err := s.miauth.StartBootstrapSession(r.Context(), gateID)
	if err != nil {
		// A generic 404 either way: an already-bound deployment, an
		// unknown gate, an expired gate, and an already-consumed gate
		// are all indistinguishable to whoever is probing this URL.
		if !errors.Is(err, miauth.ErrBootstrapUnavailable) {
			s.logger.Error("miauth bootstrap start failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		}
		writePlainTextPage(w, http.StatusNotFound, "Not found.")
		return
	}

	http.Redirect(w, r, s.upstreamMiAuthURL(started), http.StatusFound)
}

// upstreamMiAuthURL builds the fixed IDENTITY_ORIGIN MiAuth URL for
// started, shared by the ordinary and bootstrap flows. The internal
// callback embeds both the upstream session's id and its crypto/rand
// state as its own query parameters — Misskey has no notion of this
// service's own state value, so it must be carried in the callback URL
// this service controls, not derived from anything Misskey adds. Misskey
// treats the callback URL as opaque and appends its own `session=`
// parameter when redirecting back without stripping the ones already
// present; handleMiAuthCallback reads id/state from what this service
// itself embedded and ignores that echoed value.
func (s *Server) upstreamMiAuthURL(started miauth.StartedSession) string {
	callback := s.localOrigin + "/miauth/callback?" + url.Values{
		"id":    {started.UpstreamSessionID},
		"state": {started.UpstreamState},
	}.Encode()

	v := url.Values{}
	v.Set("name", miauthServiceName)
	v.Set("permission", upstreamMinimalPermission)
	v.Set("callback", callback)
	return fmt.Sprintf("%s/miauth/%s?%s", s.identityOrigin, url.PathEscape(started.UpstreamSessionID), v.Encode())
}

// handleMiAuthCallback handles the fixed internal GET /miauth/callback
// endpoint shared by both the ordinary Aria-triggered flow and the
// operator bootstrap flow. It never reveals why an attempt failed
// (ADR-0001: unauthorized users get a generic denial that never leaks
// the allowlisted ID or token information).
func (s *Server) handleMiAuthCallback(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	state := r.URL.Query().Get("state")

	result, err := s.miauth.HandleUpstreamCallback(r.Context(), id, state)
	if err != nil {
		if !errors.Is(err, miauth.ErrCallbackInvalid) && !errors.Is(err, miauth.ErrUpstreamVerification) && !errors.Is(err, miauth.ErrOwnerBindingDenied) {
			s.logger.Error("miauth callback failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		}
		writePlainTextPage(w, http.StatusBadRequest, "Authentication failed. Please try again from Aria.")
		return
	}

	if result.ClientCallback != nil {
		redirectURL, err := clientCallbackURL(*result.ClientCallback, result.RouteSessionID)
		if err != nil {
			// The configured callback was exact-match validated at startup.
			// Avoid logging its raw value here in case an operator included
			// sensitive query data in it despite that recommendation.
			s.logger.Error("miauth client callback construction failed", "request_id", logging.RequestIDFromContext(r.Context()))
			writePlainTextPage(w, http.StatusInternalServerError, "Authentication failed. Please try again from Aria.")
			return
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}
	writePlainTextPage(w, http.StatusOK, "Success. You can return to Aria.")
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
