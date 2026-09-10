package webadmin

import "testing"

func TestNewRawSessionToken_IsHighEntropyAndUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for range n {
		raw := newRawSessionToken()
		if len(raw) < 40 {
			t.Fatalf("token too short: %q (len=%d)", raw, len(raw))
		}
		if _, dup := seen[raw]; dup {
			t.Fatalf("duplicate token generated: %q", raw)
		}
		seen[raw] = struct{}{}
	}
}

func TestHashSessionToken_IsDeterministicAndDistinctForDistinctInput(t *testing.T) {
	a := hashSessionToken("token-a")
	b := hashSessionToken("token-a")
	c := hashSessionToken("token-b")
	if a != b {
		t.Fatalf("hash not deterministic: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("distinct inputs hashed to the same value: %q", a)
	}
}

func TestHashSessionToken_NeverEqualsRawToken(t *testing.T) {
	raw := newRawSessionToken()
	if hashSessionToken(raw) == raw {
		t.Fatal("hash equals the raw token")
	}
}

func TestNewCSRFToken_IsHighEntropyAndUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for range n {
		raw := newCSRFToken()
		if len(raw) < 40 {
			t.Fatalf("token too short: %q (len=%d)", raw, len(raw))
		}
		if _, dup := seen[raw]; dup {
			t.Fatalf("duplicate token generated: %q", raw)
		}
		seen[raw] = struct{}{}
	}
}
