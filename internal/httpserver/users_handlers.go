package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

// searchCandidates enumerates every actor users/search and
// users/search-by-username-and-host may return: the owner, the two
// reserved presentation actors, and (Issue #52) every currently
// resolvable Open WebUI model actor. This is the same known-actor set
// resolveUserLite already reprojects per entry-author; here it is
// gathered up front as a list to search over instead.
//
// An Open WebUI model actor whose VirtualActorResolver.ResolveVirtualActor
// call fails (an inactive model, a disabled workspace, or
// OPENWEBUI_ENABLED off — s.virtualActors is nil) is left out entirely,
// unlike resolveUserLite's own per-entry fallback to an actor-ID
// projection: a search result naming an actor by its opaque ID would
// not be a meaningful hit.
func (s *Server) searchCandidates(ctx context.Context, viewerActorID string) ([]searchCandidate, error) {
	owner, err := s.miauth.DescribeOwner(ctx, viewerActorID)
	if err != nil {
		return nil, fmt.Errorf("describe owner: %w", err)
	}
	candidates := []searchCandidate{{
		ActorID:     owner.ActorID,
		Username:    owner.Username,
		DisplayName: owner.DisplayName,
		CreatedAt:   owner.CreatedAt,
	}}

	for _, reserved := range []struct {
		actorType domain.ActorType
		username  string
	}{
		{domain.ActorAssistant, "assistant"},
		{domain.ActorSystem, "system"},
	} {
		actor, err := s.timeline.GetActorByType(ctx, reserved.actorType)
		if err != nil {
			return nil, fmt.Errorf("get %s actor: %w", reserved.actorType, err)
		}
		candidates = append(candidates, searchCandidate{
			ActorID:     actor.ID,
			Username:    reserved.username,
			DisplayName: displayNameOrEmpty(actor.DisplayName),
			CreatedAt:   actor.CreatedAt,
		})
	}

	if s.virtualActors != nil {
		models, err := s.timeline.ListActorsByType(ctx, domain.ActorOpenWebUIModel)
		if err != nil {
			return nil, fmt.Errorf("list openwebui model actors: %w", err)
		}
		for _, actor := range models {
			virtual, err := s.virtualActors.ResolveVirtualActor(ctx, actor.ID)
			if err != nil {
				continue
			}
			host := virtual.Host
			candidates = append(candidates, searchCandidate{
				ActorID:     actor.ID,
				Username:    virtual.Slug,
				Host:        &host,
				DisplayName: virtual.DisplayName,
				CreatedAt:   actor.CreatedAt,
			})
		}
	}

	return candidates, nil
}

// displayNameOrEmpty reads Actor.DisplayName's nil-until-set pointer as
// a plain string, mirroring internal/miauth's own unexported helper of
// the same name (DescribeOwner's OwnerProfile.DisplayName is already
// this shape by the time it reaches httpserver).
func displayNameOrEmpty(displayName *string) string {
	if displayName == nil {
		return ""
	}
	return *displayName
}

// notesCountForActor is notesCountForOwner generalized to any actor:
// fail-soft to 0 (logged) rather than failing the whole search request
// over one candidate's count.
func (s *Server) notesCountForActor(ctx context.Context, actorID string) int {
	n, err := s.timeline.CountByAuthor(ctx, actorID)
	if err != nil {
		s.logger.Error("count actor entries failed", "request_id", logging.RequestIDFromContext(ctx), "actor_id", actorID, "error", err.Error())
		return 0
	}
	return n
}

// projectSearchCandidate builds c's wire userDetailedNotMe projection.
// Every field newUserDetailedNotMe does not set defaults to its
// honest-false/nil zero value (see that constructor's own doc comment),
// which for Url means the polymorphic-decode discriminator key is
// present exactly as it is for /api/i and the MiAuth check response.
func (s *Server) projectSearchCandidate(ctx context.Context, c searchCandidate) userDetailedNotMe {
	u := newUserDetailedNotMe(c.ActorID, c.Username, c.DisplayName, c.CreatedAt, s.notesCountForActor(ctx, c.ActorID))
	u.Host = c.Host
	return u
}

// handleUsersSearch handles POST /api/users/search (Issue #65): a
// query-based lookup over this deployment's known actor set. This
// implements only the search itself — starting an actual Open WebUI
// Chat conversation from a selected user is out of scope (README.md's
// "Known limitations"; this PR does not touch /api/chat/*).
func (s *Server) handleUsersSearch(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersSearchRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}
	if req.Query == "" {
		writeInvalidParam(w, "query is required")
		return
	}

	candidates, err := s.searchCandidates(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("enumerate search candidates failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	var matched []searchCandidate
	for _, c := range candidates {
		if matchesSearchQuery(c, req.Query) && matchesOrigin(c, req.Origin) {
			matched = append(matched, c)
		}
	}

	offset := 0
	if req.Offset != nil && *req.Offset > 0 {
		offset = *req.Offset
	}
	page := paginateCandidates(matched, offset, clampSearchLimit(req.Limit))

	users := make([]userDetailedNotMe, len(page))
	for i, c := range page {
		users[i] = s.projectSearchCandidate(r.Context(), c)
	}
	writeJSON(w, http.StatusOK, users)
}

// handleUsersSearchByUsernameAndHost handles POST
// /api/users/search-by-username-and-host (Issue #65): an exact
// username(+host) lookup over the same known actor set handleUsersSearch
// searches by substring.
func (s *Server) handleUsersSearchByUsernameAndHost(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersSearchByUsernameAndHostRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}

	candidates, err := s.searchCandidates(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("enumerate search candidates failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	var matched []searchCandidate
	for _, c := range candidates {
		if matchesUsernameAndHost(c, req.Username, req.Host) {
			matched = append(matched, c)
		}
	}
	if limit := clampSearchLimit(req.Limit); len(matched) > limit {
		matched = matched[:limit]
	}

	users := make([]userDetailedNotMe, len(matched))
	for i, c := range matched {
		users[i] = s.projectSearchCandidate(r.Context(), c)
	}
	writeJSON(w, http.StatusOK, users)
}

// handleUsersShow handles POST /api/users/show (Issue #114): a userId or
// username(+host) lookup over the same known actor set users/search
// already enumerates, reusing searchCandidates/matchesUsernameAndHost/
// projectSearchCandidate unchanged — no new projection logic, per this
// issue's plan §0. A userId or username match returns userDetailedNotMe
// directly (not wrapped in an array), matching UsersShowRequest/
// UsersShowByUserNameRequest's single-object response; an unmatched
// lookup returns the same NO_SUCH_USER denial as users/notes below
// rather than fabricating success (AGENTS.md).
//
// UsersShowByIdsRequest{userIds} is deliberately not implemented: PR0's
// trace of Aria's user_page.dart (docs/compat/aria-v1.5.11.md) found no
// call site in the profile-page flow that uses it, and inventing a batch
// contract nothing traces would be exactly the kind of guess AGENTS.md's
// "do not silently invent a protocol" rules out. A request that only
// carries userIds fails closed with an explicit UNSUPPORTED_FEATURE
// instead of a fabricated empty/partial result.
func (s *Server) handleUsersShow(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersShowRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}

	candidates, err := s.searchCandidates(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("enumerate search candidates failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	switch {
	case req.UserID != nil && *req.UserID != "":
		for _, c := range candidates {
			if c.ActorID == *req.UserID {
				writeJSON(w, http.StatusOK, s.projectSearchCandidate(r.Context(), c))
				return
			}
		}
		writeNoSuchUser(w)
	case req.Username != nil:
		for _, c := range candidates {
			if matchesUsernameAndHost(c, req.Username, req.Host) {
				writeJSON(w, http.StatusOK, s.projectSearchCandidate(r.Context(), c))
				return
			}
		}
		writeNoSuchUser(w)
	case len(req.UserIDs) > 0:
		writeUnsupportedFeature(w, "userIds")
	default:
		writeInvalidParam(w, "userId, userIds, or username is required")
	}
}

// handleUsersNotes handles POST /api/users/notes (Issue #114): the
// requested actor's own notes, newest-first, with the same untilId
// cursor-pagination contract handleNotesTimeline already gives the home
// timeline. userId must name one of the same known actors
// searchCandidates enumerates — an id outside that set gets the same
// NO_SUCH_USER denial as an unmatched users/show lookup, never a
// fabricated empty page (AGENTS.md), so a caller cannot tell "this actor
// has no notes yet" apart from "no such actor" by checking status alone
// on this endpoint, only on notes/timeline's own untilId handling below.
func (s *Server) handleUsersNotes(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersNotesRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}
	if req.UserID == "" {
		writeInvalidParam(w, "userId is required")
		return
	}

	candidates, err := s.searchCandidates(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("enumerate search candidates failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	known := false
	for _, c := range candidates {
		if c.ActorID == req.UserID {
			known = true
			break
		}
	}
	if !known {
		writeNoSuchUser(w)
		return
	}

	limit := defaultTimelineLimit
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
	}
	if limit > maxTimelineLimit {
		limit = maxTimelineLimit
	}

	// untilId resolution mirrors handleNotesTimeline's own: a stale/
	// unknown id has nothing older to page to, so an empty page is the
	// pagination-loop-safe response (Aria stops paging on an empty
	// result), not an error — distinct from userId itself being unknown
	// above, which is checked first and always denied.
	var before *domain.Cursor
	if req.UntilID != nil && *req.UntilID != "" {
		anchor, err := s.timeline.GetEntry(r.Context(), *req.UntilID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				writeJSON(w, http.StatusOK, []note{})
				return
			}
			s.logger.Error("resolve users/notes untilId failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		before = &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}
	}

	entries, err := s.timeline.GetEntriesByAuthorDesc(r.Context(), req.UserID, before, limit, false)
	if err != nil {
		s.logger.Error("get user notes failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
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
			s.logger.Error("project user note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		notes = append(notes, n)
	}
	writeJSON(w, http.StatusOK, notes)
}
