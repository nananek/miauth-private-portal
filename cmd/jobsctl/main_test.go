package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func setJobsctlTestEnv(t *testing.T, dbPath string) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("LOCAL_ORIGIN", "https://portal.example")
	t.Setenv("IDENTITY_ORIGIN", "https://misskey.example")
	t.Setenv("DB_PATH", dbPath)
}

func seedJobsctlJob(t *testing.T, dbPath, id, jobType string, state domain.JobState, lastError *string) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Jobs.Enqueue(t.Context(), domain.Job{
		ID: id, JobType: jobType, Payload: `{"private":"payload"}`, PayloadVersion: 1,
		State: domain.JobPending, NextRunAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if state == domain.JobDead {
		if err := db.Jobs.Kill(t.Context(), id, deref(lastError), now); err != nil {
			t.Fatal(err)
		}
	} else if state == domain.JobFailed {
		if err := db.Jobs.Fail(t.Context(), id, deref(lastError), now); err != nil {
			t.Fatal(err)
		}
	} else if state == domain.JobSucceeded {
		if err := db.Jobs.Succeed(t.Context(), id, now); err != nil {
			t.Fatal(err)
		}
	}
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func TestRunListFiltersAndTruncatesError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	setJobsctlTestEnv(t, dbPath)
	longError := strings.Repeat("x", 100)
	seedJobsctlJob(t, dbPath, "dead-job", "classify", domain.JobDead, &longError)
	seedJobsctlJob(t, dbPath, "pending-job", "classify", domain.JobPending, nil)

	var out bytes.Buffer
	if err := run([]string{"list", "--state=dead", "--type=classify", "--limit=5"}, &out); err != nil {
		t.Fatalf("run(list): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "dead-job") || strings.Contains(got, "pending-job") {
		t.Fatalf("list output = %q, want only dead-job", got)
	}
	if strings.Contains(got, longError) || !strings.Contains(got, "…") {
		t.Fatalf("list output did not abbreviate last_error: %q", got)
	}
}

func TestRunShowPrintsAllJobFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	setJobsctlTestEnv(t, dbPath)
	seedJobsctlJob(t, dbPath, "show-job", "summarize", domain.JobPending, nil)

	var out bytes.Buffer
	if err := run([]string{"show", "show-job"}, &out); err != nil {
		t.Fatalf("run(show): %v", err)
	}
	for _, want := range []string{`"ID": "show-job"`, `"JobType": "summarize"`, `"Payload":`, `"CreatedAt":`, `"UpdatedAt":`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show output %q does not contain %q", out.String(), want)
		}
	}
}

func TestRunRetryRequeuesTerminalJob(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	setJobsctlTestEnv(t, dbPath)
	lastError := "provider unavailable"
	seedJobsctlJob(t, dbPath, "retry-job", "summarize", domain.JobDead, &lastError)

	var out bytes.Buffer
	if err := run([]string{"retry", "retry-job"}, &out); err != nil {
		t.Fatalf("run(retry): %v", err)
	}
	if !strings.Contains(out.String(), "requeued retry-job") {
		t.Errorf("retry output = %q", out.String())
	}

	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	job, err := db.Jobs.Get(t.Context(), "retry-job")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != domain.JobPending || job.LastError != nil {
		t.Fatalf("requeued job = %+v, want pending with cleared last error", job)
	}
}

func TestRunRetryRejectsNonTerminalJob(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	setJobsctlTestEnv(t, dbPath)
	seedJobsctlJob(t, dbPath, "pending-job", "test", domain.JobPending, nil)

	err := run([]string{"retry", "pending-job"}, &bytes.Buffer{})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("run(retry pending) error = %v, want ErrConflict", err)
	}
}

func TestRunValidatesArguments(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(nil) error = %v, want usage", err)
	}
	if err := run([]string{"unknown"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("run(unknown) error = %v", err)
	}
}
