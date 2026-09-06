package openwebui

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// uuidV4Pattern is the RFC 4122 version-4 form
// docs/compat/openwebui-0.11.3.md's captured ids take, which
// newRemoteMessageID must reproduce so a value it mints is
// indistinguishable, on the wire, from one Open WebUI itself would
// generate.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewRemoteMessageID_IsRFC4122Version4(t *testing.T) {
	id := newRemoteMessageID()
	if !uuidV4Pattern.MatchString(id) {
		t.Fatalf("newRemoteMessageID() = %q, want an RFC 4122 v4 UUID", id)
	}
}

// TestNewRemoteMessageID_IsUnique backs the rule that these ids are
// persisted before the call that uses them: two turns' ids must never
// collide, or a completions request keyed by a repeated id would
// silently overwrite the wrong message (compat (e)).
func TestNewRemoteMessageID_IsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := newRemoteMessageID()
		if seen[id] {
			t.Fatalf("newRemoteMessageID() repeated %q", id)
		}
		seen[id] = true
	}
}

// TestProviderError_ErrorTextIsFixedAndCarriesNoWrappedText is the
// redaction rule ProviderError's doc comment describes: Error() must
// never include anything from the wrapped err, since that err's text
// may (per ADR-0005 D6, observed against the pinned Open WebUI
// instance) contain upstream credential text verbatim.
func TestProviderError_ErrorTextIsFixedAndCarriesNoWrappedText(t *testing.T) {
	secret := "sk-mock-upstream-secret"
	err := NewProviderError(CategoryServerError, PhaseTurn, errors.New("upstream said: "+secret))

	got := err.Error()
	want := "openwebui: turn: server_error"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if strings.Contains(got, secret) {
		t.Errorf("Error() = %q leaks the wrapped error text", got)
	}
}

func TestProviderError_Unwrap(t *testing.T) {
	inner := errors.New("boom")
	err := NewProviderError(CategoryTimeout, PhaseCreate, inner)

	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As(err, &pe) = false")
	}
	if pe.Category != CategoryTimeout || pe.Phase != PhaseCreate {
		t.Errorf("pe = %+v, want Category=%q Phase=%q", pe, CategoryTimeout, PhaseCreate)
	}
	if !errors.Is(err, inner) {
		t.Errorf("errors.Is(err, inner) = false, want true (Unwrap must expose the wrapped error)")
	}
}

func TestNewProviderError_NilErrReturnsNil(t *testing.T) {
	if err := NewProviderError(CategoryTimeout, PhaseCreate, nil); err != nil {
		t.Errorf("NewProviderError(..., nil) = %v, want nil", err)
	}
}
