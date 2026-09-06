package textsanitize

import (
	"strings"
	"testing"
)

func TestStripHTML_RemovesTagsAndDecodesEntities(t *testing.T) {
	got := StripHTML("<p>Hello &amp; <strong>world</strong></p>", 100)
	if got != "Hello & world" {
		t.Errorf("StripHTML = %q, want %q", got, "Hello & world")
	}
}

func TestStripHTML_DropsScriptAndStyleContent(t *testing.T) {
	got := StripHTML(`<p>before</p><script>alert(1)</script><style>.x{color:red}</style><p>after</p>`, 100)
	if strings.Contains(got, "alert") || strings.Contains(got, "color:red") {
		t.Errorf("StripHTML leaked script/style content: %q", got)
	}
	if got != "beforeafter" {
		t.Errorf("StripHTML = %q, want %q", got, "beforeafter")
	}
}

func TestStripHTML_TruncatesToMaxChars(t *testing.T) {
	got := StripHTML("0123456789", 5)
	if got != "01234" {
		t.Errorf("StripHTML = %q, want %q", got, "01234")
	}
}

func TestStripHTML_CollapsesWhitespace(t *testing.T) {
	got := StripHTML("a\n\n  <br>  b   c", 100)
	if got != "a b c" {
		t.Errorf("StripHTML = %q, want %q", got, "a b c")
	}
}

func TestStripHTML_ZeroMaxCharsMeansUnbounded(t *testing.T) {
	got := StripHTML("0123456789", 0)
	if got != "0123456789" {
		t.Errorf("StripHTML = %q, want unbounded %q", got, "0123456789")
	}
}

// TestStripHTML_AttributeBasedXSSPayloadsNeverSurface is Issue #13 AC8's
// explicit XSS regression test: StripHTML discards a tag's attributes
// along with the tag itself (it never selectively keeps some
// attributes), so classic attribute-based payloads — image/svg error
// and load handlers, javascript: URIs — must never leave any trace of
// their dangerous substrings in the sanitized plain-text output that
// ends up in an entry Body.
func TestStripHTML_AttributeBasedXSSPayloadsNeverSurface(t *testing.T) {
	dangerous := []string{"onerror", "onload", "javascript:", "alert("}
	tests := []struct {
		name string
		raw  string
	}{
		{"img onerror", `<img src=x onerror=alert(1)>after`},
		{"svg onload", `<svg onload=alert(1)>after`},
		{"anchor javascript href", `<a href="javascript:alert(1)">click</a>`},
		{"mixed-case script tag", `<ScRiPt>alert(1)</ScRiPt>after`},
		{"body onload attribute", `<body onload=alert(1)>after`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripHTML(tt.raw, 0)
			for _, d := range dangerous {
				if strings.Contains(strings.ToLower(got), d) {
					t.Errorf("StripHTML(%q) = %q, leaked dangerous substring %q", tt.raw, got, d)
				}
			}
		})
	}
}
