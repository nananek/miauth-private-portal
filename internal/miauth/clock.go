package miauth

import "time"

// Clock returns the current time. It exists so tests can inject a fixed
// or advancing time source instead of depending on wall-clock time for
// TTL/expiry behavior.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
