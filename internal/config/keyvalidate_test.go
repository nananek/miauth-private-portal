package config

import "testing"

func TestValidateKeyValue_UnknownKey(t *testing.T) {
	err := ValidateKeyValue("NOT_A_REAL_KEY", "1")
	if err == nil {
		t.Fatal("ValidateKeyValue(unknown key) = nil, want an error")
	}
}

func TestValidateKeyValue_EmptyValueRejected(t *testing.T) {
	err := ValidateKeyValue(KeyJobsPollInterval, "")
	if err == nil {
		t.Fatal("ValidateKeyValue(empty value) = nil, want an error directing the caller to config unset")
	}
}

// TestValidateKeyValue_ValidValuesForEveryDBEligibleKey is a smoke test:
// one plausible legal value per Tier A key must validate cleanly, so a
// future parse() change that starts rejecting a previously-fine value
// fails here before it reaches miauthctl config set.
func TestValidateKeyValue_ValidValuesForEveryDBEligibleKey(t *testing.T) {
	valid := map[string]string{
		KeyJobsPollInterval:                          "2s",
		KeyJobsClaimBatchSize:                        "10",
		KeyJobsLeaseDuration:                         "30s",
		KeyJobsLeaseRenewMargin:                      "10s",
		KeyJobsMaxAttempts:                           "8",
		KeyJobsBackoffBase:                           "1s",
		KeyJobsBackoffMax:                            "10m",
		KeyJobsMaxConcurrent:                         "4",
		KeyJobsShutdownGrace:                         "15s",
		KeyRSSPollInterval:                           "15m",
		KeyRSSFeedURLs:                               "https://example.com/feed.xml",
		KeyRSSSummaryMaxChars:                        "4000",
		KeyIMAPPollInterval:                          "5m",
		KeyIMAPFetchTimeout:                          "30s",
		KeyIMAPMaxMessageBytes:                       "1048576",
		KeyIMAPSnippetMaxChars:                       "2000",
		KeyIMAPStoreFullBody:                         "true",
		KeyIMAPFullBodyMaxChars:                      "20000",
		KeyLLMModel:                                  "gpt-4o-mini",
		KeyLLMTimeout:                                "30s",
		KeyLLMMaxOutputTokens:                        "1024",
		KeyLLMThreadContextMaxMessages:               "20",
		KeyLLMThreadContextMaxChars:                  "8000",
		KeyLLMClassificationModel:                    "gpt-4o-mini",
		KeyLLMClassificationMaxOutputTokens:          "1024",
		KeyLLMClassificationThreadContextMaxMessages: "20",
		KeyLLMClassificationThreadContextMaxChars:    "8000",
		KeyOpenWebUICatalogSyncInterval:              "10m",
		KeyOpenWebUIWebSearchEnabled:                 "true",
	}

	for _, key := range DBEligibleKeys() {
		v, ok := valid[key]
		if !ok {
			t.Errorf("no test value provided for db-eligible key %s", key)
			continue
		}
		if err := ValidateKeyValue(key, v); err != nil {
			t.Errorf("ValidateKeyValue(%s, %q) = %v, want nil", key, v, err)
		}
	}
}

func TestValidateKeyValue_BoundsRejected(t *testing.T) {
	cases := []struct {
		key   string
		value string
	}{
		{KeyJobsMaxAttempts, "0"},          // below jobsMaxAttemptsMin
		{KeyJobsMaxAttempts, "101"},        // above jobsMaxAttemptsMax
		{KeyJobsClaimBatchSize, "not-int"}, // not an integer at all
		{KeyIMAPStoreFullBody, "not-a-bool"},
		{KeyJobsPollInterval, "not-a-duration"},
		{KeyJobsPollInterval, "-5s"}, // not positive
	}
	for _, c := range cases {
		if err := ValidateKeyValue(c.key, c.value); err == nil {
			t.Errorf("ValidateKeyValue(%s, %q) = nil, want an error", c.key, c.value)
		}
	}
}

// TestValidateKeyValue_RSSFeedURLsShapeCheck documents the deliberate
// scope of RSS_FEED_URLS's single-key validation: an http (not https)
// entry passes here even though RSS_ALLOW_INSECURE_HTTP defaults to
// false, because internal/ingest/safehttp's client independently
// enforces that policy at actual fetch time (see ValidateKeyValue's own
// doc comment). Only "not a URL at all" is rejected here.
func TestValidateKeyValue_RSSFeedURLsShapeCheck(t *testing.T) {
	if err := ValidateKeyValue(KeyRSSFeedURLs, "https://example.com/feed.xml"); err != nil {
		t.Errorf("https feed URL rejected: %v", err)
	}
	if err := ValidateKeyValue(KeyRSSFeedURLs, "http://example.com/feed.xml"); err != nil {
		t.Errorf("http feed URL rejected at set-time (should defer to safehttp at fetch time): %v", err)
	}
	if err := ValidateKeyValue(KeyRSSFeedURLs, "not-a-url"); err == nil {
		t.Error("ValidateKeyValue(RSS_FEED_URLS, \"not-a-url\") = nil, want an error")
	}
}

func TestValidateKeyValue_SecretKeyFormatStillValidatesButIsNotAClassificationCheck(t *testing.T) {
	// ValidateKeyValue only checks syntax; whether a key may be stored
	// in app_config at all is IsSecretKey's job, checked separately by
	// the CLI before ever calling Set. LLM_API_KEY has no format
	// constraint of its own, so any non-empty value passes here.
	if err := ValidateKeyValue(KeyLLMAPIKey, "sk-example"); err != nil {
		t.Errorf("ValidateKeyValue(secret key, valid shape) = %v, want nil", err)
	}
	if !IsSecretKey(KeyLLMAPIKey) {
		t.Error("IsSecretKey(LLM_API_KEY) = false, want true")
	}
}
