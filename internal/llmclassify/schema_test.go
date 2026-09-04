package llmclassify

import (
	"errors"
	"strings"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestParseAndNormalize_FullyValidOutput(t *testing.T) {
	content := `{
		"subject": "Go generics",
		"field": "programming",
		"keywords": ["go", "generics"],
		"tags": ["go", "learning"],
		"summary": "Explored Go generics constraints.",
		"openQuestions": ["how do type sets interact with methods?"],
		"unresolved": true,
		"relatedEntryIds": ["e1", "e2"],
		"learningTargets": ["type inference"],
		"priority": "medium",
		"notebookCandidate": true,
		"reviewCandidate": false,
		"confidence": 0.75
	}`

	got, err := ParseAndNormalize(content)
	if err != nil {
		t.Fatalf("ParseAndNormalize() error = %v, want nil", err)
	}
	if got.Subject == nil || *got.Subject != "Go generics" {
		t.Errorf("Subject = %v, want %q", got.Subject, "Go generics")
	}
	if got.Priority == nil || *got.Priority != "medium" {
		t.Errorf("Priority = %v, want medium", got.Priority)
	}
	if !got.Unresolved || !got.NotebookCandidate || got.ReviewCandidate {
		t.Errorf("flags = (%v, %v, %v), want (true, true, false)", got.Unresolved, got.NotebookCandidate, got.ReviewCandidate)
	}
	if got.Confidence != 0.75 {
		t.Errorf("Confidence = %v, want 0.75", got.Confidence)
	}
	if len(got.RelatedEntryIDs) != 2 {
		t.Errorf("RelatedEntryIDs = %v, want 2 entries", got.RelatedEntryIDs)
	}
}

func TestParseAndNormalize_EmptyObjectSucceeds(t *testing.T) {
	got, err := ParseAndNormalize(`{}`)
	if err != nil {
		t.Fatalf("ParseAndNormalize() error = %v, want nil", err)
	}
	if got.Priority != nil {
		t.Errorf("Priority = %v, want nil", got.Priority)
	}
	if got.Unresolved || got.NotebookCandidate || got.ReviewCandidate {
		t.Errorf("flags = (%v, %v, %v), want all false", got.Unresolved, got.NotebookCandidate, got.ReviewCandidate)
	}
}

func TestParseAndNormalize_UnknownPriorityNormalizesToNull(t *testing.T) {
	got, err := ParseAndNormalize(`{"priority": "urgent"}`)
	if err != nil {
		t.Fatalf("ParseAndNormalize() error = %v, want nil (normalize, not reject)", err)
	}
	if got.Priority != nil {
		t.Errorf("Priority = %v, want nil for an unknown enum value", got.Priority)
	}
}

func TestParseAndNormalize_OversizeArraysAreTruncated(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantLen int
		extract func(Fields) []string
	}{
		{"keywords", `{"keywords":["a","b","c","d","e","f","g","h","i","j","k","l"]}`, maxKeywords, func(f Fields) []string { return f.Keywords }},
		{"tags", `{"tags":["a","b","c","d","e","f","g","h","i","j"]}`, maxTags, func(f Fields) []string { return f.Tags }},
		{"openQuestions", `{"openQuestions":["a","b","c","d","e","f","g"]}`, maxOpenQuestions, func(f Fields) []string { return f.OpenQuestions }},
		{"learningTargets", `{"learningTargets":["a","b","c","d","e","f","g"]}`, maxLearningTargets, func(f Fields) []string { return f.LearningTargets }},
		{"relatedEntryIds", `{"relatedEntryIds":["a","b","c","d","e","f","g"]}`, maxRelatedEntries, func(f Fields) []string { return f.RelatedEntryIDs }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAndNormalize(tt.content)
			if err != nil {
				t.Fatalf("ParseAndNormalize() error = %v, want nil (truncate, not reject)", err)
			}
			if len(tt.extract(got)) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(tt.extract(got)), tt.wantLen)
			}
		})
	}
}

func TestParseAndNormalize_OversizeStringIsTruncated(t *testing.T) {
	long := strings.Repeat("a", maxStringFieldRunes+50)
	content := `{"subject": "` + long + `"}`

	got, err := ParseAndNormalize(content)
	if err != nil {
		t.Fatalf("ParseAndNormalize() error = %v, want nil", err)
	}
	if got.Subject == nil || len([]rune(*got.Subject)) != maxStringFieldRunes {
		t.Errorf("len(Subject) = %d, want %d", len([]rune(*got.Subject)), maxStringFieldRunes)
	}
}

func TestParseAndNormalize_ConfidenceIsClamped(t *testing.T) {
	tests := []struct {
		content string
		want    float64
	}{
		{`{"confidence": 5}`, 1},
		{`{"confidence": -3}`, 0},
		{`{"confidence": 0.5}`, 0.5},
	}
	for _, tt := range tests {
		got, err := ParseAndNormalize(tt.content)
		if err != nil {
			t.Fatalf("ParseAndNormalize(%q) error = %v, want nil", tt.content, err)
		}
		if got.Confidence != tt.want {
			t.Errorf("ParseAndNormalize(%q).Confidence = %v, want %v", tt.content, got.Confidence, tt.want)
		}
	}
}

func TestParseAndNormalize_InvalidJSONSyntaxIsMalformed(t *testing.T) {
	_, err := ParseAndNormalize(`{not-json`)
	if !errors.Is(err, ErrMalformedOutput) {
		t.Errorf("ParseAndNormalize() error = %v, want ErrMalformedOutput", err)
	}
}

func TestParseAndNormalize_NonObjectTopLevelIsMalformed(t *testing.T) {
	tests := []string{`"just a string"`, `[1,2,3]`, `42`, ``}
	for _, content := range tests {
		t.Run(content, func(t *testing.T) {
			_, err := ParseAndNormalize(content)
			if !errors.Is(err, ErrMalformedOutput) {
				t.Errorf("ParseAndNormalize(%q) error = %v, want ErrMalformedOutput", content, err)
			}
		})
	}
}

func TestValidateRelatedIDs_ExcludesSelfDuplicatesAndHallucinations(t *testing.T) {
	candidates := []domain.Entry{{ID: "c1"}, {ID: "c2"}}
	targetID := "target"

	got := validateRelatedIDs([]string{"c1", "c1", "target", "hallucinated", "c2"}, candidates, targetID)

	want := []string{"c1", "c2"}
	if len(got) != len(want) {
		t.Fatalf("validateRelatedIDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("validateRelatedIDs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidateRelatedIDs_AllInvalidYieldsEmptyNotError(t *testing.T) {
	candidates := []domain.Entry{{ID: "c1"}}
	got := validateRelatedIDs([]string{"nonexistent", "also-nonexistent"}, candidates, "target")
	if len(got) != 0 {
		t.Errorf("validateRelatedIDs() = %v, want empty", got)
	}
}

func TestValidateRelatedIDs_EmptyCandidatesYieldsEmpty(t *testing.T) {
	got := validateRelatedIDs([]string{"c1"}, nil, "target")
	if len(got) != 0 {
		t.Errorf("validateRelatedIDs() = %v, want empty", got)
	}
}
