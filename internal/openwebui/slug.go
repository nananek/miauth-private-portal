// This file is Issue #75's slug-generation rule: the local half of a
// VirtualActor handle (@<slug>@<presentation host>) is no longer an
// operator-supplied config value (OPENWEBUI_MODEL_SLUG, removed by that
// issue's decision to auto-discover every model rather than configure
// one at a time) but a value this service derives once, the first time
// it registers a model, and never recomputes.
package openwebui

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// slugMaxLen mirrors the character-count bound OPENWEBUI_MODEL_SLUG used
// to enforce by config validation: long enough to be readable, short
// enough that a handle stays a handle.
const slugMaxLen = 32

// slugHashLen is how many hex characters of a SHA-256 digest
// GenerateActorSlug uses, both as the whole-candidate fallback and as a
// disambiguating suffix. Collisions within this space are astronomically
// unlikely for the number of models any one workspace will ever hold, and
// GenerateActorSlug does not loop looking for a shorter one.
const slugHashLen = 8

// slugInvalidChars matches every run of bytes a normalized slug candidate
// may not contain: anything outside lowercase ASCII letters, digits, and
// underscore — the same character set OPENWEBUI_MODEL_SLUG's own pattern
// enforced before this file existed.
var slugInvalidChars = regexp.MustCompile(`[^a-z0-9_]+`)

// GenerateActorSlug derives a new model's actor_slug from its remote
// display name, falling back to a hash of its opaque external id when the
// name normalizes to nothing usable (empty, or entirely outside
// [a-z0-9_] — a non-Latin display name is the common case), and
// disambiguating with a further hash suffix when the result collides with
// something reserved reports true for.
//
// reserved is asked about the *lowercased, already-normalized* candidate,
// never about externalModelID or displayName directly. Callers combine
// whatever needs to be reserved in one workspace: other models' already-
// assigned slugs, the owner's own username, and the fixed
// "assistant"/"system" presentation names — never this model's own
// existing slug, because GenerateActorSlug is called only once, when a
// model is registered for the first time; the roadmap's stable-handle
// requirement is exactly that re-syncing or renaming a model afterward
// must never call this function again for it.
func GenerateActorSlug(externalModelID, displayName string, reserved func(candidate string) bool) string {
	candidate := normalizeSlugCandidate(displayName)
	if candidate == "" {
		candidate = hashSlug(externalModelID)
	}
	if !reserved(candidate) {
		return candidate
	}

	suffixed := withHashSuffix(candidate, externalModelID)
	if !reserved(suffixed) {
		return suffixed
	}

	// A second collision means reserved() rejected a candidate that
	// already embeds an 8-hex-character hash of externalModelID — for
	// that to happen twice, two different calls would need the same
	// externalModelID (impossible: GenerateActorSlug runs once per newly
	// discovered id) or an adversarial reserved(). Folding displayName
	// into the seed as well guarantees a third, distinct candidate rather
	// than looping.
	return hashSlug(externalModelID + "\x00" + displayName)
}

// normalizeSlugCandidate lowercases displayName, replaces every run of
// characters outside [a-z0-9_] with a single underscore, trims leading
// and trailing underscores (so "GPT-OSS 20B" becomes "gpt_oss_20b" rather
// than picking up a stray leading/trailing one from punctuation at either
// end), and bounds the result to slugMaxLen.
func normalizeSlugCandidate(displayName string) string {
	lower := strings.ToLower(displayName)
	normalized := slugInvalidChars.ReplaceAllString(lower, "_")
	normalized = strings.Trim(normalized, "_")
	if len(normalized) > slugMaxLen {
		normalized = strings.Trim(normalized[:slugMaxLen], "_")
	}
	return normalized
}

// hashSlug returns the first slugHashLen hex characters of seed's
// SHA-256 digest — always a valid slug on its own (hex digits are a
// subset of [a-z0-9_]) and well under slugMaxLen.
func hashSlug(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:slugHashLen]
}

// withHashSuffix appends "_" plus an 8-hex-character hash of seed to
// candidate, truncating candidate first if the combination would exceed
// slugMaxLen.
func withHashSuffix(candidate, seed string) string {
	suffix := "_" + hashSlug(seed)
	maxBase := slugMaxLen - len(suffix)
	if len(candidate) > maxBase {
		candidate = candidate[:maxBase]
	}
	return candidate + suffix
}
