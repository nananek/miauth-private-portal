package miauth

import "testing"

func TestNewRawAPIToken_IsHighEntropyAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok := newRawAPIToken()
		if len(tok) < 32 {
			t.Fatalf("newRawAPIToken() = %q, too short for a 256-bit token", tok)
		}
		if seen[tok] {
			t.Fatalf("newRawAPIToken() produced a duplicate: %q", tok)
		}
		seen[tok] = true
	}
}

func TestHashAPIToken_IsDeterministicAndDistinctForDistinctInput(t *testing.T) {
	a := hashAPIToken("token-a")
	b := hashAPIToken("token-a")
	if a != b {
		t.Errorf("hashAPIToken() is not deterministic: %q != %q", a, b)
	}

	c := hashAPIToken("token-b")
	if a == c {
		t.Error("hashAPIToken() produced the same hash for different inputs")
	}
}

func TestHashAPIToken_NeverEqualsRawToken(t *testing.T) {
	raw := newRawAPIToken()
	if hashAPIToken(raw) == raw {
		t.Error("hashAPIToken() returned the raw token unchanged")
	}
}
