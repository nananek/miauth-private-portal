package config

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func getenvFromMap(m map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func recordingGetenv(m map[string]string, queried *[]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		*queried = append(*queried, key)
		v, ok := m[key]
		return v, ok
	}
}

func writeTempEnvFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp env file: %v", err)
	}
	return path
}

func TestLoad_MissingRequiredAppEnv(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(nil)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), KeyAppEnv) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyAppEnv)
	}
}

func TestLoad_DefaultsWhenOnlyAppEnvSet(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv: "development",
	})})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := Config{
		Env: EnvDevelopment,
		HTTP: HTTPConfig{
			Host:                "0.0.0.0",
			Port:                8080,
			ReadTimeout:         5 * time.Second,
			ReadHeaderTimeout:   5 * time.Second,
			WriteTimeout:        10 * time.Second,
			IdleTimeout:         60 * time.Second,
			MaxRequestBodyBytes: 1 << 20,
			ShutdownGracePeriod: 15 * time.Second,
		},
		Log: LogConfig{Level: "info", Format: "text"},
		DB: DBConfig{
			Path:         "./data/portal.db",
			BusyTimeout:  5 * time.Second,
			MaxOpenConns: 8,
		},
	}

	if *cfg != want {
		t.Errorf("got %+v, want %+v", *cfg, want)
	}
}

func TestLoad_MissingConfigFileIsNotError(t *testing.T) {
	_, err := Load(LoadOptions{
		ConfigFilePath: filepath.Join(t.TempDir(), "does-not-exist.env"),
		Getenv:         getenvFromMap(map[string]string{KeyAppEnv: "development"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_FileValuesApplied(t *testing.T) {
	path := writeTempEnvFile(t, "APP_ENV=development\nHTTP_PORT=9090\n")
	cfg, err := Load(LoadOptions{
		ConfigFilePath: path,
		Getenv:         getenvFromMap(nil),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTP.Port != 9090 {
		t.Errorf("HTTP.Port = %d, want 9090", cfg.HTTP.Port)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	path := writeTempEnvFile(t, "APP_ENV=development\nHTTP_PORT=9090\n")
	cfg, err := Load(LoadOptions{
		ConfigFilePath: path,
		Getenv:         getenvFromMap(map[string]string{KeyHTTPPort: "7070"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTP.Port != 7070 {
		t.Errorf("HTTP.Port = %d, want 7070 (env should win over file)", cfg.HTTP.Port)
	}
}

func TestLoad_EmptyEnvOverrideFailsClosedInsteadOfSilentlyDiscardingFileValue(t *testing.T) {
	path := writeTempEnvFile(t, "APP_ENV=development\nHTTP_PORT=9090\n")
	_, err := Load(LoadOptions{
		ConfigFilePath: path,
		Getenv:         getenvFromMap(map[string]string{KeyHTTPPort: ""}),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyHTTPPort) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyHTTPPort)
	}
}

func TestLoad_UnknownKeyInFileFailsFast(t *testing.T) {
	path := writeTempEnvFile(t, "APP_ENV=development\nFOO_BAR=baz\n")
	_, err := Load(LoadOptions{
		ConfigFilePath: path,
		Getenv:         getenvFromMap(nil),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "FOO_BAR") || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error %q does not report FOO_BAR as unknown", err.Error())
	}
}

func TestLoad_MultipleUnknownKeysInFileAreAllReportedDeterministically(t *testing.T) {
	path := writeTempEnvFile(t, "APP_ENV=development\nZZZ_UNKNOWN=1\nAAA_UNKNOWN=2\n")

	for range 5 {
		_, err := Load(LoadOptions{
			ConfigFilePath: path,
			Getenv:         getenvFromMap(nil),
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "AAA_UNKNOWN") || !strings.Contains(err.Error(), "ZZZ_UNKNOWN") {
			t.Fatalf("error %q does not report both unknown keys", err.Error())
		}
		if strings.Index(err.Error(), "AAA_UNKNOWN") > strings.Index(err.Error(), "ZZZ_UNKNOWN") {
			t.Fatalf("error %q does not report unknown keys in a stable sorted order", err.Error())
		}
	}
}

func TestLoad_InvalidPortRange(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:   "development",
		KeyHTTPPort: "70000",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyHTTPPort) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyHTTPPort)
	}
}

func TestLoad_InvalidDBBusyTimeout(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:          "development",
		KeyDBBusyTimeoutMS: "0",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyDBBusyTimeoutMS) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyDBBusyTimeoutMS)
	}
}

func TestLoad_InvalidDBMaxOpenConns(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:         "development",
		KeyDBMaxOpenConns: "0",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyDBMaxOpenConns) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyDBMaxOpenConns)
	}
}

func TestLoad_DBPathOverride(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv: "development",
		KeyDBPath: "/data/custom.db",
	})})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DB.Path != "/data/custom.db" {
		t.Errorf("DB.Path = %q, want /data/custom.db", cfg.DB.Path)
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:   "development",
		KeyLogLevel: "verbose",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLogLevel) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLogLevel)
	}
}

func TestLoad_ProductionRequiresJSONLog(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv: "production",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLogFormat) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLogFormat)
	}
}

func TestLoad_ProductionRejectsDebugLog(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:    "production",
		KeyLogFormat: "json",
		KeyLogLevel:  "debug",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLogLevel) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLogLevel)
	}
}

func TestLoad_ProductionAcceptsValidSettings(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:    "production",
		KeyLogFormat: "json",
		KeyLogLevel:  "info",
	})})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_ErrorMessagesNeverContainRawValues(t *testing.T) {
	const secret = "super-secret-marker-value"
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:   "development",
		KeyHTTPPort: secret,
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaked raw value: %q", err.Error())
	}
}

func TestLoad_OnlyKnownEnvironmentVariablesAreQueried(t *testing.T) {
	var queried []string
	_, err := Load(LoadOptions{Getenv: recordingGetenv(map[string]string{
		KeyAppEnv: "development",
	}, &queried)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sort.Strings(queried)
	want := KnownKeys()
	sort.Strings(want)

	if len(queried) != len(want) {
		t.Fatalf("queried %v, want exactly %v", queried, want)
	}
	for i := range want {
		if queried[i] != want[i] {
			t.Errorf("queried[%d] = %q, want %q", i, queried[i], want[i])
		}
	}
}

func TestKnownKeys_MatchesSchemaSet(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range KnownKeys() {
		if seen[k] {
			t.Errorf("duplicate key in KnownKeys: %s", k)
		}
		seen[k] = true
		if !isKnownKey(k) {
			t.Errorf("KnownKeys() returned %s, but isKnownKey does not recognize it", k)
		}
	}
}

func TestConfig_Redacted(t *testing.T) {
	cfg := Config{
		Env: EnvDevelopment,
		HTTP: HTTPConfig{
			Host: "0.0.0.0", Port: 8080,
			ReadTimeout: 5 * time.Second, ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
			MaxRequestBodyBytes: 1 << 20, ShutdownGracePeriod: 15 * time.Second,
		},
		Log: LogConfig{Level: "info", Format: "text"},
	}

	redacted := cfg.Redacted()
	if redacted[KeyAppEnv] != "development" {
		t.Errorf("Redacted()[%s] = %q, want development", KeyAppEnv, redacted[KeyAppEnv])
	}
	if redacted[KeyHTTPPort] != "8080" {
		t.Errorf("Redacted()[%s] = %q, want 8080", KeyHTTPPort, redacted[KeyHTTPPort])
	}
}

func TestConfig_ValidateRejectsHandBuiltConfigWithOutOfBoundsFields(t *testing.T) {
	cfg := Config{
		Env: EnvDevelopment,
		HTTP: HTTPConfig{
			Host:                "0.0.0.0",
			Port:                0,
			ReadTimeout:         -1 * time.Second,
			ReadHeaderTimeout:   5 * time.Second,
			WriteTimeout:        10 * time.Second,
			IdleTimeout:         60 * time.Second,
			MaxRequestBodyBytes: -5,
			ShutdownGracePeriod: 15 * time.Second,
		},
		Log: LogConfig{Level: "info", Format: "text"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for out-of-bounds hand-built Config, got nil")
	}
	for _, key := range []string{KeyHTTPPort, KeyHTTPReadTimeout, KeyHTTPMaxBodyBytes} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err.Error(), key)
		}
	}
}

func TestConfig_ValidateAcceptsHandBuiltConfigWithinBounds(t *testing.T) {
	cfg := Config{
		Env: EnvDevelopment,
		HTTP: HTTPConfig{
			Host:                "0.0.0.0",
			Port:                8080,
			ReadTimeout:         5 * time.Second,
			ReadHeaderTimeout:   5 * time.Second,
			WriteTimeout:        10 * time.Second,
			IdleTimeout:         60 * time.Second,
			MaxRequestBodyBytes: 1 << 20,
			ShutdownGracePeriod: 15 * time.Second,
		},
		Log: LogConfig{Level: "info", Format: "text"},
		DB: DBConfig{
			Path:         "./data/portal.db",
			BusyTimeout:  5 * time.Second,
			MaxOpenConns: 8,
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHTTPConfig_Addr(t *testing.T) {
	h := HTTPConfig{Host: "127.0.0.1", Port: 9090}
	if got, want := h.Addr(), "127.0.0.1:9090"; got != want {
		t.Errorf("Addr() = %q, want %q", got, want)
	}
}
