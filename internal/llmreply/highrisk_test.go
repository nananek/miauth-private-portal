package llmreply

import (
	"strings"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestIsHighRisk(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "legal japanese", body: "弁護士に相談すべきか迷っている", want: true},
		{name: "medical japanese", body: "症状が続くので病院に行った", want: true},
		{name: "financial japanese", body: "確定申告の準備をしている", want: true},
		{name: "legal english", body: "Do I need legal advice for this contract?", want: true},
		{name: "medical english", body: "Got a diagnosis today from my doctor", want: true},
		{name: "financial english", body: "Looking for investment advice on index funds", want: true},
		{name: "legal english mixed case", body: "Signing a Loan Agreement tomorrow, nervous about it", want: true},
		{name: "unrelated", body: "Fixed a bug in the timeline pagination logic", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHighRisk(tt.body); got != tt.want {
				t.Errorf("isHighRisk(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestSystemPrompt_AlwaysIncludesNonAssertiveInstruction(t *testing.T) {
	for _, kind := range []domain.GenerationKind{domain.GenerationReply, domain.GenerationFollowUp} {
		for _, highRisk := range []bool{true, false} {
			prompt := systemPrompt(kind, highRisk)
			if !strings.Contains(prompt, nonAssertiveInstruction) {
				t.Errorf("systemPrompt(%q, highRisk=%v) = %q, missing base non-assertive instruction", kind, highRisk, prompt)
			}
		}
	}
}

func TestSystemPrompt_HighRiskAddsDisclaimerOnlyWhenDetected(t *testing.T) {
	withRisk := systemPrompt(domain.GenerationReply, true)
	if !strings.Contains(withRisk, highRiskDisclaimerInstruction) {
		t.Errorf("systemPrompt with highRisk=true missing disclaimer: %q", withRisk)
	}
	withoutRisk := systemPrompt(domain.GenerationReply, false)
	if strings.Contains(withoutRisk, highRiskDisclaimerInstruction) {
		t.Errorf("systemPrompt with highRisk=false unexpectedly includes disclaimer: %q", withoutRisk)
	}
}

func TestSystemPrompt_DiffersByKind(t *testing.T) {
	reply := systemPrompt(domain.GenerationReply, false)
	followUp := systemPrompt(domain.GenerationFollowUp, false)
	if reply == followUp {
		t.Error("systemPrompt should differ between reply and follow_up_question kinds")
	}
	if !strings.Contains(followUp, followUpInstruction) {
		t.Errorf("follow-up systemPrompt missing followUpInstruction: %q", followUp)
	}
	if !strings.Contains(reply, replyInstruction) {
		t.Errorf("reply systemPrompt missing replyInstruction: %q", reply)
	}
}
