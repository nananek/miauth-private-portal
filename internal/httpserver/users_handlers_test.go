package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodeUsers decodes rec's body into a []userDetailedNotMe, failing the
// test on malformed JSON.
func decodeUsers(t *testing.T, rec *httptest.ResponseRecorder) []userDetailedNotMe {
	t.Helper()
	var users []userDetailedNotMe
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users: %v; body=%s", err, rec.Body.String())
	}
	return users
}

// usernames extracts users' Username fields, in response order, for
// order-and-membership assertions without repeating field access.
func usernames(users []userDetailedNotMe) []string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = u.Username
	}
	return out
}

// TestUsersSearch_ResponseCarriesURLKeyDiscriminatorForEveryUser is
// Issue #65's single most load-bearing assertion. misskey_dart's
// polymorphic User.fromJson (lib/src/data/base/user.dart) decodes a
// search result as UserLite rather than UserDetailed whenever the "url"
// key is absent from the object, and every one of Aria's user-search
// call sites (user_select_dialog.dart, search_users_notifier_provider.
// dart, search_users_by_username_provider.dart) filters its results
// with `.whereType<UserDetailed>()` — so a missing "url" key would
// silently drop every result from what Aria ever shows, while this
// endpoint still returns 200 with superficially correct-looking JSON.
// This checks raw decoded JSON (map[string]any), not the typed
// userDetailedNotMe struct: a struct comparison could never tell "the
// key is absent" apart from "the key is present with value null," and
// only the former is the failure mode this test exists to catch. It
// also pins the discriminator table's other rule: "avatarId" and
// "isFollowing" must never appear, or the object would instead decode
// as MeDetailed/UserDetailedNotMeWithRelations and fail to parse
// against their own additional required fields.
//
// The query ("o") is chosen to match both a local actor (the owner,
// username "owner") and the Open WebUI model actor
// (newNoteAPITestServerOpenWebUIEnabled's seeded model has a generated
// username and a display name equal to its own external id
// "gpt-oss:20b", both containing "o"), so one assertion covers a
// Host == nil and a Host != nil projection alike.
func TestUsersSearch_ResponseCarriesURLKeyDiscriminatorForEveryUser(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)

	rec := ts.post(t, "/api/users/search", map[string]any{"query": "o"})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/search: %d %s", rec.Code, rec.Body.String())
	}

	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if len(raw) < 2 {
		t.Fatalf("results = %v, want at least 2 (owner and the openwebui model)", raw)
	}
	for _, u := range raw {
		if _, ok := u["url"]; !ok {
			t.Errorf("user %v missing \"url\" key: would decode as UserLite and be silently dropped by Aria's whereType<UserDetailed>()", u)
		}
		if _, ok := u["avatarId"]; ok {
			t.Errorf("user %v has \"avatarId\": would misdecode as MeDetailed", u)
		}
		if _, ok := u["isFollowing"]; ok {
			t.Errorf("user %v has \"isFollowing\": would misdecode as UserDetailedNotMeWithRelations", u)
		}
	}
}

// TestUsersSearch_MatchesUsernameAndDisplayNameCaseInsensitively covers
// both fields matchesSearchQuery checks, and that matching ignores case
// (Aria's own search box does not require an exact-case query).
func TestUsersSearch_MatchesUsernameAndDisplayNameCaseInsensitively(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)

	rec := ts.post(t, "/api/users/search", map[string]any{"query": "ASSIST"})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/search: %d %s", rec.Code, rec.Body.String())
	}
	got := usernames(decodeUsers(t, rec))
	if len(got) != 1 || got[0] != "assistant" {
		t.Fatalf("usernames = %v, want exactly [assistant]", got)
	}

	// "gpt-oss" only appears in the model's display name (its own
	// external id "gpt-oss:20b" — no catalog sync has run to give it a
	// nicer one), never its generated username, so a hit here is
	// specifically a display-name match.
	rec = ts.post(t, "/api/users/search", map[string]any{"query": "gpt-oss"})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/search: %d %s", rec.Code, rec.Body.String())
	}
	got = usernames(decodeUsers(t, rec))
	wantSlug := openWebUITestModelSlug()
	if len(got) != 1 || got[0] != wantSlug {
		t.Fatalf("usernames = %v, want exactly [%s] (a display-name match)", got, wantSlug)
	}
}

// TestUsersSearch_OriginFilterScopesLocalVsRemote covers the "origin"
// request field: "local" must exclude the Open WebUI model actor (the
// only candidate with a non-nil host), "remote" must return only it,
// and an omitted/"combined" origin must return both.
func TestUsersSearch_OriginFilterScopesLocalVsRemote(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	modelSlug := openWebUITestModelSlug()

	cases := []struct {
		origin any
		want   []string
	}{
		{nil, []string{"owner", modelSlug}},
		{"combined", []string{"owner", modelSlug}},
		{"local", []string{"owner"}},
		{"remote", []string{modelSlug}},
	}
	for _, c := range cases {
		body := map[string]any{"query": "o"}
		if c.origin != nil {
			body["origin"] = c.origin
		}
		rec := ts.post(t, "/api/users/search", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("origin=%v: %d %s", c.origin, rec.Code, rec.Body.String())
		}
		got := usernames(decodeUsers(t, rec))
		if !sameElements(got, c.want) {
			t.Errorf("origin=%v: usernames = %v, want %v", c.origin, got, c.want)
		}
	}
}

// TestUsersSearch_EmptyQueryIsInvalidParam covers query's "required"
// contract (Issue #65 plan §1): an absent or empty query must not be
// treated as "match everything".
func TestUsersSearch_EmptyQueryIsInvalidParam(t *testing.T) {
	ts := newNoteAPITestServer(t)

	for _, body := range []map[string]any{{}, {"query": ""}} {
		rec := ts.post(t, "/api/users/search", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%v: status = %d %s, want %d", body, rec.Code, rec.Body.String(), http.StatusBadRequest)
		}
	}
}

// TestUsersSearch_NoMatchesReturnsEmptyArrayNotNull covers Aria's own
// pagination-loop-safe convention (mirrored from handleNotesTimeline
// elsewhere in this package): a JSON `null` array and misskey_dart's
// generated list parsers do not mix well, so an empty result must be
// serialized as `[]`.
func TestUsersSearch_NoMatchesReturnsEmptyArrayNotNull(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/users/search", map[string]any{"query": "no-such-actor-exists"})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/search: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Errorf("body = %q, want an empty JSON array, not null", got)
	}
}

// TestUsersSearch_OffsetAndLimitPageDeterministically covers offset/
// limit handling, including an offset past the end producing an empty
// (not erroring) page.
func TestUsersSearch_OffsetAndLimitPageDeterministically(t *testing.T) {
	ts := newNoteAPITestServer(t)
	// "s" matches "assistant" and "system" (candidate order: owner,
	// assistant, system), never "owner": exactly two matches to page
	// over deterministically.
	limit := 1

	rec := ts.post(t, "/api/users/search", map[string]any{"query": "s", "limit": limit, "offset": 0})
	got := usernames(decodeUsers(t, rec))
	if len(got) != 1 || got[0] != "assistant" {
		t.Fatalf("offset=0: usernames = %v, want [assistant]", got)
	}

	rec = ts.post(t, "/api/users/search", map[string]any{"query": "s", "limit": limit, "offset": 1})
	got = usernames(decodeUsers(t, rec))
	if len(got) != 1 || got[0] != "system" {
		t.Fatalf("offset=1: usernames = %v, want [system]", got)
	}

	rec = ts.post(t, "/api/users/search", map[string]any{"query": "s", "limit": limit, "offset": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("offset=2: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeUsers(t, rec); len(got) != 0 {
		t.Errorf("offset=2 (past the end): usernames = %v, want none", usernames(got))
	}
}

// TestUsersSearch_OpenWebUIDisabledNeverReturnsModelActor covers Issue
// #65 plan §5's required "openwebui無効時に0件" coverage: with
// s.virtualActors nil (OPENWEBUI_ENABLED off), a query that would only
// ever match the model actor must return no results at all, not a
// degraded actor-ID projection.
func TestUsersSearch_OpenWebUIDisabledNeverReturnsModelActor(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/users/search", map[string]any{"query": "gpt-oss"})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/search: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeUsers(t, rec); len(got) != 0 {
		t.Errorf("usernames = %v, want none (openwebui disabled)", usernames(got))
	}
}

// TestUsersSearchByUsernameAndHost_ExactUsernameMatchIsCaseInsensitive
// covers the endpoint's exact-username lookup, with no host given (the
// local-only default).
func TestUsersSearchByUsernameAndHost_ExactUsernameMatchIsCaseInsensitive(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/users/search-by-username-and-host", map[string]any{"username": "ASSISTANT"})
	if rec.Code != http.StatusOK {
		t.Fatalf("search-by-username-and-host: %d %s", rec.Code, rec.Body.String())
	}
	got := usernames(decodeUsers(t, rec))
	if len(got) != 1 || got[0] != "assistant" {
		t.Fatalf("usernames = %v, want exactly [assistant]", got)
	}
}

// TestUsersSearchByUsernameAndHost_HostScopesLocalVsRemoteModel covers
// the host filter's three shapes: omitted (local only, so the remote
// model actor's exact username still misses), matching the model's own
// presentation host (a hit), and a different host (a miss even for a
// username that otherwise matches).
func TestUsersSearchByUsernameAndHost_HostScopesLocalVsRemoteModel(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	modelSlug := openWebUITestModelSlug()

	rec := ts.post(t, "/api/users/search-by-username-and-host", map[string]any{"username": modelSlug})
	if rec.Code != http.StatusOK {
		t.Fatalf("no host: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeUsers(t, rec); len(got) != 0 {
		t.Errorf("username=%s, no host: usernames = %v, want none (host omitted means local-only)", modelSlug, usernames(got))
	}

	rec = ts.post(t, "/api/users/search-by-username-and-host", map[string]any{"username": modelSlug, "host": "openwebui.example.net"})
	if rec.Code != http.StatusOK {
		t.Fatalf("matching host: %d %s", rec.Code, rec.Body.String())
	}
	got := usernames(decodeUsers(t, rec))
	if len(got) != 1 || got[0] != modelSlug {
		t.Fatalf("username=%s, host=openwebui.example.net: usernames = %v, want exactly [%s]", modelSlug, got, modelSlug)
	}

	rec = ts.post(t, "/api/users/search-by-username-and-host", map[string]any{"username": modelSlug, "host": "different.example.net"})
	if rec.Code != http.StatusOK {
		t.Fatalf("mismatched host: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeUsers(t, rec); len(got) != 0 {
		t.Errorf("username=%s, host=different.example.net: usernames = %v, want none", modelSlug, usernames(got))
	}
}

// TestUsersSearchByUsernameAndHost_UnknownUsernameReturnsEmptyArray
// covers the "no match" shape for this endpoint specifically, since it
// has no query-required validation the way users/search does (username
// and host are both optional per Issue #65 plan §1) — an unknown
// username is simply zero results, not an error.
func TestUsersSearchByUsernameAndHost_UnknownUsernameReturnsEmptyArray(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/users/search-by-username-and-host", map[string]any{"username": "no-such-actor"})
	if rec.Code != http.StatusOK {
		t.Fatalf("search-by-username-and-host: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Errorf("body = %q, want an empty JSON array, not null", got)
	}
}

// TestUsersShow_ByUserIDCarriesURLKeyDiscriminator is users/show's
// counterpart to TestUsersSearch_ResponseCarriesURLKeyDiscriminatorForEveryUser
// (Issue #114): the same "url" key present / "avatarId","isFollowing"
// keys absent contract applies here, since users/show's response is the
// same userDetailedNotMe struct, decoded through the same polymorphic
// UserDetailed.fromJson discriminator (docs/compat/aria-v1.5.11.md).
func TestUsersShow_ByUserIDCarriesURLKeyDiscriminator(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/users/show", map[string]any{"userId": ts.ownerID})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/show: %d %s", rec.Code, rec.Body.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["url"]; !ok {
		t.Errorf("response %v missing \"url\" key: would decode as UserLite", raw)
	}
	if _, ok := raw["avatarId"]; ok {
		t.Errorf("response %v has \"avatarId\": would misdecode as MeDetailed", raw)
	}
	if _, ok := raw["isFollowing"]; ok {
		t.Errorf("response %v has \"isFollowing\": would misdecode as UserDetailedNotMeWithRelations", raw)
	}
	if raw["id"] != ts.ownerID {
		t.Errorf("id = %v, want %s", raw["id"], ts.ownerID)
	}
}

// TestUsersShow_ByUsername covers the username(+host) dispatch branch
// against a reserved local actor (host omitted, local-only).
func TestUsersShow_ByUsername(t *testing.T) {
	ts := newNoteAPITestServer(t)

	rec := ts.post(t, "/api/users/show", map[string]any{"username": "assistant"})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/show: %d %s", rec.Code, rec.Body.String())
	}
	var got userDetailedNotMe
	mustDecode(t, rec, &got)
	if got.Username != "assistant" {
		t.Errorf("username = %q, want %q", got.Username, "assistant")
	}
}

// TestUsersShow_ByUsernameAndHost_OpenWebUIModel covers the
// username+host branch against a non-local actor, exercising the same
// matchesUsernameAndHost helper users/search-by-username-and-host uses.
func TestUsersShow_ByUsernameAndHost_OpenWebUIModel(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	modelSlug := openWebUITestModelSlug()

	rec := ts.post(t, "/api/users/show", map[string]any{"username": modelSlug, "host": "openwebui.example.net"})
	if rec.Code != http.StatusOK {
		t.Fatalf("users/show: %d %s", rec.Code, rec.Body.String())
	}
	var got userDetailedNotMe
	mustDecode(t, rec, &got)
	if got.Username != modelSlug {
		t.Errorf("username = %q, want %q", got.Username, modelSlug)
	}
	if got.Host == nil || *got.Host != "openwebui.example.net" {
		t.Errorf("host = %v, want %q", got.Host, "openwebui.example.net")
	}
}

// TestUsersShow_UnknownUserIDReturnsNoSuchUser and
// TestUsersShow_UnknownUsernameReturnsNoSuchUser cover Issue #114's
// "fail explicitly, never fabricate success" acceptance criterion for
// both dispatch branches.
func TestUsersShow_UnknownUserIDReturnsNoSuchUser(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/users/show", map[string]any{"userId": "no-such-actor-id"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_USER")
}

func TestUsersShow_UnknownUsernameReturnsNoSuchUser(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/users/show", map[string]any{"username": "no-such-actor"})
	assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_USER")
}

// TestUsersShow_UserIDsIsUnsupportedFeature pins the deliberate
// non-implementation of UsersShowByIdsRequest's batch lookup (Issue
// #114 plan §2.3): an explicit, typed rejection, never a fabricated
// empty or partial result.
func TestUsersShow_UserIDsIsUnsupportedFeature(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/users/show", map[string]any{"userIds": []string{ts.ownerID}})
	assertWireError(t, rec, http.StatusBadRequest, "UNSUPPORTED_FEATURE")
}

// TestUsersShow_NoFieldsIsInvalidParam covers the request shape with
// none of userId/userIds/username set.
func TestUsersShow_NoFieldsIsInvalidParam(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/users/show", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d %s, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
}

// sameElements reports whether got and want contain the same strings,
// ignoring order — origin filtering's own order (candidate-enumeration
// order) is an implementation detail this test does not pin.
func sameElements(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}
