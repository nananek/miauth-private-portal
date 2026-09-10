package miauth

import "strings"

// Effective local API scopes this service ever grants. Notes-endpoint
// enforcement of write:notes/read:notes is Issue #7's scope; write:account
// enforcement (POST /api/i/update) is Issue #23 PR1's; read:reactions/
// write:reactions enforcement (POST /api/notes/reactions/create, /delete,
// and /api/notes/reactions) is Issue #23 PR4's; read:notifications
// enforcement (POST /api/i/notifications) is Issue #23 PR6's; #5 only
// computes and stores the effective set on the issued token.
const (
	ScopeReadAccount       = "read:account"
	ScopeReadNotes         = "read:notes"
	ScopeWriteNotes        = "write:notes"
	ScopeWriteAccount      = "write:account"
	ScopeReadReactions     = "read:reactions"
	ScopeWriteReactions    = "write:reactions"
	ScopeReadNotifications = "read:notifications"
	// ScopeReadDrive and ScopeWriteDrive gate Issue #77 PR3's Misskey-
	// compatible Drive API. Aria's MiAuth permission query already
	// requests both today (docs/compat/aria-v1.5.11.md), so — like
	// read:notifications before it — a local API token issued before
	// this PR shipped will not carry them until the owner re-approves
	// through miauthctl, or an operator runs `miauthctl tokens
	// reflect-scopes` (Issue #133) to add them to the existing token in
	// place.
	ScopeReadDrive  = "read:drive"
	ScopeWriteDrive = "write:drive"
)

// grantableScopes are the scopes granted only when Aria's requested
// permission set contains them. read:notes is deliberately excluded
// here: see effectiveScopes.
var grantableScopes = []string{ScopeReadAccount, ScopeWriteNotes, ScopeWriteAccount, ScopeReadReactions, ScopeWriteReactions, ScopeReadNotifications, ScopeReadDrive, ScopeWriteDrive}

// effectiveScopes computes the local API token scopes granted for a raw
// requested permission string (Aria's comma-separated `permission`
// query value).
//
// docs/compat/aria-v1.5.11.md fixes the effective local scope set as
// exactly read:account, read:notes, write:notes, (since Issue #23 PR1)
// write:account, (since Issue #23 PR4) read:reactions/write:reactions,
// and (since Issue #23 PR6) read:notifications. read:notes is granted
// unconditionally rather than intersected with the request because
// Aria's fixed, source-traced permission list never actually requests a
// bare "read:notes" — only "write:notes" appears in it — yet the compat
// doc requires read:notes as part of what a successful login grants. An
// intersection-only implementation would therefore never grant it; this
// is not a bug, it is the compat doc's explicit contract. write:account,
// the two reactions scopes, and read:notifications, unlike read:notes,
// Aria's permission list does request explicitly (see
// docs/compat/aria-v1.5.11.md), so they are granted via the normal
// intersection below rather than needing the same unconditional
// carve-out. A token issued before Issue #23 PR4/PR6 shipped never
// carries read:reactions/write:reactions/read:notifications even if
// Aria's original request included them — re-approving through
// miauthctl adds them retroactively, or (Issue #133) an operator can run
// `miauthctl tokens reflect-scopes --token-id <id>` (or `--all`) to add
// them to an already-issued token in place, without a fresh Aria login;
// see Service.ReflectScopes.
func effectiveScopes(requestedPermission string) []string {
	requested := make(map[string]bool)
	for _, p := range strings.Split(requestedPermission, ",") {
		if p = strings.TrimSpace(p); p != "" {
			requested[p] = true
		}
	}

	out := []string{ScopeReadNotes}
	for _, s := range grantableScopes {
		if requested[s] {
			out = append(out, s)
		}
	}
	return out
}

// scopesString renders scopes as the space-separated form stored in
// APIToken.Scopes.
func scopesString(scopes []string) string {
	return strings.Join(scopes, " ")
}

// clampToGrowth returns storedScopes with every scope from recomputed that
// storedScopes does not already have appended (grantableScopes' declared
// order, mirroring effectiveScopes' own ordering), and never drops a
// scope storedScopes already has — even if recomputed (today's
// grantableScopes intersected with the session's original request) would
// no longer include it.
//
// This is Service.ReflectScopes' (Issue #133) core, deliberate design
// decision: additive-only, never a full recompute-and-replace. Every
// change to grantableScopes in this codebase's history so far has been a
// pure addition, never a removal, so in practice recomputed is always a
// superset of what an older token would have. But the code must not
// assume that invariant holds forever: if a future change ever did
// remove or rename a grantableScopes entry, a naive "replace with
// effectiveScopes(...) verbatim" reflect would silently revoke a scope
// from an already-issued, already-in-use token as a side effect of a
// routine, expected-to-be-safe "catch this token up" maintenance
// command — surprising, security-relevant behavior hidden inside an
// operation whose name and purpose both promise only growth. A
// legitimate scope revocation is a categorically different, higher-stakes
// operation than "pick up new capabilities" and deserves its own
// explicit, loudly-named tool with its own confirmation/audit trail, not
// a side effect of reflect-scopes.
func clampToGrowth(storedScopes, recomputed string) string {
	have := map[string]bool{}
	for _, s := range strings.Fields(storedScopes) {
		have[s] = true
	}
	out := strings.Fields(storedScopes)
	for _, s := range strings.Fields(recomputed) {
		if !have[s] {
			out = append(out, s)
			have[s] = true
		}
	}
	return scopesString(out)
}

// hasScope reports whether the space-separated scopes string s grants
// scope by exact match — AGENTS.md requires exact scope enforcement,
// never inferring an unimplemented capability from a broader request.
func hasScope(s, scope string) bool {
	for _, got := range strings.Fields(s) {
		if got == scope {
			return true
		}
	}
	return false
}
