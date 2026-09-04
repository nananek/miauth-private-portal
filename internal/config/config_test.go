package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// validAuthEnv is the minimal Auth configuration required by tests.
func validAuthEnv() map[string]string {
	return map[string]string{
		KeyLocalOrigin: "https://portal.example",
	}
}

func defaultJobsConfig() JobsConfig {
	return JobsConfig{
		PollInterval:        time.Second,
		ClaimBatchSize:      10,
		LeaseDuration:       30 * time.Second,
		LeaseRenewMargin:    10 * time.Second,
		MaxAttempts:         8,
		BackoffBase:         time.Second,
		BackoffMax:          10 * time.Minute,
		MaxConcurrentJobs:   4,
		ShutdownGracePeriod: 15 * time.Second,
	}
}

func defaultLLMConfig() LLMConfig {
	return LLMConfig{
		Enabled:                                false,
		Timeout:                                30 * time.Second,
		MaxOutputTokens:                        1024,
		ThreadContextMaxMessages:               20,
		ThreadContextMaxChars:                  8000,
		ClassificationEnabled:                  false,
		ClassificationMaxOutputTokens:          1024,
		ClassificationThreadContextMaxMessages: 20,
		ClassificationThreadContextMaxChars:    8000,
	}
}

func defaultRSSConfig() RSSConfig {
	return RSSConfig{
		Enabled:           false,
		PollInterval:      15 * time.Minute,
		FetchTimeout:      15 * time.Second,
		MaxResponseBytes:  2_097_152,
		MaxRedirects:      3,
		SummaryMaxChars:   4000,
		AllowInsecureHTTP: false,
	}
}

func mergeMaps(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

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
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}))})
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
			WriteTimeout:        15 * time.Second,
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
		Auth: AuthConfig{
			LocalOrigin:   "https://portal.example",
			OwnerUsername: "owner",
		},
		Jobs: defaultJobsConfig(),
		LLM:  defaultLLMConfig(),
		RSS:  defaultRSSConfig(),
	}

	// AuthConfig.AriaClientCallbacks is a []string, so Config is no
	// longer comparable with ==; reflect.DeepEqual is the correct
	// replacement (both are nil here, so it also confirms an unset
	// ARIA_CLIENT_CALLBACKS yields nil rather than an empty slice).
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("got %+v, want %+v", *cfg, want)
	}
}

func TestLoad_MissingConfigFileIsNotError(t *testing.T) {
	_, err := Load(LoadOptions{
		ConfigFilePath: filepath.Join(t.TempDir(), "does-not-exist.env"),
		Getenv:         getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{KeyAppEnv: "development"})),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_FileValuesApplied(t *testing.T) {
	path := writeTempEnvFile(t, "APP_ENV=development\nHTTP_PORT=9090\nLOCAL_ORIGIN=https://portal.example\n")
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
	path := writeTempEnvFile(t, "APP_ENV=development\nHTTP_PORT=9090\nLOCAL_ORIGIN=https://portal.example\n")
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

func TestLoad_RemovedUpstreamAuthKeysAreRejected(t *testing.T) {
	for _, key := range []string{"IDENTITY_ORIGIN", "ALLOWED_MISSKEY_USER_ID", "UPSTREAM_HTTP_TIMEOUT"} {
		t.Run(key, func(t *testing.T) {
			path := writeTempEnvFile(t, "APP_ENV=development\nLOCAL_ORIGIN=https://portal.example\n"+key+"=removed\n")
			_, err := Load(LoadOptions{ConfigFilePath: path, Getenv: getenvFromMap(nil)})
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("error = %v, want unknown-key error for %s", err, key)
			}
		})
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
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
		KeyDBPath: "/data/custom.db",
	}))})
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
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "production",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLogFormat) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLogFormat)
	}
}

func TestLoad_ProductionRejectsDebugLog(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:    "production",
		KeyLogFormat: "json",
		KeyLogLevel:  "debug",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLogLevel) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLogLevel)
	}
}

func TestLoad_ProductionAcceptsValidSettings(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:    "production",
		KeyLogFormat: "json",
		KeyLogLevel:  "info",
	}))})
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
	_, err := Load(LoadOptions{Getenv: recordingGetenv(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}), &queried)})
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
			WriteTimeout:        15 * time.Second,
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
			WriteTimeout:        15 * time.Second,
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
		Auth: AuthConfig{
			LocalOrigin:   "https://portal.example",
			OwnerUsername: "owner",
		},
		Jobs: defaultJobsConfig(),
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoad_JobsOverrides(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:               "development",
		KeyJobsWorkerID:         "worker-a",
		KeyJobsPollInterval:     "250ms",
		KeyJobsClaimBatchSize:   "7",
		KeyJobsLeaseDuration:    "45s",
		KeyJobsLeaseRenewMargin: "15s",
		KeyJobsMaxAttempts:      "5",
		KeyJobsBackoffBase:      "2s",
		KeyJobsBackoffMax:       "2m",
		KeyJobsMaxConcurrent:    "3",
		KeyJobsShutdownGrace:    "20s",
	}))})
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := JobsConfig{
		WorkerID:            "worker-a",
		PollInterval:        250 * time.Millisecond,
		ClaimBatchSize:      7,
		LeaseDuration:       45 * time.Second,
		LeaseRenewMargin:    15 * time.Second,
		MaxAttempts:         5,
		BackoffBase:         2 * time.Second,
		BackoffMax:          2 * time.Minute,
		MaxConcurrentJobs:   3,
		ShutdownGracePeriod: 20 * time.Second,
	}
	if !reflect.DeepEqual(cfg.Jobs, want) {
		t.Errorf("Jobs = %+v, want %+v", cfg.Jobs, want)
	}
}

func TestLoad_RejectsInvalidJobsSettings(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		wantKey string
	}{
		{name: "claim batch below range", values: map[string]string{KeyJobsClaimBatchSize: "0"}, wantKey: KeyJobsClaimBatchSize},
		{name: "claim batch above range", values: map[string]string{KeyJobsClaimBatchSize: "101"}, wantKey: KeyJobsClaimBatchSize},
		{name: "poll interval non-positive", values: map[string]string{KeyJobsPollInterval: "0s"}, wantKey: KeyJobsPollInterval},
		{name: "lease duration non-positive", values: map[string]string{KeyJobsLeaseDuration: "0s"}, wantKey: KeyJobsLeaseDuration},
		{name: "renew margin non-positive", values: map[string]string{KeyJobsLeaseRenewMargin: "0s"}, wantKey: KeyJobsLeaseRenewMargin},
		{name: "max attempts below range", values: map[string]string{KeyJobsMaxAttempts: "0"}, wantKey: KeyJobsMaxAttempts},
		{name: "max attempts above range", values: map[string]string{KeyJobsMaxAttempts: "101"}, wantKey: KeyJobsMaxAttempts},
		{name: "concurrency below range", values: map[string]string{KeyJobsMaxConcurrent: "0"}, wantKey: KeyJobsMaxConcurrent},
		{name: "concurrency above range", values: map[string]string{KeyJobsMaxConcurrent: "65"}, wantKey: KeyJobsMaxConcurrent},
		{name: "renew margin equals lease", values: map[string]string{KeyJobsLeaseDuration: "10s", KeyJobsLeaseRenewMargin: "10s"}, wantKey: KeyJobsLeaseRenewMargin},
		{name: "renew margin exceeds lease", values: map[string]string{KeyJobsLeaseDuration: "10s", KeyJobsLeaseRenewMargin: "11s"}, wantKey: KeyJobsLeaseRenewMargin},
		{name: "backoff base non-positive", values: map[string]string{KeyJobsBackoffBase: "0s"}, wantKey: KeyJobsBackoffBase},
		{name: "backoff max non-positive", values: map[string]string{KeyJobsBackoffMax: "0s"}, wantKey: KeyJobsBackoffMax},
		{name: "backoff base exceeds max", values: map[string]string{KeyJobsBackoffBase: "2m", KeyJobsBackoffMax: "1m"}, wantKey: KeyJobsBackoffBase},
		{name: "shutdown grace non-positive", values: map[string]string{KeyJobsShutdownGrace: "0s"}, wantKey: KeyJobsShutdownGrace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := mergeMaps(validAuthEnv(), map[string]string{KeyAppEnv: "development"}, tt.values)
			_, err := Load(LoadOptions{Getenv: getenvFromMap(values)})
			if err == nil {
				t.Fatal("Load() succeeded, want validation error")
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error %q does not mention %s", err.Error(), tt.wantKey)
			}
		})
	}
}

func TestHTTPConfig_Addr(t *testing.T) {
	h := HTTPConfig{Host: "127.0.0.1", Port: 9090}
	if got, want := h.Addr(), "127.0.0.1:9090"; got != want {
		t.Errorf("Addr() = %q, want %q", got, want)
	}
}

func TestLoad_MissingLocalOrigin(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv: "development",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLocalOrigin) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLocalOrigin)
	}
}

func TestLoad_OriginRejectsPathQueryFragmentUserinfo(t *testing.T) {
	cases := []string{
		"https://portal.example/miauth",
		"https://portal.example?x=1",
		"https://portal.example#frag",
		"https://user:pass@portal.example",
	}
	for _, v := range cases {
		_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
			KeyAppEnv:      "development",
			KeyLocalOrigin: v,
		})})
		if err == nil {
			t.Errorf("LOCAL_ORIGIN=%q: expected error, got nil", v)
			continue
		}
		if !strings.Contains(err.Error(), KeyLocalOrigin) {
			t.Errorf("LOCAL_ORIGIN=%q: error %q does not mention %s", v, err.Error(), KeyLocalOrigin)
		}
	}
}

func TestLoad_DevelopmentAcceptsHTTPOrigin(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:      "development",
		KeyLocalOrigin: "http://localhost:8080",
	})})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoad_OriginTrimsTrailingSlash backs the fix for LOCAL_ORIGIN
// configured with trailing slashes: it must be
// normalized to a bare origin, not stored as-is, or every URL this
// service builds by naively concatenating "origin + /path" ends up with
// a double slash.
func TestLoad_OriginTrimsTrailingSlash(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:      "development",
		KeyLocalOrigin: "https://portal.example//",
	})})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.LocalOrigin != "https://portal.example" {
		t.Errorf("LocalOrigin = %q, want no trailing slash", cfg.Auth.LocalOrigin)
	}
}

func TestLoad_ProductionRejectsHTTPOrigin(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:      "production",
		KeyLogFormat:   "json",
		KeyLogLevel:    "info",
		KeyLocalOrigin: "http://portal.example",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLocalOrigin) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLocalOrigin)
	}
}

func TestLoad_AriaClientCallbacks_ParsesAndAcceptsNonHTTPSScheme(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:              "development",
		KeyAriaClientCallbacks: "aria://aria/miauth, https://portal.example/return",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"aria://aria/miauth", "https://portal.example/return"}
	if !reflect.DeepEqual(cfg.Auth.AriaClientCallbacks, want) {
		t.Errorf("AriaClientCallbacks = %v, want %v", cfg.Auth.AriaClientCallbacks, want)
	}
}

func TestLoad_AriaClientCallbacks_RejectsEmptyEntry(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:              "development",
		KeyAriaClientCallbacks: "aria://aria/miauth,,https://portal.example/return",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyAriaClientCallbacks) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyAriaClientCallbacks)
	}
}

func TestLoad_AriaClientCallbacks_RetainsCommaInsideURL(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:              "development",
		KeyAriaClientCallbacks: "https://portal.example/return?ids=1,2,aria://aria/miauth",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"https://portal.example/return?ids=1,2", "aria://aria/miauth"}
	if !reflect.DeepEqual(cfg.Auth.AriaClientCallbacks, want) {
		t.Errorf("AriaClientCallbacks = %v, want %v", cfg.Auth.AriaClientCallbacks, want)
	}
}

func TestLoad_AriaClientCallbacks_RejectsEntryWithoutScheme(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:              "development",
		KeyAriaClientCallbacks: "not-a-url",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyAriaClientCallbacks) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyAriaClientCallbacks)
	}
}

func TestLoad_OwnerUsernameDefault(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.OwnerUsername != "owner" {
		t.Errorf("OwnerUsername = %q, want owner", cfg.Auth.OwnerUsername)
	}
}

func TestLoad_OwnerUsernameOverride(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:        "development",
		KeyOwnerUsername: "nananek",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.OwnerUsername != "nananek" {
		t.Errorf("OwnerUsername = %q, want nananek", cfg.Auth.OwnerUsername)
	}
}

func TestLoad_OwnerUsernameRejectsInvalidCharacters(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:        "development",
		KeyOwnerUsername: "not valid!",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyOwnerUsername) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyOwnerUsername)
	}
}

func TestLoad_OwnerDisplayNameOptional(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.OwnerDisplayName != "" {
		t.Errorf("OwnerDisplayName = %q, want empty (null)", cfg.Auth.OwnerDisplayName)
	}

	cfg, err = Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:           "development",
		KeyOwnerDisplayName: "Nana",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.OwnerDisplayName != "Nana" {
		t.Errorf("OwnerDisplayName = %q, want Nana", cfg.Auth.OwnerDisplayName)
	}
}
func TestLoad_LLMDisabledByDefaultAndDoesNotRequireAnyField(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(cfg.LLM, defaultLLMConfig()) {
		t.Errorf("LLM = %+v, want %+v", cfg.LLM, defaultLLMConfig())
	}
}

func TestLoad_LLMEnabledRequiresBaseURLAndModel(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:     "development",
		KeyLLMEnabled: "true",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLLMBaseURL) || !strings.Contains(err.Error(), KeyLLMModel) {
		t.Errorf("error %q does not mention %s and %s", err.Error(), KeyLLMBaseURL, KeyLLMModel)
	}
}

func TestLoad_LLMEnabledWithRequiredFieldsSucceeds(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:     "development",
		KeyLLMEnabled: "true",
		KeyLLMBaseURL: "https://api.openai.example/v1/",
		KeyLLMAPIKey:  "sk-test",
		KeyLLMModel:   "gpt-test",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.LLM.Enabled {
		t.Error("LLM.Enabled = false, want true")
	}
	if cfg.LLM.BaseURL != "https://api.openai.example/v1" {
		t.Errorf("LLM.BaseURL = %q, want trailing slash trimmed", cfg.LLM.BaseURL)
	}
	if cfg.LLM.APIKey != "sk-test" {
		t.Errorf("LLM.APIKey = %q, want sk-test", cfg.LLM.APIKey)
	}
	if cfg.LLM.Model != "gpt-test" {
		t.Errorf("LLM.Model = %q, want gpt-test", cfg.LLM.Model)
	}
}

func TestLoad_LLMEnabledRejectsNonHTTPSBaseURLInProduction(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:     "production",
		KeyLogFormat:  "json",
		KeyLLMEnabled: "true",
		KeyLLMBaseURL: "http://api.openai.example/v1",
		KeyLLMModel:   "gpt-test",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLLMBaseURL) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLLMBaseURL)
	}
}

func TestLoad_LLMInvalidBoolFailsClosed(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:     "development",
		KeyLLMEnabled: "not-a-bool",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLLMEnabled) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLLMEnabled)
	}
}

func TestLoad_LLMBoundsRejectOutOfRangeValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"max output tokens too low", KeyLLMMaxOutputTokens, "0"},
		{"max output tokens too high", KeyLLMMaxOutputTokens, "999999"},
		{"thread context messages too low", KeyLLMThreadContextMaxMessages, "0"},
		{"thread context chars too low", KeyLLMThreadContextMaxChars, "0"},
		{"timeout not a duration", KeyLLMTimeout, "not-a-duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
				KeyAppEnv: "development",
				tt.key:    tt.val,
			}))})
			if err == nil {
				t.Fatalf("%s=%s: expected error, got nil", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not mention %s", err.Error(), tt.key)
			}
		})
	}
}

func TestLoad_LLMClassificationDisabledByDefaultAndDoesNotRequireAnyField(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLM.ClassificationEnabled {
		t.Error("LLM.ClassificationEnabled = true, want false by default")
	}
}

// TestLoad_LLMClassificationEnabledRequiresBaseURL documents that
// LLM_BASE_URL (shared connection config) is required when
// LLM_CLASSIFICATION_ENABLED=true, even while LLM_ENABLED stays false: an
// operator can run classification without reply generation.
func TestLoad_LLMClassificationEnabledRequiresBaseURL(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:                   "development",
		KeyLLMClassificationEnabled: "true",
		KeyLLMClassificationModel:   "gpt-classify",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLLMBaseURL) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLLMBaseURL)
	}
}

// TestLoad_LLMClassificationEnabledRequiresModelDirectlyOrViaLLMModel
// documents ClassificationModelOrDefault's fallback: classification fails
// closed only when neither LLM_CLASSIFICATION_MODEL nor LLM_MODEL resolves
// to a non-empty model name.
func TestLoad_LLMClassificationEnabledRequiresModelDirectlyOrViaLLMModel(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:                   "development",
		KeyLLMClassificationEnabled: "true",
		KeyLLMBaseURL:               "https://api.openai.example/v1",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLLMClassificationModel) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLLMClassificationModel)
	}
}

func TestLoad_LLMClassificationEnabledFallsBackToLLMModel(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:                   "development",
		KeyLLMClassificationEnabled: "true",
		KeyLLMBaseURL:               "https://api.openai.example/v1",
		KeyLLMModel:                 "gpt-shared",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.LLM.ClassificationModelOrDefault(); got != "gpt-shared" {
		t.Errorf("ClassificationModelOrDefault() = %q, want %q", got, "gpt-shared")
	}
}

func TestLoad_LLMClassificationEnabledWithOwnModelSucceeds(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:                   "development",
		KeyLLMClassificationEnabled: "true",
		KeyLLMBaseURL:               "https://api.openai.example/v1",
		KeyLLMClassificationModel:   "gpt-classify",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLM.ClassificationModel != "gpt-classify" {
		t.Errorf("LLM.ClassificationModel = %q, want %q", cfg.LLM.ClassificationModel, "gpt-classify")
	}
	if got := cfg.LLM.ClassificationModelOrDefault(); got != "gpt-classify" {
		t.Errorf("ClassificationModelOrDefault() = %q, want %q", got, "gpt-classify")
	}
}

func TestLoad_LLMClassificationBoundsRejectOutOfRangeValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"max output tokens too low", KeyLLMClassificationMaxOutputTokens, "0"},
		{"max output tokens too high", KeyLLMClassificationMaxOutputTokens, "999999"},
		{"thread context messages too low", KeyLLMClassificationThreadContextMaxMessages, "0"},
		{"thread context chars too low", KeyLLMClassificationThreadContextMaxChars, "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
				KeyAppEnv: "development",
				tt.key:    tt.val,
			}))})
			if err == nil {
				t.Fatalf("%s=%s: expected error, got nil", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not mention %s", err.Error(), tt.key)
			}
		})
	}
}

// TestConfig_Redacted_NeverExposesLLMAPIKey documents the same secret
// treatment AGENTS.md and Issue #9's acceptance criteria require for
// LLM_API_KEY.
func TestConfig_Redacted_NeverExposesLLMAPIKey(t *testing.T) {
	const secretKey = "sk-super-secret"
	cfg := Config{LLM: LLMConfig{APIKey: secretKey}}
	redacted := cfg.Redacted()
	if strings.Contains(redacted[KeyLLMAPIKey], secretKey) {
		t.Errorf("Redacted()[%s] leaked the raw value: %q", KeyLLMAPIKey, redacted[KeyLLMAPIKey])
	}
	if redacted[KeyLLMAPIKey] != "<set>" {
		t.Errorf("Redacted()[%s] = %q, want <set>", KeyLLMAPIKey, redacted[KeyLLMAPIKey])
	}

	unsetRedacted := Config{}.Redacted()
	if unsetRedacted[KeyLLMAPIKey] != "<unset>" {
		t.Errorf("Redacted()[%s] = %q, want <unset>", KeyLLMAPIKey, unsetRedacted[KeyLLMAPIKey])
	}
}

func TestLoad_RSSDisabledByDefaultAndDoesNotRequireAnyField(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(cfg.RSS, defaultRSSConfig()) {
		t.Errorf("RSS = %+v, want %+v", cfg.RSS, defaultRSSConfig())
	}
}

func TestLoad_RSSEnabledRequiresFeedURLs(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:     "development",
		KeyRSSEnabled: "true",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyRSSFeedURLs) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyRSSFeedURLs)
	}
}

func TestLoad_RSSEnabledWithFeedURLsSucceeds(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:      "development",
		KeyRSSEnabled:  "true",
		KeyRSSFeedURLs: "https://example.com/feed.xml,https://example.org/atom.xml",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"https://example.com/feed.xml", "https://example.org/atom.xml"}
	if !reflect.DeepEqual(cfg.RSS.FeedURLs, want) {
		t.Errorf("RSS.FeedURLs = %v, want %v", cfg.RSS.FeedURLs, want)
	}
}

func TestLoad_RSSFeedURLsRetainsCommasInsideQuery(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:      "development",
		KeyRSSEnabled:  "true",
		KeyRSSFeedURLs: "https://example.com/feed.xml?ids=1,2,3,https://example.org/atom.xml",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"https://example.com/feed.xml?ids=1,2,3", "https://example.org/atom.xml"}
	if !reflect.DeepEqual(cfg.RSS.FeedURLs, want) {
		t.Errorf("RSS.FeedURLs = %v, want %v", cfg.RSS.FeedURLs, want)
	}
}

func TestLoad_RSSEnabledRejectsHTTPFeedURLWithoutAllowInsecureHTTP(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:      "development",
		KeyRSSEnabled:  "true",
		KeyRSSFeedURLs: "http://example.com/feed.xml",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyRSSFeedURLs) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyRSSFeedURLs)
	}
}

func TestLoad_RSSEnabledAllowsHTTPFeedURLWithAllowInsecureHTTP(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:               "development",
		KeyRSSEnabled:           "true",
		KeyRSSFeedURLs:          "http://example.com/feed.xml",
		KeyRSSAllowInsecureHTTP: "true",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.RSS.AllowInsecureHTTP {
		t.Error("RSS.AllowInsecureHTTP = false, want true")
	}
}

func TestLoad_RSSEnabledRejectsInvalidFeedURL(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:      "development",
		KeyRSSEnabled:  "true",
		KeyRSSFeedURLs: "not-a-url",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyRSSFeedURLs) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyRSSFeedURLs)
	}
}

func TestLoad_RSSEnabledRequiresFetchTimeoutLessThanPollInterval(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:          "development",
		KeyRSSEnabled:      "true",
		KeyRSSFeedURLs:     "https://example.com/feed.xml",
		KeyRSSPollInterval: "10s",
		KeyRSSFetchTimeout: "15s",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyRSSFetchTimeout) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyRSSFetchTimeout)
	}
}

func TestLoad_RSSBoundsRejectOutOfRangeValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"max redirects negative", KeyRSSMaxRedirects, "-1"},
		{"max redirects too high", KeyRSSMaxRedirects, "999"},
		{"summary max chars too low", KeyRSSSummaryMaxChars, "0"},
		{"max response bytes too low", KeyRSSMaxResponseBytes, "0"},
		{"poll interval not a duration", KeyRSSPollInterval, "not-a-duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
				KeyAppEnv: "development",
				tt.key:    tt.val,
			}))})
			if err == nil {
				t.Fatalf("%s=%s: expected error, got nil", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not mention %s", err.Error(), tt.key)
			}
		})
	}
}
