package config

import (
	"strings"
	"testing"
)

func TestParseEnvFile_ValidFile(t *testing.T) {
	input := `# comment
APP_ENV=development

HTTP_HOST="0.0.0.0"
HTTP_PORT='8080'
LOG_LEVEL=info
`
	values, err := ParseEnvFile(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"APP_ENV":   "development",
		"HTTP_HOST": "0.0.0.0",
		"HTTP_PORT": "8080",
		"LOG_LEVEL": "info",
	}
	if len(values) != len(want) {
		t.Fatalf("got %d values, want %d: %v", len(values), len(want), values)
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("values[%q] = %q, want %q", k, values[k], v)
		}
	}
}

func TestParseEnvFile_MalformedLineIncludesLineNumber(t *testing.T) {
	input := "APP_ENV=development\nNOT_A_VALID_LINE\n"
	_, err := ParseEnvFile(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %q does not name line 2", err.Error())
	}
}

func TestParseEnvFile_DuplicateKeyIsError(t *testing.T) {
	input := "APP_ENV=development\nAPP_ENV=production\n"
	_, err := ParseEnvFile(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate key APP_ENV") {
		t.Errorf("error %q does not mention duplicate key", err.Error())
	}
}

func TestParseEnvFile_InvalidKeyFormat(t *testing.T) {
	cases := []string{
		"lowercase_key=value\n",
		"1STARTS_WITH_DIGIT=value\n",
		"HAS-DASH=value\n",
	}
	for _, input := range cases {
		_, err := ParseEnvFile(strings.NewReader(input))
		if err == nil {
			t.Errorf("input %q: expected error, got nil", input)
		}
	}
}

func TestParseEnvFile_EmptyFile(t *testing.T) {
	values, err := ParseEnvFile(strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("expected no values, got %v", values)
	}
}
