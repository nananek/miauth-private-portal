package webadmin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// newRawBootstrapToken and hashBootstrapToken duplicate
// internal/miauth's newRawAPIToken/hashAPIToken exactly (32 bytes of
// crypto/rand entropy, base64url-encoded; SHA-256 hash-at-rest — see
// that package's hashAPIToken doc comment for why a fast unsalted hash
// is correct for a server-generated high-entropy secret, not a
// password). Duplicated, not imported: ADR-0001's "keep distinct
// records for distinct credentials" rule is precisely about not letting
// two credential types share code paths that could let one's behavior
// silently drift the other's.
func newRawBootstrapToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf) // never errors on Go 1.24+; see miauth's identical comment
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hashBootstrapToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
