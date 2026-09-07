package config

// ValidateKeyValue validates a single known key's raw string value the
// same way Load parses and bounds-checks it, without requiring every
// other key to be present. It is the one place miauthctl config
// set/validate (a single key=value pair, not a whole file — see below)
// and the startup DB-overlay auto-seed path (ADR-0006 §2-4) both call,
// so a value accepted here is guaranteed to parse the same way inside a
// full Load.
//
// An empty value is rejected the same way Load rejects an environment
// variable set to an empty string (see Load's own comment): ambiguous
// between "reset to the bootstrap default" and "no change", so a caller
// wanting that must use the key's own Unset path instead of Set with "".
//
// This function validates one explicit key=value pair, the same shape a
// CLI argument or an app_config row's value column takes; it does not
// implement "config validate --file", which should instead reuse
// loadConfigFile/parse directly so a whole file's empty-value-means-
// default semantics match Load's own file-loading behavior exactly.
//
// RSS_FEED_URLS is the one db-eligible key whose real per-entry
// URL-format check (validateRSSFeedURLs) lives behind Validate's
// RSS_ENABLED gate rather than in parse() itself. It is checked here as
// an unconditional shape-only pass (any absolute http(s) URL, regardless
// of RSS_ALLOW_INSECURE_HTTP): internal/ingest/safehttp's own client
// independently enforces the http/https policy at fetch time (defense in
// depth), so this function only needs to catch "not a URL at all" before
// a value ever reaches app_config.
func ValidateKeyValue(key, value string) error {
	if !isKnownKey(key) {
		return &ValidationError{Fields: []FieldError{{Key: key, Reason: "unknown config key"}}}
	}
	if value == "" {
		return &ValidationError{Fields: []FieldError{{Key: key, Reason: `must not be empty; use "config unset" to remove this key instead of setting it to an empty string`}}}
	}

	_, errs := parse(map[string]string{key: value})
	var matched []FieldError
	for _, e := range errs {
		if e.Key == key {
			matched = append(matched, e)
		}
	}

	if key == KeyRSSFeedURLs {
		list := splitOptionalURLList(map[string]string{key: value}, key)
		validateRSSFeedURLs(&matched, key, list, true)
	}

	if len(matched) > 0 {
		return &ValidationError{Fields: matched}
	}
	return nil
}
