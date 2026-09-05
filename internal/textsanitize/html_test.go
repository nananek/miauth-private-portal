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
