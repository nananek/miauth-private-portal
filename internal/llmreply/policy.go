package llmreply

import (
	"strings"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// PolicyVersion identifies this file's reply/follow-up heuristic and is
// recorded on every domain.LLMGeneration as PromptVersion, alongside
// promptbuilder.go's context-assembly behavior. Bump it (and add a table
// test below) whenever the heuristic itself changes, so a stored
// generation's PromptVersion always names the exact logic that produced
// it.
//
// The trigger word/punctuation lists below are intentionally hardcoded
// Go constants for v1, not operator-configurable: Issue #9's plan
// deliberately deferred config-driven trigger lists to a future issue,
// filed only if an operator actually needs to tune them. Do not add a
// config key for this without opening that follow-up issue first.
const PolicyVersion = "reply-v1"

// ReplyDecision is DecideReply's versioned policy output: whether to
// generate at all, and if so, as a direct reply or a follow-up question.
// Per Issue #9's v1 scope, follow-up is an alternative to reply, never
// both: at most one generation job is ever produced for a given post.
type ReplyDecision struct {
	ShouldGenerate bool
	// Kind is meaningful only when ShouldGenerate is true.
	Kind          domain.GenerationKind
	PolicyVersion string
}

// explicitRequestMarkers name punctuation/phrases that make a post an
// explicit request for a reply. Matching is substring-based (not
// suffix-only): a mid-sentence question is still an explicit request.
// English entries are matched case-insensitively; Japanese entries are
// matched as-is (case does not apply).
var explicitRequestMarkers = []string{
	"?", "？",
	"教えて", "どう思う", "どう思いますか", "返信して", "返信をお願い", "コメントして", "コメントください",
	"アドバイスください", "アドバイスをください", "意見が欲しい", "意見をください",
}

// explicitRequestMarkersLower is the lowercased subset used for
// case-insensitive matching against ASCII/English text.
var explicitRequestMarkersLower = []string{
	"please reply", "what do you think", "any thoughts", "can you help",
	"could you help", "please advise", "please help", "wdyt",
}

// beneficialSupplementMarkers name phrases suggesting a post would
// benefit from a follow-up question even without an explicit request:
// unresolved thoughts, open questions to self, or something the author
// is still working through.
var beneficialSupplementMarkers = []string{
	"メモ", "TODO", "わからない", "分からない", "気になる", "調べたい", "検討中", "悩んでいる", "迷っている",
}

var beneficialSupplementMarkersLower = []string{
	"todo", "not sure", "i wonder", "still learning", "need to figure out", "trying to understand",
}

// DecideReply applies the v1 heuristic to body (a new user_post's text)
// and returns whether/how to generate. An empty or whitespace-only body
// never generates.
func DecideReply(body string) ReplyDecision {
	decision := ReplyDecision{PolicyVersion: PolicyVersion}

	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return decision
	}
	lower := strings.ToLower(trimmed)

	if containsAny(trimmed, explicitRequestMarkers) || containsAny(lower, explicitRequestMarkersLower) {
		decision.ShouldGenerate = true
		decision.Kind = domain.GenerationReply
		return decision
	}
	if containsAny(trimmed, beneficialSupplementMarkers) || containsAny(lower, beneficialSupplementMarkersLower) {
		decision.ShouldGenerate = true
		decision.Kind = domain.GenerationFollowUp
		return decision
	}
	return decision
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
