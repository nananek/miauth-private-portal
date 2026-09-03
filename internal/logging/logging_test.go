package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

const secretValue = "super-secret-marker-value"

func TestRedaction_KnownSensitiveKeys(t *testing.T) {
	cases := []string{
		"authorization", "Authorization", "AUTHORIZATION",
		"cookie", "Cookie",
		"set-cookie",
		"i",
		"token", "access_token", "api_key", "apikey",
		"state", "miauth_state",
		"password", "secret", "body", "prompt", "mail_body",
	}

	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			logger := New(&buf, Config{Format: "json", Level: "info"})
			logger.Info("event", key, secretValue)

			out := buf.String()
			if strings.Contains(out, secretValue) {
				t.Errorf("key %q: raw secret leaked into log: %s", key, out)
			}
			if !strings.Contains(out, redactedValue) {
				t.Errorf("key %q: expected %q marker in log: %s", key, redactedValue, out)
			}
		})
	}
}

func TestRedaction_AppliesInsideNestedGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})
	logger.Info("event", slog.Group("request", slog.String("authorization", secretValue)))

	out := buf.String()
	if strings.Contains(out, secretValue) {
		t.Errorf("secret leaked from nested group: %s", out)
	}
	if !strings.Contains(out, redactedValue) {
		t.Errorf("expected redaction marker in nested group output: %s", out)
	}
}

func TestRedaction_NonSensitiveKeysPassThrough(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})
	logger.Info("event", "route", "/healthz", "status", 200)

	out := buf.String()
	if !strings.Contains(out, "/healthz") {
		t.Errorf("expected non-sensitive value to pass through: %s", out)
	}
}

func TestNew_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "warn"})

	logger.Info("should be filtered")
	if buf.Len() != 0 {
		t.Errorf("expected no output at info level when configured for warn, got: %s", buf.String())
	}

	logger.Warn("should appear")
	if buf.Len() == 0 {
		t.Error("expected output at warn level")
	}
}

func TestNew_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "json", Level: "info"})
	logger.Info("event", "key", "value")

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("expected valid JSON output, got error %v for: %s", err, buf.String())
	}
}

func TestNew_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Config{Format: "text", Level: "info"})
	logger.Info("event", "key", "value")

	if err := json.Unmarshal(buf.Bytes(), &map[string]any{}); err == nil {
		t.Errorf("expected non-JSON text output, but it parsed as JSON: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "msg=event") {
		t.Errorf("expected slog text format, got: %s", buf.String())
	}
}

func TestRequestIDContext_RoundTrip(t *testing.T) {
	ctx := WithRequestID(t.Context(), "abc123")
	if got := RequestIDFromContext(ctx); got != "abc123" {
		t.Errorf("RequestIDFromContext() = %q, want abc123", got)
	}
}

func TestRequestIDFromContext_EmptyWhenUnset(t *testing.T) {
	if got := RequestIDFromContext(t.Context()); got != "" {
		t.Errorf("RequestIDFromContext() = %q, want empty string", got)
	}
}
