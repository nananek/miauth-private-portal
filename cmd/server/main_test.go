package main

import (
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
)

func TestJobsConfigFrom(t *testing.T) {
	in := config.JobsConfig{
		WorkerID:            "worker-a",
		PollInterval:        time.Second,
		ClaimBatchSize:      3,
		LeaseDuration:       30 * time.Second,
		LeaseRenewMargin:    10 * time.Second,
		MaxAttempts:         7,
		BackoffBase:         2 * time.Second,
		BackoffMax:          time.Minute,
		MaxConcurrentJobs:   2,
		ShutdownGracePeriod: 15 * time.Second,
	}
	got := jobsConfigFrom(in)
	if got.WorkerID != in.WorkerID || got.PollInterval != in.PollInterval ||
		got.ClaimBatchSize != in.ClaimBatchSize || got.LeaseDuration != in.LeaseDuration ||
		got.LeaseRenewMargin != in.LeaseRenewMargin || got.MaxAttempts != in.MaxAttempts ||
		got.BackoffBase != in.BackoffBase || got.BackoffMax != in.BackoffMax ||
		got.MaxConcurrentJobs != in.MaxConcurrentJobs || got.ShutdownGracePeriod != in.ShutdownGracePeriod {
		t.Fatalf("jobsConfigFrom(%+v) = %+v", in, got)
	}
	if got.BackoffJitter != 0.2 {
		t.Errorf("BackoffJitter = %v, want fixed 0.2", got.BackoffJitter)
	}
}
