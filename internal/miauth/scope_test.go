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
	// exactly read:account, read:notes, write:notes, (since Issue #23
	// PR1) write:account, (since Issue #23 PR4) read:reactions/
	// write:reactions, (since Issue #23 PR6) read:notifications, and
	// (since Issue #77 PR3) read:drive/write:drive for Aria's real
	// request — this is the compat doc's literal contract, not derived
	// from the request by pure intersection (see effectiveScopes' doc
	// comment for why read:notes is unconditional; the other seven,
	// unlike read:notes, are granted via the normal intersection since
	// ariaPermissionList does request them).
	got := effectiveScopes(ariaPermissionList)
	want := []string{
		ScopeReadNotes, ScopeReadAccount, ScopeWriteNotes, ScopeWriteAccount,
		ScopeReadReactions, ScopeWriteReactions, ScopeReadNotifications, ScopeReadDrive, ScopeWriteDrive,
	}
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
	// write:blocks is a real Misskey permission ariaPermissionList
	// requests but this service never grants (no blocks feature exists)
	// — a stand-in for "unknown to this service," now that write:drive
	// (Issue #77 PR3) is no longer unknown itself.
	got := effectiveScopes(" read:account , write:blocks , write:notes ")
	want := []string{ScopeReadNotes, ScopeReadAccount, ScopeWriteNotes}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveScopes() = %v, want %v", got, want)
	}
}

func TestEffectiveScopes_GrantsDriveScopesWhenRequested(t *testing.T) {
	got := effectiveScopes("read:drive,write:drive")
	want := []string{ScopeReadNotes, ScopeReadDrive, ScopeWriteDrive}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effectiveScopes(\"read:drive,write:drive\") = %v, want %v", got, want)
	}
}

func TestClampToGrowth_AddsMissingScopesOnly(t *testing.T) {
	stored := scopesString([]string{ScopeReadNotes, ScopeReadAccount})
	recomputed := scopesString([]string{ScopeReadNotes, ScopeReadAccount, ScopeWriteAccount, ScopeReadDrive})
	got := clampToGrowth(stored, recomputed)
	want := scopesString([]string{ScopeReadNotes, ScopeReadAccount, ScopeWriteAccount, ScopeReadDrive})
	if got != want {
		t.Errorf("clampToGrowth(%q, %q) = %q, want %q", stored, recomputed, got, want)
	}
}

// TestClampToGrowth_NeverDropsAScopeRecomputedNoLongerGrants is the
// single most important regression test for §5a's grow-only decision: it
// simulates a hypothetical future grantableScopes removal by handing
// clampToGrowth a recomputed set that omits a scope storedScopes already
// has, and asserts that scope survives regardless.
func TestClampToGrowth_NeverDropsAScopeRecomputedNoLongerGrants(t *testing.T) {
	stored := scopesString([]string{ScopeReadNotes, ScopeReadAccount, ScopeWriteAccount})
	recomputed := scopesString([]string{ScopeReadNotes, ScopeReadAccount}) // ScopeWriteAccount "removed"
	got := clampToGrowth(stored, recomputed)
	if !hasScope(got, ScopeWriteAccount) {
		t.Errorf("clampToGrowth(%q, %q) = %q, dropped %s that stored already had", stored, recomputed, got, ScopeWriteAccount)
	}
	if got != stored {
		t.Errorf("clampToGrowth(%q, %q) = %q, want unchanged %q (recomputed adds nothing new)", stored, recomputed, got, stored)
	}
}

func TestClampToGrowth_Idempotent(t *testing.T) {
	stored := scopesString([]string{ScopeReadNotes, ScopeReadAccount})
	recomputed := scopesString([]string{ScopeReadNotes, ScopeReadAccount, ScopeWriteAccount})
	first := clampToGrowth(stored, recomputed)
	second := clampToGrowth(first, recomputed)
	if second != first {
		t.Errorf("clampToGrowth is not idempotent: first = %q, second = %q", first, second)
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
