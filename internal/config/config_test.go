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

// validAuthEnv is the minimal set of Auth env vars every test that
// expects Load to succeed must supply, now that LOCAL_ORIGIN and
// IDENTITY_ORIGIN are required in every environment.
func validAuthEnv() map[string]string {
	return map[string]string{
		KeyLocalOrigin:    "https://portal.example",
		KeyIdentityOrigin: "https://misskey.example",
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
		Auth: AuthConfig{
			LocalOrigin:         "https://portal.example",
			IdentityOrigin:      "https://misskey.example",
			UpstreamHTTPTimeout: 10 * time.Second,
			OwnerUsername:       "owner",
		},
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
	path := writeTempEnvFile(t, "APP_ENV=development\nHTTP_PORT=9090\nLOCAL_ORIGIN=https://portal.example\nIDENTITY_ORIGIN=https://misskey.example\n")
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
	path := writeTempEnvFile(t, "APP_ENV=development\nHTTP_PORT=9090\nLOCAL_ORIGIN=https://portal.example\nIDENTITY_ORIGIN=https://misskey.example\n")
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
		Auth: AuthConfig{
			LocalOrigin:         "https://portal.example",
			IdentityOrigin:      "https://misskey.example",
			UpstreamHTTPTimeout: 10 * time.Second,
			OwnerUsername:       "owner",
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

func TestLoad_MissingLocalOrigin(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:         "development",
		KeyIdentityOrigin: "https://misskey.example",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyLocalOrigin) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyLocalOrigin)
	}
}

func TestLoad_MissingIdentityOrigin(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:      "development",
		KeyLocalOrigin: "https://portal.example",
	})})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyIdentityOrigin) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyIdentityOrigin)
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
			KeyAppEnv:         "development",
			KeyLocalOrigin:    v,
			KeyIdentityOrigin: "https://misskey.example",
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
		KeyAppEnv:         "development",
		KeyLocalOrigin:    "http://localhost:8080",
		KeyIdentityOrigin: "https://misskey.example",
	})})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoad_OriginTrimsTrailingSlash backs the fix for a LOCAL_ORIGIN or
// IDENTITY_ORIGIN configured with a single trailing slash: it must be
// normalized to a bare origin, not stored as-is, or every URL this
// service builds by naively concatenating "origin + /path" ends up with
// a double slash.
func TestLoad_OriginTrimsTrailingSlash(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:         "development",
		KeyLocalOrigin:    "https://portal.example/",
		KeyIdentityOrigin: "https://misskey.example/",
	})})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.LocalOrigin != "https://portal.example" {
		t.Errorf("LocalOrigin = %q, want no trailing slash", cfg.Auth.LocalOrigin)
	}
	if cfg.Auth.IdentityOrigin != "https://misskey.example" {
		t.Errorf("IdentityOrigin = %q, want no trailing slash", cfg.Auth.IdentityOrigin)
	}
}

func TestLoad_ProductionRejectsHTTPOrigin(t *testing.T) {
	_, err := Load(LoadOptions{Getenv: getenvFromMap(map[string]string{
		KeyAppEnv:         "production",
		KeyLogFormat:      "json",
		KeyLogLevel:       "info",
		KeyLocalOrigin:    "http://portal.example",
		KeyIdentityOrigin: "https://misskey.example",
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

func TestLoad_UpstreamHTTPTimeoutDefaultAndValidation(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv: "development",
	}))})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.UpstreamHTTPTimeout != 10*time.Second {
		t.Errorf("UpstreamHTTPTimeout = %v, want 10s", cfg.Auth.UpstreamHTTPTimeout)
	}

	_, err = Load(LoadOptions{Getenv: getenvFromMap(mergeMaps(validAuthEnv(), map[string]string{
		KeyAppEnv:              "development",
		KeyUpstreamHTTPTimeout: "0s",
	}))})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), KeyUpstreamHTTPTimeout) {
		t.Errorf("error %q does not mention %s", err.Error(), KeyUpstreamHTTPTimeout)
	}
}

// TestConfig_Redacted_NeverExposesAllowedMisskeyUserID backs Issue #5's
// acceptance criteria: the allowlisted upstream user ID must never reach
// a log line or response, so Redacted must show only whether it is set.
func TestConfig_Redacted_NeverExposesAllowedMisskeyUserID(t *testing.T) {
	const secretUserID = "super-secret-upstream-user-id"
	cfg := Config{
		Auth: AuthConfig{AllowedMisskeyUserID: secretUserID},
	}
	redacted := cfg.Redacted()
	if strings.Contains(redacted[KeyAllowedMisskeyUserID], secretUserID) {
		t.Errorf("Redacted()[%s] leaked the raw value: %q", KeyAllowedMisskeyUserID, redacted[KeyAllowedMisskeyUserID])
	}
	if redacted[KeyAllowedMisskeyUserID] != "<set>" {
		t.Errorf("Redacted()[%s] = %q, want <set>", KeyAllowedMisskeyUserID, redacted[KeyAllowedMisskeyUserID])
	}

	unsetRedacted := Config{}.Redacted()
	if unsetRedacted[KeyAllowedMisskeyUserID] != "<unset>" {
		t.Errorf("Redacted()[%s] = %q, want <unset>", KeyAllowedMisskeyUserID, unsetRedacted[KeyAllowedMisskeyUserID])
	}
}
