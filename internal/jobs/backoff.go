package jobs

import (
	"math/rand/v2"
	"time"
)

func backoff(cfg Config, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 20 {
		attempt = 20
	}

	factor := time.Duration(1 << attempt)
	d := cfg.BackoffMax
	if cfg.BackoffBase <= cfg.BackoffMax/factor {
		d = cfg.BackoffBase * factor
	}
	if d <= 0 || d > cfg.BackoffMax {
		d = cfg.BackoffMax
	}

	jitterRange := float64(d) * cfg.BackoffJitter
	delta := (rand.Float64()*2 - 1) * jitterRange
	withJitter := time.Duration(float64(d) + delta)
	if withJitter < cfg.BackoffBase {
		return cfg.BackoffBase
	}
	if withJitter > cfg.BackoffMax {
		return cfg.BackoffMax
	}
	return withJitter
}
