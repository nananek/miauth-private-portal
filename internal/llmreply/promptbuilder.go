package llmreply

import (
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// basePersona frames the assistant's role for every generation,
// regardless of kind or high-risk status.
const basePersona = "You are a private journaling assistant. Only the journal's owner can see this conversation."

const replyInstruction = "Write a concise, helpful reply to the latest entry below."

const followUpInstruction = "Ask exactly one concise, genuinely useful follow-up question about the latest entry below, rather than answering it directly."

// ContextBudget bounds how much prior thread history BuildThreadContext
// includes in a generation request.
type ContextBudget struct {
	// MaxMessages bounds the number of prior entries included. Zero or
	// negative means unbounded by message count.
	MaxMessages int
	// MaxChars bounds the total combined body length of included prior
	// entries. Zero or negative means unbounded by character count.
	MaxChars int
}

// BuildThreadContext returns the prior entries in threadEntries (already
// oldest-first, per EntryRepository.ListByThread) that precede
// targetEntryID, bounded by budget. targetEntryID itself is never
// included: callers already hold the target Entry and pass it to
// BuildMessages separately as the turn generation responds to.
//
// Hidden and archived entries are always excluded from LLM context: a
// user who hides or archives an entry has signaled it should no longer
// influence the visible timeline, and that intent extends to not feeding
// it back into a newly generated reply.
func BuildThreadContext(threadEntries []domain.Entry, targetEntryID string, budget ContextBudget) []domain.Entry {
	var prior []domain.Entry
	for _, e := range threadEntries {
		if e.ID == targetEntryID {
			break
		}
		if e.ArchivedAt != nil || e.HiddenAt != nil {
			continue
		}
		prior = append(prior, e)
	}

	if budget.MaxMessages > 0 && len(prior) > budget.MaxMessages {
		prior = prior[len(prior)-budget.MaxMessages:]
	}

	if budget.MaxChars > 0 {
		total := 0
		for _, e := range prior {
			total += len(e.Body)
		}
		for len(prior) > 0 && total > budget.MaxChars {
			total -= len(prior[0].Body)
			prior = prior[1:]
		}
	}
	return prior
}

// BuildMessages assembles the full chat-completions message list: a
// system prompt (persona + always-on qualified-language instruction +
// kind-specific framing + an optional high-risk disclaimer instruction),
// the bounded prior thread context (oldest-first), and target's own body
// as the final turn generation responds to.
func BuildMessages(kind domain.GenerationKind, context []domain.Entry, target domain.Entry) []Message {
	messages := []Message{{Role: "system", Content: systemPrompt(kind, isHighRisk(target.Body))}}
	for _, e := range context {
		messages = append(messages, Message{Role: roleForKind(e.Kind), Content: e.Body})
	}
	messages = append(messages, Message{Role: "user", Content: target.Body})
	return messages
}

// roleForKind maps an Entry's Kind to its chat-completions role.
// EntryUserPost, EntryNews, EntryMail, and EntrySystem are all
// non-assistant context from the model's perspective (they are never
// something the assistant itself said), so they all become "user"; only
// EntryLLMReply/EntryLLMFollowUp — always authored by the reserved
// assistant actor — become "assistant". This mirrors
// timeline.authorActorTypeForKind without needing an extra actor lookup:
// Kind alone already determines it.
func roleForKind(kind domain.EntryKind) string {
	switch kind {
	case domain.EntryLLMReply, domain.EntryLLMFollowUp:
		return "assistant"
	default:
		return "user"
	}
}

func systemPrompt(kind domain.GenerationKind, highRisk bool) string {
	instruction := replyInstruction
	if kind == domain.GenerationFollowUp {
		instruction = followUpInstruction
	}
	prompt := fmt.Sprintf("%s %s %s", basePersona, nonAssertiveInstruction, instruction)
	if highRisk {
		prompt = prompt + " " + highRiskDisclaimerInstruction
	}
	return prompt
}
