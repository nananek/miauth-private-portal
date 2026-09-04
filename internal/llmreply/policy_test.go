package llmreply

import (
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestDecideReply(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantGenerate bool
		wantKind     domain.GenerationKind
	}{
		{name: "empty body", body: "", wantGenerate: false},
		{name: "whitespace only", body: "   \n\t", wantGenerate: false},
		{name: "ascii question mark", body: "What time is it?", wantGenerate: true, wantKind: domain.GenerationReply},
		{name: "full-width question mark", body: "今何時？", wantGenerate: true, wantKind: domain.GenerationReply},
		{name: "explicit japanese trigger", body: "今日の学びについてどう思う", wantGenerate: true, wantKind: domain.GenerationReply},
		{name: "explicit english trigger", body: "Learned Go today, what do you think", wantGenerate: true, wantKind: domain.GenerationReply},
		{name: "beneficial japanese trigger", body: "この設計についてまだよくわからない", wantGenerate: true, wantKind: domain.GenerationFollowUp},
		{name: "beneficial english trigger", body: "Still learning how contexts propagate in Go", wantGenerate: true, wantKind: domain.GenerationFollowUp},
		{name: "plain statement with no trigger", body: "Finished refactoring the auth module today.", wantGenerate: false},
		{name: "explicit trigger takes priority over beneficial", body: "todo: not sure, can you help me decide?", wantGenerate: true, wantKind: domain.GenerationReply},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideReply(tt.body)
			if got.ShouldGenerate != tt.wantGenerate {
				t.Errorf("DecideReply(%q).ShouldGenerate = %v, want %v", tt.body, got.ShouldGenerate, tt.wantGenerate)
			}
			if tt.wantGenerate && got.Kind != tt.wantKind {
				t.Errorf("DecideReply(%q).Kind = %q, want %q", tt.body, got.Kind, tt.wantKind)
			}
			if got.PolicyVersion != PolicyVersion {
				t.Errorf("DecideReply(%q).PolicyVersion = %q, want %q", tt.body, got.PolicyVersion, PolicyVersion)
			}
		})
	}
}

func TestDecideReply_NeverGeneratesBothKindsAtOnce(t *testing.T) {
	// v1 scope: follow-up is an alternative to reply, never both.
	decision := DecideReply("todo: still not sure what to do, what do you think?")
	if !decision.ShouldGenerate {
		t.Fatal("expected ShouldGenerate = true")
	}
	if decision.Kind != domain.GenerationReply && decision.Kind != domain.GenerationFollowUp {
		t.Errorf("Kind = %q, want exactly one of reply/follow_up_question", decision.Kind)
	}
}
