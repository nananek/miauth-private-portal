package webadmin

import "github.com/go-webauthn/webauthn/webauthn"

// ownerWebAuthnUser adapts the singleton Owner actor to go-webauthn's
// User interface (ADR-0010 Decision 6: a web admin credential is bound
// to the Owner actor, never a new login-capable actor type).
// WebAuthnID is the Owner's actor ID — an opaque domain.NewID() string,
// well under WebAuthn's 64-byte user-handle limit.
type ownerWebAuthnUser struct {
	ownerActorID     string
	ownerUsername    string
	ownerDisplayName string
	credentials      []webauthn.Credential
}

func (u ownerWebAuthnUser) WebAuthnID() []byte                         { return []byte(u.ownerActorID) }
func (u ownerWebAuthnUser) WebAuthnName() string                       { return u.ownerUsername }
func (u ownerWebAuthnUser) WebAuthnDisplayName() string                { return u.ownerDisplayName }
func (u ownerWebAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
