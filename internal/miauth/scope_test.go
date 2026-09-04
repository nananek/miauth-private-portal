package miauth

import (
	"reflect"
	"testing"
)

// ariaPermissionList is the exact, literal permission query value
// docs/compat/aria-v1.5.11.md records from a static source trace of the
// pinned Aria commit's GET /miauth/{session} construction. Note that it
// does not contain a bare "read:notes" entry.
const ariaPermissionList = "read:account,write:account,read:blocks,write:blocks," +
	"read:drive,write:drive,read:favorites,write:favorites," +
	"read:following,write:following,read:mutes,write:mutes," +
	"write:notes,read:notes-schedule,write:notes-schedule," +
	"read:notifications,write:notifications,read:reactions,write:reactions," +
	"write:votes,read:pages,write:pages,write:page-likes,read:page-likes," +
	"read:channels,write:channels,read:gallery,write:gallery," +
	"read:gallery-likes,write:gallery-likes,read:flash,write:flash," +
	"read:flash-likes,write:flash-likes,write:clip-favorite,read:clip-favorite," +
	"write:report-abuse,read:chat,write:chat"

func TestEffectiveScopes_AriaPermissionList(t *testing.T) {
	// docs/compat/aria-v1.5.11.md fixes the effective local scope set as
	// exactly read:account, read:notes, and write:notes for Aria's real
	// request — this is the compat doc's literal contract, not derived
	// from the request by pure intersection (see effectiveScopes' doc
	// comment for why read:notes is unconditional).
	got := effectiveScopes(ariaPermissionList)
	want := []string{ScopeReadNotes, ScopeReadAccount, ScopeWriteNotes}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveScopes(ariaPermissionList) = %v, want %v", got, want)
	}
}

func TestEffectiveScopes_AlwaysGrantsReadNotes(t *testing.T) {
	got := effectiveScopes("")
	want := []string{ScopeReadNotes}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveScopes(\"\") = %v, want %v", got, want)
	}
}

func TestEffectiveScopes_OnlyGrantsRequestedGrantableScopes(t *testing.T) {
	got := effectiveScopes("read:account")
	want := []string{ScopeReadNotes, ScopeReadAccount}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveScopes(\"read:account\") = %v, want %v", got, want)
	}
}

func TestEffectiveScopes_IgnoresUnknownAndWhitespace(t *testing.T) {
	got := effectiveScopes(" read:account , write:drive , write:notes ")
	want := []string{ScopeReadNotes, ScopeReadAccount, ScopeWriteNotes}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveScopes() = %v, want %v", got, want)
	}
}

func TestHasScope(t *testing.T) {
	s := scopesString([]string{ScopeReadNotes, ScopeReadAccount})
	if !hasScope(s, ScopeReadAccount) {
		t.Error("hasScope() = false, want true for a granted scope")
	}
	if hasScope(s, ScopeWriteNotes) {
		t.Error("hasScope() = true, want false for a scope not in the set")
	}
	// Exact match only: a broader scope string must not be treated as
	// implicitly satisfying a narrower request or vice versa.
	if hasScope(s, "read") {
		t.Error("hasScope() matched a substring instead of requiring an exact scope")
	}
}
