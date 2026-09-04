package miauth

import "testing"

func TestDecideBinding(t *testing.T) {
	const (
		origin      = "https://misskey.example"
		otherOrigin = "https://other.example"
		allowedID   = "allowed-user"
		otherID     = "other-user"
	)

	tests := []struct {
		name                 string
		existing             *verifiedIdentity
		allowedMisskeyUserID string
		viaBootstrapGate     bool
		verified             verifiedIdentity
		wantAllow            bool
	}{
		{
			name:                 "no binding, no allowlist, ordinary flow: denied (no public first-login-wins)",
			existing:             nil,
			allowedMisskeyUserID: "",
			viaBootstrapGate:     false,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: otherID},
			wantAllow:            false,
		},
		{
			name:                 "no binding, allowlist set, ordinary flow, matching user: allowed",
			existing:             nil,
			allowedMisskeyUserID: allowedID,
			viaBootstrapGate:     false,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			wantAllow:            true,
		},
		{
			name:                 "no binding, allowlist set, ordinary flow, wrong user: denied",
			existing:             nil,
			allowedMisskeyUserID: allowedID,
			viaBootstrapGate:     false,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: otherID},
			wantAllow:            false,
		},
		{
			name:                 "no binding, no allowlist, bootstrap gate: allowed (gate itself authorizes)",
			existing:             nil,
			allowedMisskeyUserID: "",
			viaBootstrapGate:     true,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: otherID},
			wantAllow:            true,
		},
		{
			name:                 "no binding, allowlist set, bootstrap gate, matching user: allowed",
			existing:             nil,
			allowedMisskeyUserID: allowedID,
			viaBootstrapGate:     true,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			wantAllow:            true,
		},
		{
			name:                 "no binding, allowlist set, bootstrap gate, wrong user: denied (gate does not bypass allowlist)",
			existing:             nil,
			allowedMisskeyUserID: allowedID,
			viaBootstrapGate:     true,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: otherID},
			wantAllow:            false,
		},
		{
			name:                 "existing binding, matching identity: allowed",
			existing:             &verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			allowedMisskeyUserID: "",
			viaBootstrapGate:     false,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			wantAllow:            true,
		},
		{
			name:                 "existing binding, wrong user: denied",
			existing:             &verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			allowedMisskeyUserID: "",
			viaBootstrapGate:     false,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: otherID},
			wantAllow:            false,
		},
		{
			name:                 "existing binding, wrong origin (same user id at a different instance): denied",
			existing:             &verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			allowedMisskeyUserID: "",
			viaBootstrapGate:     false,
			verified:             verifiedIdentity{IdentityOrigin: otherOrigin, UpstreamUserID: allowedID},
			wantAllow:            false,
		},
		{
			name:                 "existing binding matches, but no longer matches a since-changed allowlist: denied",
			existing:             &verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			allowedMisskeyUserID: otherID,
			viaBootstrapGate:     false,
			verified:             verifiedIdentity{IdentityOrigin: origin, UpstreamUserID: allowedID},
			wantAllow:            false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideBinding(tc.existing, tc.allowedMisskeyUserID, tc.viaBootstrapGate, tc.verified)
			if got.Allow != tc.wantAllow {
				t.Errorf("decideBinding() = %+v, want Allow=%v", got, tc.wantAllow)
			}
		})
	}
}
