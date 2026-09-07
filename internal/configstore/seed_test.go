package configstore_test

import (
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/configstore"
)

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.LoadOptions{
		Getenv: func(key string) (string, bool) {
			switch key {
			case config.KeyAppEnv:
				return "development", true
			case config.KeyLocalOrigin:
				return "https://portal.example", true
			case config.KeyJobsMaxAttempts:
				return "9", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("load test config: %v", err)
	}
	return cfg
}

func TestSeed_WritesEveryDBEligibleKeyOnce(t *testing.T) {
	db := newTestDB(t)
	cfg := loadTestConfig(t)

	seeded, err := configstore.Seed(t.Context(), db, cfg, "system-actor", time.Now())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if want := len(config.DBEligibleKeys()); seeded != want {
		t.Fatalf("Seed seeded %d keys, want %d (every db-eligible key)", seeded, want)
	}

	entry, err := db.Config.Get(t.Context(), config.KeyJobsMaxAttempts)
	if err != nil {
		t.Fatalf("Get(%s): %v", config.KeyJobsMaxAttempts, err)
	}
	if entry.Value != "9" {
		t.Errorf("seeded %s = %q, want the env-resolved value 9", config.KeyJobsMaxAttempts, entry.Value)
	}
	if entry.UpdatedBy != "system-actor" {
		t.Errorf("seeded %s UpdatedBy = %q, want system-actor", config.KeyJobsMaxAttempts, entry.UpdatedBy)
	}

	history, err := db.ConfigAudit.ListByKey(t.Context(), config.KeyJobsMaxAttempts)
	if err != nil {
		t.Fatalf("ListByKey: %v", err)
	}
	if len(history) != 1 || history[0].OldValue != nil {
		t.Errorf("seed audit history = %+v, want exactly one entry with a nil OldValue", history)
	}
}

// TestSeed_IsIdempotentAndNeverOverwritesAnExistingRow backs ADR-0006
// §2-4's "既存行は上書きしない": an operator's own value, once set, must
// survive a subsequent Seed call (for example, the next server restart)
// even if the bootstrap value has since changed.
func TestSeed_IsIdempotentAndNeverOverwritesAnExistingRow(t *testing.T) {
	db := newTestDB(t)
	cfg := loadTestConfig(t)

	if _, err := configstore.Seed(t.Context(), db, cfg, "system-actor", time.Now()); err != nil {
		t.Fatalf("first Seed: %v", err)
	}

	if err := db.Config.Set(t.Context(), config.KeyJobsMaxAttempts, "42", 1, "owner-1", time.Now()); err != nil {
		t.Fatalf("operator Set: %v", err)
	}

	seeded, err := configstore.Seed(t.Context(), db, cfg, "system-actor", time.Now())
	if err != nil {
		t.Fatalf("second Seed: %v", err)
	}
	if seeded != 0 {
		t.Errorf("second Seed seeded %d keys, want 0 (every key already has a row)", seeded)
	}

	entry, err := db.Config.Get(t.Context(), config.KeyJobsMaxAttempts)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry.Value != "42" {
		t.Errorf("Get after re-seed = %q, want the operator's own value 42 preserved", entry.Value)
	}
}
