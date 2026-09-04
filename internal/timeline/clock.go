package timeline

import "time"

// Clock returns the current time. Tests can provide a fixed clock so
// entry, thread, and visibility timestamps can be asserted without
// depending on wall-clock timing.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
