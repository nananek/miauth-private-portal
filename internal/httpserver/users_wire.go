package httpserver

import (
	"strings"
	"time"
)

// usersSearchRequest is POST /api/users/search's request (Issue #65).
// misskey_dart's ApiService omits any null field before sending
// (docs/compat/aria-v1.5.11.md's general rule), so Offset/Limit/Origin/
// Detail must all tolerate being entirely absent from the body, not
// just present-and-null; only Query is required.
type usersSearchRequest struct {
	Query  string  `json:"query"`
	Offset *int    `json:"offset"`
	Limit  *int    `json:"limit"`
	Origin *string `json:"origin"`
	Detail *bool   `json:"detail"`
}

// usersSearchByUsernameAndHostRequest is POST
// /api/users/search-by-username-and-host's request (Issue #65). Detail
// is accepted but has no effect: every result this deployment returns
// already carries the fields users/search's own Detail would add, since
// omitting them would risk the "url" key discriminator gap this issue
// exists to avoid (see userDetailedNotMe.Url's doc comment).
type usersSearchByUsernameAndHostRequest struct {
	Username *string `json:"username"`
	Host     *string `json:"host"`
	Limit    *int    `json:"limit"`
	Detail   *bool   `json:"detail"`
}

// usersShowRequest is POST /api/users/show's request (Issue #114).
// Three distinct misskey_dart request types — UsersShowRequest{userId},
// UsersShowByIdsRequest{userIds}, and UsersShowByUserNameRequest{
// username, host} — all post to this same endpoint (pinned
// misskey_users.dart, traced in docs/compat/aria-v1.5.11.md), told apart
// only by which fields are present. userIds is accepted so a malformed-
// body decode never fails on it, but handleUsersShow does not implement
// the batch lookup it requests — see that handler's own doc comment.
type usersShowRequest struct {
	UserID   *string  `json:"userId"`
	UserIDs  []string `json:"userIds"`
	Username *string  `json:"username"`
	Host     *string  `json:"host"`
}

// usersNotesRequest is POST /api/users/notes's request (Issue #114,
// misskey_dart's UsersNotesRequest). withRenotes/withReplies/withFiles/
// fileType/sinceDate/untilDate/allowPartial are accepted by
// decodeJSONBody's tolerant decoding (unknown/omitted fields never fail
// it) but have no corresponding field here and are never read — the
// same "accept but ignore" stance streamConnectBody.params takes for
// /streaming's connect frame, since this service has no renote or
// file-attachment concept to filter by.
type usersNotesRequest struct {
	UserID  string  `json:"userId"`
	Limit   *int    `json:"limit"`
	UntilID *string `json:"untilId"`
}

// searchCandidate is the intermediate projection of one known local
// actor (owner, a reserved assistant/system presentation actor, or an
// Open WebUI model's VirtualActor) that users/search and
// users/search-by-username-and-host filter and page over, before either
// endpoint converts a match to the wire userDetailedNotMe shape. Host
// mirrors userLite's own convention (noteapi_wire.go): nil for every
// local actor, non-nil only for an Open WebUI model actor's fixed
// presentation host — a label, never federation.
type searchCandidate struct {
	ActorID     string
	Username    string
	Host        *string
	DisplayName string
	CreatedAt   time.Time
}

// defaultUsersSearchLimit/maxUsersSearchLimit mirror real Misskey's own
// users/search bounds (default 10, clamp 100); this deployment's known
// actor set is always far smaller, but matching the real bounds costs
// nothing and avoids inventing a different convention.
const (
	defaultUsersSearchLimit = 10
	maxUsersSearchLimit     = 100
)

// matchesSearchQuery reports whether query (already required non-empty
// by the caller) case-insensitively matches c's username or display
// name anywhere in the string. This deployment's known-actor set is at
// most a handful of rows (Issue #65 plan §3.1), so a simple substring
// match is enough; it need not reproduce real Misskey's search ranking
// algorithm.
func matchesSearchQuery(c searchCandidate, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(c.Username), q) || strings.Contains(strings.ToLower(c.DisplayName), q)
}

// matchesOrigin reports whether c belongs to the requested origin
// scope. A nil or unrecognized origin (including omitted, matching
// misskey_dart's null-field-elision) behaves like "combined": every
// candidate matches.
func matchesOrigin(c searchCandidate, origin *string) bool {
	if origin == nil {
		return true
	}
	switch *origin {
	case "local":
		return c.Host == nil
	case "remote":
		return c.Host != nil
	default:
		return true
	}
}

// matchesUsernameAndHost reports whether c satisfies
// users/search-by-username-and-host's filter: username, when given,
// must match c's own username exactly (case-insensitively); host, when
// given and non-empty, must match c's own host exactly (also
// case-insensitively — real hostnames are case-insensitive) — an
// omitted or empty host instead requires c to be local (Host == nil),
// the same "no host means local" convention docs/compat/aria-v1.5.11.md
// documents for the wire's own host field.
func matchesUsernameAndHost(c searchCandidate, username, host *string) bool {
	if username != nil && !strings.EqualFold(c.Username, *username) {
		return false
	}
	if host == nil || *host == "" {
		return c.Host == nil
	}
	return c.Host != nil && strings.EqualFold(*c.Host, *host)
}

// clampSearchLimit applies users/search and
// users/search-by-username-and-host's shared limit convention: a nil or
// non-positive requested limit falls back to defaultUsersSearchLimit,
// and anything above maxUsersSearchLimit is clamped down to it —
// mirroring handleNotesTimeline's own defaultTimelineLimit/
// maxTimelineLimit handling.
func clampSearchLimit(requested *int) int {
	limit := defaultUsersSearchLimit
	if requested != nil && *requested > 0 {
		limit = *requested
	}
	if limit > maxUsersSearchLimit {
		limit = maxUsersSearchLimit
	}
	return limit
}

// paginateCandidates applies offset/limit to matched, tolerating an
// offset at or beyond the end (an empty page, not an error or panic) —
// the same bounds handling paginateAfterID applies for notes/children.
func paginateCandidates(matched []searchCandidate, offset, limit int) []searchCandidate {
	if offset >= len(matched) {
		return nil
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end]
}
