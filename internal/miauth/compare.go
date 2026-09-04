package miauth

import "crypto/subtle"

// constantTimeEqual reports whether a and b are equal without leaking
// timing information about where they first differ, per ADR-0001 §5's
// constant-time state comparison requirement. The length check runs
// before the constant-time comparison since subtle.ConstantTimeCompare
// requires equal-length inputs; comparing lengths first keeps that
// precondition explicit rather than relying on the function's own
// documented behavior for mismatched lengths.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
