package jobs

import (
	"testing"
	"time"
)

func TestBackoffIsBoundedWithJitter(t *testing.T) {
	cfg := Config{BackoffBase: time.Second, BackoffMax: 8 * time.Second, BackoffJitter: 0.25}
	for _, attempt := range []int{-1, 0, 1, 2, 20, 100} {
		for range 100 {
			got := backoff(cfg, attempt)
			if got < cfg.BackoffBase || got > cfg.BackoffMax {
				t.Fatalf("backoff(attempt=%d) = %v, want within [%v, %v]", attempt, got, cfg.BackoffBase, cfg.BackoffMax)
			}
		}
	}
}

func TestBackoffDoublesWithoutJitterAndCaps(t *testing.T) {
	cfg := Config{BackoffBase: time.Second, BackoffMax: 5 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for attempt, expected := range want {
		if got := backoff(cfg, attempt); got != expected {
			t.Errorf("backoff(attempt=%d) = %v, want %v", attempt, got, expected)
		}
	}
}
