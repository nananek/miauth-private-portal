package miauth

import "context"

// UpstreamProvider is the narrow outbound boundary this service uses to
// verify the owner against the configured IDENTITY_ORIGIN.
// internal/provider/misskey implements it against a real upstream
// Misskey instance; tests use a fake, per AGENTS.md's "put provider
// boundaries behind narrow interfaces" rule.
type UpstreamProvider interface {
	// Check calls the upstream MiAuth check endpoint for
	// upstreamSessionID.
	//
	// ok=false, err=nil means the attempt is not yet approved or was
	// denied upstream — the upstream MiAuth check response does not
	// distinguish pending from denial (docs/compat/aria-v1.5.11.md notes
	// the same ambiguity from Aria's side of an equivalent call), so
	// this boundary does not invent a distinction the wire contract
	// does not make.
	//
	// err is a transport, timeout, or response-decode failure: it is
	// evidence about the upstream call itself, not about the user's
	// identity, and callers must not treat it the same as ok=false.
	Check(ctx context.Context, upstreamSessionID string) (upstreamUserID string, ok bool, err error)
}
