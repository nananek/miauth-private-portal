package webadmin

import "testing"

func TestNewRawBootstrapToken_IsHighEntropyAndUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for range n {
		raw := newRawBootstrapToken()
		if len(raw) < 40 {
			t.Fatalf("token too short: %q (len=%d)", raw, len(raw))
		}
		if _, dup := seen[raw]; dup {
			t.Fatalf("duplicate token generated: %q", raw)
		}
		seen[raw] = struct{}{}
	}
}

func TestHashBootstrapToken_IsDeterministicAndDistinctForDistinctInput(t *testing.T) {
	a := hashBootstrapToken("token-a")
	b := hashBootstrapToken("token-a")
	c := hashBootstrapToken("token-b")
	if a != b {
		t.Fatalf("hash not deterministic: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("distinct inputs hashed to the same value: %q", a)
	}
}
