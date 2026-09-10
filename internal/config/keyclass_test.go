package config

import "testing"

// TestClassOf_EveryKnownKeyClassifiedExactlyOnce is the mechanical check
// backing ADR-0006's "3分類" design: secretKeys and dbEligibleKeys must
// be disjoint (ClassOf's precedence would otherwise silently hide a
// mistake), and every ClassOf result must be one of the three declared
// constants.
func TestClassOf_EveryKnownKeyClassifiedExactlyOnce(t *testing.T) {
	for _, key := range KnownKeys() {
		isSecret := secretKeys[key]
		isDBEligible := dbEligibleKeys[key]
		if isSecret && isDBEligible {
			t.Errorf("%s is classified as both secret and db-eligible", key)
		}
		switch ClassOf(key) {
		case ClassBootstrapOnly, ClassSecret, ClassDBEligible:
		default:
			t.Errorf("ClassOf(%s) returned an unrecognized KeyClass", key)
		}
	}
}

func TestIsSecretKey_MatchesADR0005D10(t *testing.T) {
	want := map[string]bool{
		KeyLLMAPIKey:       true,
		KeyOpenWebUIAPIKey: true,
		KeyIMAPUsername:    true,
		KeyIMAPPassword:    true,
	}
	for _, key := range KnownKeys() {
		if got, w := IsSecretKey(key), want[key]; got != w {
			t.Errorf("IsSecretKey(%s) = %v, want %v", key, got, w)
		}
	}
}

// TestIsDBEligibleKey_MatchesTierA pins down the exact Tier A key set
// plan-76 §2-3 confirmed, so an accidental addition or removal in
// dbEligibleKeys fails a test instead of silently changing which keys
// ADR-0006's DB overlay accepts.
//
// RSS_ENABLED is deliberately absent even though an earlier draft of
// plan-76's Tier A table listed it alongside RSS_POLL_INTERVAL/
// RSS_FEED_URLS/RSS_SUMMARY_MAX_CHARS/IMAP_POLL_INTERVAL: the plan's own
// final decision (§0-4, §2-3 Tier B, §7 point 4) names RSS_ENABLED as
// one of the five *_ENABLED subsystem-construction flags explicitly kept
// out of scope. This test encodes that resolution; see the PR1 handoff
// note for the discrepancy this settles.
func TestIsDBEligibleKey_MatchesTierA(t *testing.T) {
	want := []string{
		KeyJobsPollInterval,
		KeyJobsClaimBatchSize,
		KeyJobsLeaseDuration,
		KeyJobsLeaseRenewMargin,
		KeyJobsMaxAttempts,
		KeyJobsBackoffBase,
		KeyJobsBackoffMax,
		KeyJobsMaxConcurrent,
		KeyJobsShutdownGrace,
		KeyRSSPollInterval,
		KeyRSSFeedURLs,
		KeyRSSSummaryMaxChars,
		KeyIMAPPollInterval,
		KeyIMAPFetchTimeout,
		KeyIMAPMaxMessageBytes,
		KeyIMAPSnippetMaxChars,
		KeyIMAPStoreFullBody,
		KeyIMAPFullBodyMaxChars,
		KeyLLMModel,
		KeyLLMTimeout,
		KeyLLMMaxOutputTokens,
		KeyLLMThreadContextMaxMessages,
		KeyLLMThreadContextMaxChars,
		KeyLLMClassificationModel,
		KeyLLMClassificationMaxOutputTokens,
		KeyLLMClassificationThreadContextMaxMessages,
		KeyLLMClassificationThreadContextMaxChars,
		KeyOpenWebUICatalogSyncInterval,
		KeyOpenWebUIWebSearchEnabled,
	}
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}

	got := DBEligibleKeys()
	if len(got) != len(want) {
		t.Fatalf("DBEligibleKeys() has %d keys, want %d: got=%v", len(got), len(want), got)
	}
	for _, k := range got {
		if !wantSet[k] {
			t.Errorf("DBEligibleKeys() unexpectedly includes %s", k)
		}
	}
	for _, k := range want {
		if !IsDBEligibleKey(k) {
			t.Errorf("IsDBEligibleKey(%s) = false, want true", k)
		}
	}

	// Explicitly pin the Tier B / bootstrap-only keys most likely to be
	// mistaken for Tier A, so a future refactor that widens
	// dbEligibleKeys by accident fails loudly here.
	for _, key := range []string{
		KeyJobsWorkerID,
		KeyRSSEnabled,
		KeyIMAPEnabled,
		KeyLLMEnabled,
		KeyLLMClassificationEnabled,
		KeyOpenWebUIEnabled,
		KeyOpenWebUIGenerationEnabled,
		KeyOpenWebUIBaseURL,
		KeyOpenWebUIAllowedOrigins,
		KeyIMAPHost,
		KeyIMAPPort,
		KeyIMAPTLSMode,
		KeyLLMBaseURL,
		// RSS_FILTER_SCRIPT_PATH (Issue #135) is executable logic loaded
		// and compiled once at startup, not a scalar this service can
		// safely re-read on a cadence — see
		// docs/decisions/0009-rss-item-filtering.md.
		KeyRSSFilterScriptPath,
	} {
		if IsDBEligibleKey(key) {
			t.Errorf("IsDBEligibleKey(%s) = true, want false (Tier B or bootstrap-only)", key)
		}
	}
}

func TestClassOf_UnknownKeyIsBootstrapOnly(t *testing.T) {
	if got := ClassOf("NOT_A_REAL_KEY"); got != ClassBootstrapOnly {
		t.Errorf("ClassOf(unknown) = %v, want ClassBootstrapOnly", got)
	}
}
