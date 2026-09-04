package rss

import (
	"html"
	"strings"
)

// stripHTML renders raw feed markup (an RSS <description> or Atom
// <summary>/<content>, which real-world feeds commonly fill with HTML)
// as plain text: tags and their attributes are discarded, <script> and
// <style> element content is discarded along with the tags themselves,
// entities are decoded, and runs of whitespace collapse to a single
// space. AGENTS.md requires external content is treated as untrusted
// and never executed; this is a small hand-written scanner rather than
// golang.org/x/net/html because that dependency's only use here would be
// this one strip-to-plain-text operation, and AGENTS.md requires a
// concrete reason before adding a new dependency — the standard
// library's "html" package (entity decoding) plus a tag scanner is
// enough for "never render markup, always plain text".
//
// This is not a full HTML parser: malformed/unclosed tags are handled
// leniently (an unterminated "<" simply discards the remainder of the
// input) rather than matching a browser's error-recovery behavior
// exactly, which is acceptable here because the output is never
// re-parsed as HTML by anything downstream.
func stripHTML(raw string, maxChars int) string {
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
