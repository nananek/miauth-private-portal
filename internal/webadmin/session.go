package webadmin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// newRawSessionToken/hashSessionToken/newCSRFToken duplicate the exact
// crypto/rand + SHA-256 pattern token.go already uses for bootstrap
// tokens (see that file's own doc comment for why duplication, not a
// shared helper, is deliberate here). The CSRF token is NOT hashed at
// rest (unlike the session token): it is a synchronizer value compared
// directly against what the session's own page embedded and echoed
// back, not a bearer credential presented instead of proving session
// possession — see plan-136-phase2 §1 Decision 5.
func newRawSessionToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newCSRFToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}
