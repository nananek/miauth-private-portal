package config

import "strings"

// SplitRSSFeedURLs parses value (RSS_FEED_URLS's raw comma-separated,
// optionally "|username"-suffixed wire format — see splitRSSFeedURLs's
// own doc comment in config.go for the exact separator rules) into
// parallel url/optional-username slices. Exported for
// internal/webadmin's Web UI RSS feed screen (Issue #136 Phase 4), so
// that package can parse the current RSS_FEED_URLS value for display
// and editing without reimplementing this format's comma/pipe-separator
// rules — plan-136's own instruction ("reuse internal/config's existing
// splitRSSFeedURLs-equivalent parsing, don't reimplement it").
// An empty value returns (nil, nil): no feeds configured.
func SplitRSSFeedURLs(value string) (urls []string, usernames []*string) {
	return splitRSSFeedURLs(map[string]string{KeyRSSFeedURLs: value}, KeyRSSFeedURLs)
}

// JoinRSSFeedURLs is SplitRSSFeedURLs's inverse: renders parallel url/
// optional-username slices back into RSS_FEED_URLS's raw wire format
// ("url" per entry, or "url|username" when that entry's username is
// non-nil and non-empty), comma-joined. len(urls) and len(usernames)
// must match — a caller-programming-error panic (an out-of-range index)
// otherwise, the same "caller's responsibility to keep these parallel"
// contract splitRSSFeedURLs's own return already establishes. An empty
// urls returns "" — callers that need to distinguish "no feeds" from
// "unset the override entirely" must do so themselves (ValidateKeyValue
// rejects an empty Set value; see internal/webadmin.Service.
// RemoveRSSFeed for how the one caller of this function that can reach
// zero entries handles it).
func JoinRSSFeedURLs(urls []string, usernames []*string) string {
	entries := make([]string, len(urls))
	for i, u := range urls {
		if usernames[i] != nil && *usernames[i] != "" {
			entries[i] = u + "|" + *usernames[i]
		} else {
			entries[i] = u
		}
	}
	return strings.Join(entries, ",")
}
