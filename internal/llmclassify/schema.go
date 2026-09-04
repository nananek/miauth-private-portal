package llmclassify

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// PromptVersion identifies this file's structured-output schema and
// promptbuilder.go's prompt-assembly behavior, recorded on every
// domain.LLMClassification as PromptVersion so a stored classification
// always names the exact logic that produced it. Bump it (and add a
// table test in schema_test.go) whenever the schema or its validation
// rules change.
const PromptVersion = "classify-v1"

// Bound constants for v1's normalization rules. Like
// internal/llmreply.PolicyVersion's trigger-word lists, these are
// intentionally hardcoded Go constants, not operator-configurable.
const (
	maxKeywords         = 10
	maxOpenQuestions    = 5
	maxLearningTargets  = 5
	maxTags             = 8
	maxRelatedEntries   = 5
	maxStringFieldRunes = 500
)

var allowedPriorities = map[string]bool{"low": true, "medium": true, "high": true}

// rawOutput mirrors the LLM's requested JSON output shape exactly (see
// promptbuilder.go's system prompt). Every field is optional from the
// model's perspective; ParseAndNormalize fills in safe zero values for
// anything omitted rather than rejecting the response.
type rawOutput struct {
	Subject           *string  `json:"subject"`
	Field             *string  `json:"field"`
	Keywords          []string `json:"keywords"`
	Tags              []string `json:"tags"`
	Summary           *string  `json:"summary"`
	OpenQuestions     []string `json:"openQuestions"`
	Unresolved        bool     `json:"unresolved"`
	RelatedEntryIDs   []string `json:"relatedEntryIds"`
	LearningTargets   []string `json:"learningTargets"`
	Priority          *string  `json:"priority"`
	NotebookCandidate bool     `json:"notebookCandidate"`
	ReviewCandidate   bool     `json:"reviewCandidate"`
	Confidence        float64  `json:"confidence"`
}

// Fields is one classification result after ParseAndNormalize: schema
// validation applied, unknown enums normalized to null, oversize
// collections truncated, and confidence clamped. RelatedEntryIDs still
// needs validateRelatedIDs applied against the actual same-thread
// candidate set before it is trustworthy (ParseAndNormalize alone cannot
// know which IDs are real).
type Fields struct {
	Subject           *string
	Field             *string
	Keywords          []string
	Tags              []string
	Summary           *string
	OpenQuestions     []string
	Unresolved        bool
	RelatedEntryIDs   []string
	LearningTargets   []string
	Priority          *string
	NotebookCandidate bool
	ReviewCandidate   bool
	Confidence        float64
}

// structuredOutputJSON is the subset of Fields persisted as opaque JSON
// in domain.LLMClassification.StructuredOutput; Summary, Tags, and
// RelatedEntryIDs all have their own dedicated columns/tables (see
// internal/storage/sqlite/migrations/0005_tags_classifications.sql and
// 0009_llm_classification_flags.sql) and are never duplicated here.
type structuredOutputJSON struct {
	Subject         *string  `json:"subject,omitempty"`
	Field           *string  `json:"field,omitempty"`
	Keywords        []string `json:"keywords,omitempty"`
	OpenQuestions   []string `json:"openQuestions,omitempty"`
	LearningTargets []string `json:"learningTargets,omitempty"`
	Confidence      float64  `json:"confidence"`
}

// StructuredOutputJSON encodes f's opaque-JSON subset for
// domain.LLMClassification.StructuredOutput.
func (f Fields) StructuredOutputJSON() (string, error) {
	b, err := json.Marshal(structuredOutputJSON{
		Subject:         f.Subject,
		Field:           f.Field,
		Keywords:        f.Keywords,
		OpenQuestions:   f.OpenQuestions,
		LearningTargets: f.LearningTargets,
		Confidence:      f.Confidence,
	})
	if err != nil {
		return "", fmt.Errorf("llmclassify: encode structured output: %w", err)
	}
	return string(b), nil
}

// ErrMalformedOutput reports that the LLM's response could not be
// interpreted as this schema's JSON shape at all (invalid JSON syntax, or
// a valid JSON value that is not an object). Every other defect —
// unknown enum values, oversize collections, out-of-range numbers,
// invalid related IDs — is normalized rather than rejected; see
// ParseAndNormalize's doc comment.
var ErrMalformedOutput = errors.New("llmclassify: malformed structured output")

// ParseAndNormalize decodes content (the raw LLM completion text) against
// this package's v1 structured-output schema and normalizes it in place:
//
//   - an unknown Priority enum value is normalized to nil, not rejected;
//   - Keywords/Tags/OpenQuestions/LearningTargets/RelatedEntryIDs longer
//     than their bound are truncated to the first N entries;
//   - Subject/Field/Summary longer than maxStringFieldRunes are truncated;
//   - Confidence outside [0, 1] is clamped into range.
//
// Only a response that cannot be parsed as a JSON object at all returns
// ErrMalformedOutput; every other defect above is repaired instead of
// failing the whole classification. RelatedEntryIDs is truncated here but
// not yet filtered against real candidates or self-references — call
// validateRelatedIDs with the actual same-thread candidate set for that.
func ParseAndNormalize(content string) (Fields, error) {
	var raw rawOutput
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return Fields{}, fmt.Errorf("%w: %v", ErrMalformedOutput, err)
	}

	f := Fields{
		Subject:           truncateStringPtr(raw.Subject),
		Field:             truncateStringPtr(raw.Field),
		Keywords:          truncateStrings(raw.Keywords, maxKeywords),
		Tags:              truncateStrings(raw.Tags, maxTags),
		Summary:           truncateStringPtr(raw.Summary),
		OpenQuestions:     truncateStrings(raw.OpenQuestions, maxOpenQuestions),
		Unresolved:        raw.Unresolved,
		RelatedEntryIDs:   truncateStrings(raw.RelatedEntryIDs, maxRelatedEntries),
		LearningTargets:   truncateStrings(raw.LearningTargets, maxLearningTargets),
		Priority:          normalizePriority(raw.Priority),
		NotebookCandidate: raw.NotebookCandidate,
		ReviewCandidate:   raw.ReviewCandidate,
		Confidence:        clampConfidence(raw.Confidence),
	}
	return f, nil
}

func normalizePriority(p *string) *string {
	if p == nil || !allowedPriorities[*p] {
		return nil
	}
	return p
}

func clampConfidence(c float64) float64 {
	switch {
	case c < 0:
		return 0
	case c > 1:
		return 1
	default:
		return c
	}
}

func truncateStrings(in []string, max int) []string {
	if len(in) <= max {
		return in
	}
	return in[:max]
}

func truncateStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	runes := []rune(*s)
	if len(runes) <= maxStringFieldRunes {
		return s
	}
	truncated := string(runes[:maxStringFieldRunes])
	return &truncated
}

// validateRelatedIDs filters rawIDs down to the ones this classification
// may safely record as LLMClassification.RelatedEntryIDs: it drops
// targetID itself (self-reference), drops duplicates, and drops any ID
// that does not name one of candidates (a hallucinated or
// cross-thread ID — v1's related-post scope is same-thread only, see
// promptbuilder.go's BuildCandidates). An empty result is valid: it does
// not fail the classification, it just means no related posts survived
// validation.
func validateRelatedIDs(rawIDs []string, candidates []domain.Entry, targetID string) []string {
	candidateIDs := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candidateIDs[c.ID] = true
	}

	seen := make(map[string]bool, len(rawIDs))
	var out []string
	for _, id := range rawIDs {
		if id == targetID || seen[id] || !candidateIDs[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == maxRelatedEntries {
			break
		}
	}
	return out
}
