package sqlite

import (
	"testing"
	"time"
)

// TestFormatTime_LexicographicOrderMatchesChronologicalOrder guards against
// regressing to time.RFC3339Nano, whose formatter omits the fractional
// second entirely when it is exactly zero. That breaks every ORDER BY and
// row-value pagination cursor on a stored timestamp column, all of which
// rely on formatTime's output sorting the same way as string as it does as
// a time (entryRepository.ListTimeline's (created_at, id) > (?, ?) cursor
// in particular).
func TestFormatTime_LexicographicOrderMatchesChronologicalOrder(t *testing.T) {
	earlier := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)         // no fractional part
	later := time.Date(2024, 1, 1, 0, 0, 0, 500_000_000, time.UTC) // .5s later, same second

	if !later.After(earlier) {
		t.Fatalf("test fixture is wrong: %v is not after %v", later, earlier)
	}

	gotEarlier, gotLater := formatTime(earlier), formatTime(later)
	if !(gotEarlier < gotLater) {
		t.Fatalf("formatTime(%v) = %q, formatTime(%v) = %q; want the first to sort before the second",
			earlier, gotEarlier, later, gotLater)
	}
}

func TestFormatTime_AlwaysWritesTheFraction(t *testing.T) {
	got := formatTime(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	want := "2024-01-01T00:00:00.000000000Z"
	if got != want {
		t.Fatalf("formatTime = %q, want %q", got, want)
	}
}

func TestParseTime_RoundTripsFormatTime(t *testing.T) {
	for _, tc := range []time.Time{
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 6, 15, 12, 30, 45, 123_456_789, time.UTC),
		time.Date(2024, 6, 15, 12, 30, 45, 500_000_000, time.UTC),
	} {
		got, err := parseTime(formatTime(tc))
		if err != nil {
			t.Fatalf("parseTime(formatTime(%v)): %v", tc, err)
		}
		if !got.Equal(tc) {
			t.Errorf("round-trip of %v = %v, want equal", tc, got)
		}
	}
}
