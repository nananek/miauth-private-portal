package webadmin

import (
	"encoding/json"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// encodeSessionData/decodeSessionData round-trip *webauthn.SessionData
// through the library's own JSON tags (it's already a plain
// json.Marshal-able struct — verified against the installed library
// version; if a future version changes that, these two functions are
// the only place to adapt).
func encodeSessionData(s *webauthn.SessionData) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func decodeSessionData(raw string) (*webauthn.SessionData, error) {
	var s webauthn.SessionData
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func encodeCredential(c *webauthn.Credential) (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

// decodeCredentials round-trips every stored WebAdminCredential's
// CredentialJSON back into the library's own Credential type, used both
// to seed webauthn.User.WebAuthnCredentials() and to build a
// registration ceremony's exclude list. A corrupt row is silently
// skipped rather than failing the whole call: one bad historical row
// must not block every future registration attempt for the Owner.
func decodeCredentials(records []domain.WebAdminCredential) []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(records))
	for _, r := range records {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(r.CredentialJSON), &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}
