package miauth

import "testing"

func TestConstantTimeEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"equal", "same-value", "same-value", true},
		{"different same length", "aaaaaaaa", "bbbbbbbb", false},
		{"different length", "short", "much-longer-value", false},
		{"both empty", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := constantTimeEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("constantTimeEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
