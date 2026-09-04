package miauth

import "strings"

// Effective local API scopes this service ever grants. Notes-endpoint
// enforcement of write:notes/read:notes is Issue #7's scope; #5 only
// computes and stores the effective set on the issued token.
const (
	ScopeReadAccount = "read:account"
	ScopeReadNotes   = "read:notes"
	ScopeWriteNotes  = "write:notes"
)

// grantableScopes are the scopes granted only when Aria's requested
// permission set contains them. read:notes is deliberately excluded
// here: see effectiveScopes.
var grantableScopes = []string{ScopeReadAccount, ScopeWriteNotes}

// effectiveScopes computes the local API token scopes granted for a raw
// requested permission string (Aria's comma-separated `permission`
// query value).
//
// docs/compat/aria-v1.5.11.md fixes the effective local scope set as
// exactly read:account, read:notes, and write:notes. read:notes is
// granted unconditionally rather than intersected with the request
// because Aria's fixed, source-traced permission list never actually
// requests a bare "read:notes" — only "write:notes" appears in it — yet
// the compat doc requires read:notes as part of what a successful login
// grants. An intersection-only implementation would therefore never
// grant it; this is not a bug, it is the compat doc's explicit
// contract.
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
