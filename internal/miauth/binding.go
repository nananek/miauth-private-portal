package miauth

// verifiedIdentity carries the upstream identity pair used for binding
// decisions. LocalActorID is populated only when the value came from an
// existing OwnerBinding, allowing the authorization path to reuse that
// binding without querying the Owner actor again.
type verifiedIdentity struct {
	IdentityOrigin string
	UpstreamUserID string
	LocalActorID   string
}

// bindingDecision is the pure result of evaluating a verified upstream
// identity against this deployment's owner-binding rules.
type bindingDecision struct {
	Allow bool
}

// decideBinding implements ADR-0001 §2's owner-binding rules as a pure
// function so the decision matrix can be tested without a database.
//
//   - No existing binding, reached through the ordinary Aria-triggered
//     flow (viaBootstrapGate is false): allowed only when
//     allowedMisskeyUserID is configured and the verified user ID
//     matches it exactly. There is no public first-login-wins path —
//     an unset allowlist means the deployment is bootstrap-only.
//   - No existing binding, reached through the operator bootstrap gate:
//     the gate's own single-use consumption is what authorizes this
//     path (ADR-0001 §2), so it does not additionally require
//     allowedMisskeyUserID. If allowedMisskeyUserID *is* configured,
//     though, the verified identity must still match it: the gate
//     augments the "allowlist unset" case, it must never let a leaked
//     or reused gate bind an identity a configured allowlist would
//     otherwise reject.
//   - An existing binding: allowed only when the verified identity
//     matches the existing binding exactly (an already-bound deployment
//     always re-verifies against its stored identity — ADR-0001's
//     authentication sequence), and, if allowedMisskeyUserID is set,
//     also matches it.
func decideBinding(existing *verifiedIdentity, allowedMisskeyUserID string, viaBootstrapGate bool, verified verifiedIdentity) bindingDecision {
	matchesAllowlist := allowedMisskeyUserID == "" || verified.UpstreamUserID == allowedMisskeyUserID

	if existing != nil {
		matchesExisting := verified.IdentityOrigin == existing.IdentityOrigin && verified.UpstreamUserID == existing.UpstreamUserID
		return bindingDecision{Allow: matchesExisting && matchesAllowlist}
	}

	if viaBootstrapGate {
		return bindingDecision{Allow: matchesAllowlist}
	}

	return bindingDecision{Allow: allowedMisskeyUserID != "" && matchesAllowlist}
}
