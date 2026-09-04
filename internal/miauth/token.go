package miauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// newRawAPIToken returns a new local API token: 32 bytes (256 bits) of
// crypto/rand entropy, base64url-encoded without padding. It is
// returned to the caller (Aria) exactly once by Service.Check; only
// hashAPIToken's output of it is ever persisted.
func newRawAPIToken() string {
	buf := make([]byte, 32)
	// crypto/rand.Read never returns an error on Go 1.24+ (it crashes the
	// process instead, go.dev/issue/66821), matching domain.NewID's use
	// of the same package.
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// hashAPIToken returns the one-way hash persisted as APIToken.TokenHash
// and used to look a token up by exact match.
//
// Plain SHA-256 is deliberate, not an oversight: bcrypt/argon2 and other
// slow key-derivation functions exist to defend a low-entropy,
// human-chosen secret (a password) against offline brute force by
// making each guess expensive. This token is the opposite case — a
// server-generated 256-bit random value with no guessable structure —
// so a fast unsalted hash is standard practice for this class of secret
// (the same approach GitHub and Stripe use for their own API tokens); a
// slow KDF here would only add verification latency an attacker gains
// nothing from forcing.
func hashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
