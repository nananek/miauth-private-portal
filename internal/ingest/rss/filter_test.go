package rss

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest"
)

func writeScript(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "filter.star")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func TestLoadFilter_MissingFile(t *testing.T) {
	_, err := LoadFilter(filepath.Join(t.TempDir(), "does-not-exist.star"))
	if err == nil {
		t.Fatal("LoadFilter() = nil error, want an error for a missing file")
	}
}

func TestLoadFilter_SyntaxError(t *testing.T) {
	path := writeScript(t, "def matches(title, body, source_host, source_uri, provenance_url):\n    if True\n        return True\n")
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("LoadFilter() = nil error, want an error for a syntax error")
	}
}

func TestLoadFilter_NoMatchesFunction(t *testing.T) {
	path := writeScript(t, "def other(title, body, source_host, source_uri, provenance_url):\n    return False\n")
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("LoadFilter() = nil error, want an error when the script defines no top-level matches function")
	}
}

func TestLoadFilter_WrongArity(t *testing.T) {
	path := writeScript(t, "def matches(title, body):\n    return False\n")
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("LoadFilter() = nil error, want an error when matches() has the wrong number of parameters")
	}
}

func TestLoadFilter_ValidScript_ShouldExcludeMatchesAndKeeps(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    return "spam" in title.lower()
`)
	f, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("LoadFilter: %v", err)
	}
	source := domain.ExternalSource{URI: "https://example.com/feed.xml"}

	exclude, err := f.shouldExclude(source, ingest.FetchedItem{Title: "This is SPAM"})
	if err != nil {
		t.Fatalf("shouldExclude: %v", err)
	}
	if !exclude {
		t.Error("shouldExclude() = false, want true for a title containing the banned keyword")
	}

	exclude, err = f.shouldExclude(source, ingest.FetchedItem{Title: "Ordinary article"})
	if err != nil {
		t.Fatalf("shouldExclude: %v", err)
	}
	if exclude {
		t.Error("shouldExclude() = true, want false for a title without the banned keyword")
	}
}

func TestFilter_ShouldExclude_SourceHostAndURIPassedThrough(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    return source_host == "note.com" and "spam" in title
`)
	f, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("LoadFilter: %v", err)
	}

	noteSource := domain.ExternalSource{URI: "https://note.com/rss"}
	exclude, err := f.shouldExclude(noteSource, ingest.FetchedItem{Title: "spam post"})
	if err != nil {
		t.Fatalf("shouldExclude: %v", err)
	}
	if !exclude {
		t.Error("shouldExclude() = false, want true: source_host must be derived from source.URI")
	}

	otherSource := domain.ExternalSource{URI: "https://example.com/rss"}
	exclude, err = f.shouldExclude(otherSource, ingest.FetchedItem{Title: "spam post"})
	if err != nil {
		t.Fatalf("shouldExclude: %v", err)
	}
	if exclude {
		t.Error("shouldExclude() = true, want false: the per-feed rule must not fire for a different source_host")
	}
}

func TestFilter_ShouldExclude_NonBoolReturnIsError(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    return "not a bool"
`)
	f, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("LoadFilter: %v", err)
	}
	_, err = f.shouldExclude(domain.ExternalSource{URI: "https://example.com/feed.xml"}, ingest.FetchedItem{Title: "x"})
	if err == nil {
		t.Fatal("shouldExclude() = nil error, want an error when matches() does not return a bool")
	}
}

// TestLoadFilter_UndefinedNameRejected proves the script runs no
// unresolved reference: go.starlark.net's static resolver rejects an
// undefined name at compile time (LoadFilter fails closed at startup),
// rather than letting it reach a per-item runtime error.
func TestLoadFilter_UndefinedNameRejected(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    return undefined_name
`)
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("LoadFilter() = nil error, want an error when the script references an undefined name")
	}
}

// TestLoadFilter_NoFilesystemOrNetworkBuiltins proves no open/exec/os/
// http-style builtin is exposed to the script: referencing one is an
// undefined name, caught by the static resolver at LoadFilter time (the
// same "no unresolved reference" mechanism TestLoadFilter_UndefinedNameRejected
// pins), never a builtin a script could actually call.
func TestLoadFilter_NoFilesystemOrNetworkBuiltins(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    open("/etc/passwd")
    return False
`)
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("LoadFilter() = nil error, want an error: no open() builtin should be exposed to the script")
	}
}

// TestFilter_ShouldExclude_FailBuiltinIsError exercises a genuine
// runtime (not load-time/resolver) failure: fail() is a predeclared
// Starlark builtin (so it is not caught by the static resolver the way
// an undefined name is), and calling it must surface as a plain error
// from shouldExclude, never a panic.
func TestFilter_ShouldExclude_FailBuiltinIsError(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    fail("boom")
`)
	f, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("LoadFilter: %v", err)
	}
	_, err = f.shouldExclude(domain.ExternalSource{URI: "https://example.com/feed.xml"}, ingest.FetchedItem{Title: "x"})
	if err == nil {
		t.Fatal("shouldExclude() = nil error, want an error when the script calls fail()")
	}
}

func TestLoadFilter_LoadStatementRejected(t *testing.T) {
	path := writeScript(t, `
load("other.star", "helper")

def matches(title, body, source_host, source_uri, provenance_url):
    return False
`)
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("LoadFilter() = nil error, want an error: load() must be rejected (no Load callback is configured)")
	}
}

// TestFilter_ShouldExclude_StepBoundExceeded proves the execution bound
// wired into shouldExclude is real, not just documented: a script that
// tries to iterate far more than any legitimate filter rule would (a
// large but still bounded `for` loop — go.starlark.net's default dialect
// disallows `while` and recursion, so this is the only way a script can
// try to run long) is cut off by SetMaxExecutionSteps rather than
// running to completion or hanging the test.
func TestFilter_ShouldExclude_StepBoundExceeded(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    total = 0
    for i in range(100000000):
        total += i
    return total > 0
`)
	f, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("LoadFilter: %v", err)
	}
	_, err = f.shouldExclude(domain.ExternalSource{URI: "https://example.com/feed.xml"}, ingest.FetchedItem{Title: "x"})
	if err == nil {
		t.Fatal("shouldExclude() = nil error, want an error: the loop's iteration count must exceed filterMaxSteps")
	}
	if !strings.Contains(err.Error(), "too many steps") && !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("shouldExclude() error = %v, want it to indicate the execution-step bound was hit", err)
	}
}

func TestFilter_ShouldExclude_ProvenanceURLNilBecomesEmptyString(t *testing.T) {
	path := writeScript(t, `
def matches(title, body, source_host, source_uri, provenance_url):
    return provenance_url == ""
`)
	f, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("LoadFilter: %v", err)
	}
	exclude, err := f.shouldExclude(domain.ExternalSource{URI: "https://example.com/feed.xml"}, ingest.FetchedItem{Title: "x"})
	if err != nil {
		t.Fatalf("shouldExclude: %v", err)
	}
	if !exclude {
		t.Error("shouldExclude() = false, want true: a nil ProvenanceURL must reach the script as an empty string")
	}
}
