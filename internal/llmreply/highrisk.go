package llmreply

import "strings"

// highRiskMarkers name legal/medical/financial topics that AGENTS.md
// requires qualified, non-definitive language for ("High-risk legal,
// medical, and financial replies must be qualified and must not claim
// certainty"). Matching is substring-based and, like policy.go's
// trigger lists, a hardcoded v1 constant rather than operator-configurable.
var highRiskMarkers = []string{
	// legal
	"訴訟", "弁護士", "契約書", "法律", "違法", "loan agreement",
	// medical
	"診断", "処方", "症状", "通院", "医師", "病院", "薬",
	// financial
	"投資", "資産運用", "確定申告", "税金", "住宅ローン",
}

var highRiskMarkersLower = []string{
	"legal advice", "lawsuit", "contract law", "sue me", "sue them",
	"medical advice", "diagnosis", "symptom", "prescription", "treatment",
	"financial advice", "investment advice", "tax advice", "stock tip",
}

// isHighRisk reports whether body touches a legal, medical, or financial
// topic this service must answer only with qualified language.
func isHighRisk(body string) bool {
	lower := strings.ToLower(body)
	return containsAny(body, highRiskMarkers) || containsAny(lower, highRiskMarkersLower)
}

// nonAssertiveInstruction is always included in the system prompt,
// regardless of high-risk detection, so a v1 misclassification never
// leaves an unqualified, certainty-claiming assistant persona as the
// fallback (Issue #9 acceptance criteria: "断定を強制するpromptになっていない").
const nonAssertiveInstruction = "Use qualified, non-definitive language (\"it may be\", \"one option could be\", ...). Never state an uncertain claim as settled fact."

// highRiskDisclaimerInstruction is appended only when isHighRisk detects
// a legal, medical, or financial topic, on top of nonAssertiveInstruction.
const highRiskDisclaimerInstruction = "This entry touches a legal, medical, or financial topic. Do not give definitive advice; note relevant uncertainty and limits, and suggest consulting a qualified professional where appropriate."
