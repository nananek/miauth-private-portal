package openwebui

import (
	"strings"
	"testing"
)

func neverReserved(string) bool { return false }

func TestGenerateActorSlug_NormalizesDisplayName(t *testing.T) {
	tests := map[string]struct {
		displayName string
		want        string
	}{
		"lowercases":                  {"GPT-OSS", "gpt_oss"},
		"collapses punctuation runs":  {"GPT--OSS!!20B", "gpt_oss_20b"},
		"trims leading and trailing":  {" .GPT-OSS. ", "gpt_oss"},
		"keeps digits and underscore": {"model_20b", "model_20b"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := GenerateActorSlug("ext-id", tt.displayName, neverReserved)
			if got != tt.want {
				t.Errorf("GenerateActorSlug(%q) = %q, want %q", tt.displayName, got, tt.want)
			}
		})
	}
}

func TestGenerateActorSlug_TruncatesToMaxLen(t *testing.T) {
	long := strings.Repeat("a", 40)
	got := GenerateActorSlug("ext-id", long, neverReserved)
	if len(got) != slugMaxLen {
		t.Errorf("len(GenerateActorSlug(...)) = %d, want %d", len(got), slugMaxLen)
	}
	if got != strings.Repeat("a", slugMaxLen) {
		t.Errorf("GenerateActorSlug(long) = %q, want the first %d a's", got, slugMaxLen)
	}
}

// TestGenerateActorSlug_EmptyOrNonLatinDisplayNameFallsBackToHash covers
// the two ways a display name normalizes to nothing usable: genuinely
// empty, and a non-Latin name whose every character falls outside
// [a-z0-9_] and therefore collapses to a single trimmed-away underscore
// run.
func TestGenerateActorSlug_EmptyOrNonLatinDisplayNameFallsBackToHash(t *testing.T) {
	want := hashSlug("ext-id")
	for name, displayName := range map[string]string{
		"empty":     "",
		"non-latin": "日本語モデル",
		"symbols":   "!!!",
	} {
		t.Run(name, func(t *testing.T) {
			got := GenerateActorSlug("ext-id", displayName, neverReserved)
			if got != want {
				t.Errorf("GenerateActorSlug(%q) = %q, want the hash fallback %q", displayName, got, want)
			}
			if len(got) != slugHashLen {
				t.Errorf("len(GenerateActorSlug(%q)) = %d, want %d", displayName, len(got), slugHashLen)
			}
		})
	}
}

func TestGenerateActorSlug_DifferentExternalIDsHashDifferently(t *testing.T) {
	a := GenerateActorSlug("model-a", "", neverReserved)
	b := GenerateActorSlug("model-b", "", neverReserved)
	if a == b {
		t.Errorf("GenerateActorSlug for two different external ids both produced %q", a)
	}
}

// TestGenerateActorSlug_CollisionAppendsHashSuffix covers the reserved
// path: a candidate reserved() rejects gets a hash suffix appended, and
// that suffixed form is what gets returned once reserved() accepts it.
func TestGenerateActorSlug_CollisionAppendsHashSuffix(t *testing.T) {
	reserved := func(candidate string) bool { return candidate == "model" }
	got := GenerateActorSlug("ext-id", "Model", reserved)
	want := "model_" + hashSlug("ext-id")
	if got != want {
		t.Errorf("GenerateActorSlug with a reserved candidate = %q, want %q", got, want)
	}
	if reserved(got) {
		t.Fatalf("test setup bug: the suffixed candidate %q is itself reserved", got)
	}
}

// TestGenerateActorSlug_ReservedNamesGetSuffixed pins the specific
// reserved names a caller is expected to guard: the owner's username and
// the fixed assistant/system presentation names, matching the collision
// set OPENWEBUI_MODEL_SLUG's own validation used to enforce.
func TestGenerateActorSlug_ReservedNamesGetSuffixed(t *testing.T) {
	ownerUsername := "nekono"
	reserved := func(candidate string) bool {
		return candidate == ownerUsername || candidate == "assistant" || candidate == "system"
	}
	for _, displayName := range []string{"Nekono", "assistant", "system"} {
		t.Run(displayName, func(t *testing.T) {
			got := GenerateActorSlug("ext-id", displayName, reserved)
			if reserved(got) {
				t.Errorf("GenerateActorSlug(%q) = %q, still reserved", displayName, got)
			}
			if !strings.HasSuffix(got, "_"+hashSlug("ext-id")) {
				t.Errorf("GenerateActorSlug(%q) = %q, want a hash-suffixed form", displayName, got)
			}
		})
	}
}

// TestGenerateActorSlug_CaseInsensitiveCollisionIsTheCallersJob pins that
// GenerateActorSlug itself does no case folding when consulting reserved:
// it always asks about its own lowercase candidate, so a caller wanting
// "Owner"/"owner" to collide (as OPENWEBUI_MODEL_SLUG's validation once
// did) folds case inside its own reserved function.
func TestGenerateActorSlug_CaseInsensitiveCollisionIsTheCallersJob(t *testing.T) {
	reserved := func(candidate string) bool { return strings.EqualFold(candidate, "Owner") }
	got := GenerateActorSlug("ext-id", "Owner", reserved)
	if got == "owner" {
		t.Fatal("GenerateActorSlug returned the reserved candidate unchanged")
	}
}
