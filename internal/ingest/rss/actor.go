// This file is Issue #77 PR4's (ADR-0008) design-A host/username
// derivation for one RSS-kind domain.ExternalSource, computed once when
// the source is first registered (cmd/server) and never recomputed
// afterward — the same "compute once at registration, never resync"
// shape Issue #75's GenerateActorSlug (internal/openwebui/slug.go)
// already established for Open WebUI model handles. The two are
// intentionally not shared code: same shape, deliberately duplicated
// rather than making RSS ingestion depend on the unrelated Open WebUI
// feature package (AGENTS.md's narrow use-case package boundaries).
package rss

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// usernameMaxLen and usernameHashLen mirror GenerateActorSlug's own
// slugMaxLen/slugHashLen constants — see that function's doc comment for
// why these particular values.
const (
	usernameMaxLen  = 32
	usernameHashLen = 8
)

// usernameInvalidChars matches every run of bytes a normalized username
// candidate may not contain — internal/config's ownerUsernamePattern
// (Misskey's own username character set: ASCII letters, digits,
// underscore) named as a disallow-list instead of an allow-list so it
// can drive a ReplaceAllString collapse.
var usernameInvalidChars = regexp.MustCompile(`[^A-Za-z0-9]+`)

// HostFromFeedURL returns feedURL's hostname (no port, no scheme) —
// ADR-0008's real, per-feed presentation host. It fails only when
// feedURL does not parse as an absolute URL with a host at all, which
// should not happen for a value RSS_FEED_URLS' own validation already
// accepted (internal/config's validateRSSFeedURLs requires an absolute
// http(s) URL).
func HostFromFeedURL(feedURL string) (string, error) {
	u, err := url.Parse(feedURL)
	if err != nil {
		return "", fmt.Errorf("rss: parse feed URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("rss: feed URL %q has no host", feedURL)
	}
	return strings.ToLower(host), nil
}

// DefaultUsername derives a Misskey-username-charset candidate from
// host, for a feed whose RSS_FEED_URLS entry set no explicit
// "|username" suffix (internal/config's parsing — that explicit value
// is used verbatim instead, never passed through here). It disambiguates
// against reserved (every username already registered for the same
// host — ADR-0008's per-host uniqueness) the same way GenerateActorSlug
// disambiguates a model slug: the normalized host first, then that
// candidate plus a hash-of-feedURL suffix, then (astronomically
// unlikely) a hash of host+feedURL outright.
func DefaultUsername(host, feedURL string, reserved func(candidate string) bool) string {
	candidate := normalizeUsernameCandidate(host)
	if candidate == "" {
		candidate = hashUsername(feedURL)
	}
	if !reserved(candidate) {
		return candidate
	}

	suffixed := usernameWithHashSuffix(candidate, feedURL)
	if !reserved(suffixed) {
		return suffixed
	}

	// A second collision means reserved() rejected a candidate that
	// already embeds an 8-hex-character hash of feedURL — for that to
	// happen twice, two different calls would need the same feedURL
	// (impossible: DefaultUsername runs once per newly registered
	// source) or an adversarial reserved(). Folding host into the seed
	// as well guarantees a third, distinct candidate rather than looping.
	return hashUsername(host + "\x00" + feedURL)
}

// normalizeUsernameCandidate lowercases host (DNS names are case-
// insensitive; a deterministic default should not depend on how an
// operator happened to capitalize a URL), collapses every run of
// characters outside the Misskey username charset into a single
// underscore, trims leading/trailing underscores, and bounds the result
// to usernameMaxLen.
func normalizeUsernameCandidate(host string) string {
	lower := strings.ToLower(host)
	normalized := usernameInvalidChars.ReplaceAllString(lower, "_")
	normalized = strings.Trim(normalized, "_")
	if len(normalized) > usernameMaxLen {
		normalized = strings.Trim(normalized[:usernameMaxLen], "_")
	}
	return normalized
}

func hashUsername(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:usernameHashLen]
}

func usernameWithHashSuffix(candidate, seed string) string {
	suffix := "_" + hashUsername(seed)
	maxBase := usernameMaxLen - len(suffix)
	if len(candidate) > maxBase {
		candidate = candidate[:maxBase]
	}
	return candidate + suffix
}
