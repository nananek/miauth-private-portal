package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
)

// writeNoSuchList follows the same invented-but-Misskey-flavored
// convention writeNoSuchNote/writeNoSuchFile/writeNoSuchUser already
// establish (noteapi_errors.go's wireError doc comment) — not a literal
// real-Misskey error ID. writeNoSuchUser itself (used by
// handleUsersListsPush below) is defined once, in noteapi_errors.go —
// Issue #114's users/show and users/notes needed the same denial first.

func writeNoSuchList(w http.ResponseWriter) {
	writeWireError(w, http.StatusBadRequest, "no-such-list", "NO_SUCH_LIST", "No such list.", "client", nil)
}

// writeUserListError maps an internal/userlist.Service error to a wire
// response, logging (and generalizing to a 500) anything else — the same
// op-scoped shape writeDriveFolderError already establishes.
// domain.ErrNotFound is the only sentinel any userlist.Service method
// returns (a listID that does not exist), so there is only one case to
// special-case here.
func (s *Server) writeUserListError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		writeNoSuchList(w)
		return
	}
	s.logger.Error(op+" failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
	writeInternalError(w)
}

// isKnownActor reports whether actorID names one of this deployment's
// known actors (searchCandidates' own owner/assistant/system/Open WebUI
// model enumeration — users_handlers.go, Issue #65). POST
// /api/users/lists/push uses it to reject an unknown actorID explicitly
// (plan-115 §2.2) rather than letting a foreign-key violation surface as
// a generic 500.
func (s *Server) isKnownActor(ctx context.Context, actorID string) (bool, error) {
	candidates, err := s.searchCandidates(ctx, LocalActorIDFromContext(ctx))
	if err != nil {
		return false, err
	}
	for _, c := range candidates {
		if c.ActorID == actorID {
			return true, nil
		}
	}
	return false, nil
}

// projectUserListResponse re-fetches l.ID's current membership and
// projects it alongside l — the shared final step every users/lists/*
// handler below that returns a single list takes. On a membership-lookup
// failure it writes the error response itself and returns ok=false, so
// callers can just `return` without duplicating error handling.
func (s *Server) projectUserListResponse(w http.ResponseWriter, r *http.Request, op string, l domain.UserList) (userListResponse, bool) {
	memberIDs, err := s.userLists.MemberActorIDs(r.Context(), l.ID)
	if err != nil {
		s.writeUserListError(w, r, op, err)
		return userListResponse{}, false
	}
	return projectUserList(l, memberIDs), true
}

// handleUsersListsCreate handles POST /api/users/lists/create.
func (s *Server) handleUsersListsCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersListsCreateRequest](r)
	if !ok || req.Name == "" {
		writeInvalidParam(w, "name is required")
		return
	}
	l, err := s.userLists.Create(r.Context(), req.Name)
	if err != nil {
		s.writeUserListError(w, r, "create user list", err)
		return
	}
	resp, ok := s.projectUserListResponse(w, r, "create user list", l)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsersListsList handles POST /api/users/lists/list: every list
// this (single-owner) deployment has created, in
// userlist.Service.ListAll's stable order.
func (s *Server) handleUsersListsList(w http.ResponseWriter, r *http.Request) {
	lists, err := s.userLists.ListAll(r.Context())
	if err != nil {
		s.writeUserListError(w, r, "list user lists", err)
		return
	}
	out := make([]userListResponse, 0, len(lists))
	for _, l := range lists {
		resp, ok := s.projectUserListResponse(w, r, "list user lists", l)
		if !ok {
			return
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUsersListsShow handles POST /api/users/lists/show.
func (s *Server) handleUsersListsShow(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersListsShowRequest](r)
	if !ok || req.ListID == "" {
		writeInvalidParam(w, "listId is required")
		return
	}
	l, err := s.userLists.Get(r.Context(), req.ListID)
	if err != nil {
		s.writeUserListError(w, r, "show user list", err)
		return
	}
	resp, ok := s.projectUserListResponse(w, r, "show user list", l)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsersListsUpdate handles POST /api/users/lists/update.
func (s *Server) handleUsersListsUpdate(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersListsUpdateRequest](r)
	if !ok || req.ListID == "" {
		writeInvalidParam(w, "listId is required")
		return
	}
	l, err := s.userLists.Update(r.Context(), req.ListID, req.Name, req.IsPublic)
	if err != nil {
		s.writeUserListError(w, r, "update user list", err)
		return
	}
	resp, ok := s.projectUserListResponse(w, r, "update user list", l)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsersListsDelete handles POST /api/users/lists/delete.
func (s *Server) handleUsersListsDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersListsDeleteRequest](r)
	if !ok || req.ListID == "" {
		writeInvalidParam(w, "listId is required")
		return
	}
	if err := s.userLists.Delete(r.Context(), req.ListID); err != nil {
		s.writeUserListError(w, r, "delete user list", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUsersListsPush handles POST /api/users/lists/push: add userId to
// listId's membership. userId must name a known actor (isKnownActor) —
// an unknown one is NO_SUCH_USER, never a fabricated success (Issue #115
// acceptance criteria).
func (s *Server) handleUsersListsPush(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersListsPushRequest](r)
	if !ok || req.ListID == "" || req.UserID == "" {
		writeInvalidParam(w, "listId and userId are required")
		return
	}

	known, err := s.isKnownActor(r.Context(), req.UserID)
	if err != nil {
		s.logger.Error("enumerate search candidates failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	if !known {
		writeNoSuchUser(w)
		return
	}

	if err := s.userLists.AddMember(r.Context(), req.ListID, req.UserID); err != nil {
		s.writeUserListError(w, r, "push user list member", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUsersListsPull handles POST /api/users/lists/pull: remove userId
// from listId's membership. Unlike push, userId is not checked against
// the known actor set — internal/userlist.Service.RemoveMember is
// idempotent for a non-member, so pulling a stale/unknown userId is a
// harmless no-op rather than an error worth distinguishing.
func (s *Server) handleUsersListsPull(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[usersListsPullRequest](r)
	if !ok || req.ListID == "" || req.UserID == "" {
		writeInvalidParam(w, "listId and userId are required")
		return
	}
	if err := s.userLists.RemoveMember(r.Context(), req.ListID, req.UserID); err != nil {
		s.writeUserListError(w, r, "pull user list member", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
