package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSources_EnvWinsOverFileWinsOverDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("JOBS_MAX_ATTEMPTS=5\nJOBS_POLL_INTERVAL=3s\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	getenv := func(key string) (string, bool) {
		if key == KeyJobsMaxAttempts {
			return "9", true
		}
		return "", false
	}

	sources, err := Sources(LoadOptions{ConfigFilePath: path, Getenv: getenv})
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if sources[KeyJobsMaxAttempts] != SourceEnv {
		t.Errorf("Sources()[%s] = %s, want %s (env overrides file)", KeyJobsMaxAttempts, sources[KeyJobsMaxAttempts], SourceEnv)
	}
	if sources[KeyJobsPollInterval] != SourceFile {
		t.Errorf("Sources()[%s] = %s, want %s", KeyJobsPollInterval, sources[KeyJobsPollInterval], SourceFile)
	}
	if sources[KeyJobsClaimBatchSize] != SourceDefault {
		t.Errorf("Sources()[%s] = %s, want %s", KeyJobsClaimBatchSize, sources[KeyJobsClaimBatchSize], SourceDefault)
	}
}

func TestSources_NoConfigFile(t *testing.T) {
	sources, err := Sources(LoadOptions{Getenv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if sources[KeyJobsMaxAttempts] != SourceDefault {
		t.Errorf("Sources()[%s] = %s, want %s", KeyJobsMaxAttempts, sources[KeyJobsMaxAttempts], SourceDefault)
	}
}
