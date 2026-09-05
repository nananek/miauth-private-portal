// Package textsanitize renders untrusted markup (an RSS <description>/
// Atom <summary>, or Issue #12's IMAP mail body) as plain text: tags and
// their attributes are discarded, <script> and <style> element content is
// discarded along with the tags themselves, entities are decoded, and
// runs of whitespace collapse to a single space. AGENTS.md requires
// external content — feeds and mail alike — is treated as untrusted and
// never executed; every caller of StripHTML shares this one
// implementation rather than each adapter maintaining its own copy of
// security-relevant sanitization logic, which would otherwise be free to
// drift out of sync with each other.
package textsanitize

import (
	"html"
	"strings"
)

// StripHTML is not a full HTML parser: malformed/unclosed tags are
// handled leniently (an unterminated "<" simply discards the remainder of
// the input) rather than matching a browser's error-recovery behavior
// exactly, which is acceptable here because the output is never
// re-parsed as HTML by anything downstream. golang.org/x/net/html is
// deliberately not used because this package's only need is "never
// render markup, always plain text": the standard library's "html"
// package (entity decoding) plus this tag scanner is enough, and
// AGENTS.md requires a concrete reason before adding a new dependency.
func StripHTML(raw string, maxChars int) string {
	var sb strings.Builder
	sb.Grow(len(raw))

	skipUntil := "" // "</script>" or "</style>" while inside that element
	i := 0
	n := len(raw)
	for i < n {
		c := raw[i]
		if c == '<' {
			end := strings.IndexByte(raw[i:], '>')
			if end < 0 {
				break
			}
			tag := raw[i : i+end+1]
			lower := strings.ToLower(tag)
			switch {
			case skipUntil != "":
				if strings.Contains(lower, skipUntil) {
					skipUntil = ""
				}
			case strings.HasPrefix(lower, "<script"):
				skipUntil = "</script>"
			case strings.HasPrefix(lower, "<style"):
				skipUntil = "</style>"
			}
			i += end + 1
			continue
		}
		if skipUntil == "" {
			sb.WriteByte(c)
		}
		i++
	}

	text := html.UnescapeString(sb.String())
	text = strings.Join(strings.Fields(text), " ")

	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) > maxChars {
		runes = runes[:maxChars]
	}
	return string(runes)
}
