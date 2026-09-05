package httpserver

import (
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// TestWireText_MarksReplyAndFollowUp pins Issue #13 AC5: an
// llm_reply/llm_follow_up entry's wire-visible text gets a fixed
// distinguishing marker prefix, since Misskey's Note has no field of its
// own to carry Entry.Kind.
func TestWireText_MarksReplyAndFollowUp(t *testing.T) {
	tests := []struct {
		name string
		kind domain.EntryKind
		body string
		want string
	}{
		{"reply", domain.EntryLLMReply, "here is my answer", "[reply]\n\nhere is my answer"},
		{"follow-up", domain.EntryLLMFollowUp, "can you clarify?", "[follow-up question]\n\ncan you clarify?"},
		{"user post untouched", domain.EntryUserPost, "hello world", "hello world"},
		{"news untouched (already marked in Body itself)", domain.EntryNews, "[news] headline\n\nsummary", "[news] headline\n\nsummary"},
		{"mail untouched (already marked in Body itself)", domain.EntryMail, "From: a@example.com\n\nhi", "From: a@example.com\n\nhi"},
		{"system untouched", domain.EntrySystem, "system notice", "system notice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := domain.Entry{Kind: tt.kind, Body: tt.body}
			if got := wireText(e); got != tt.want {
				t.Errorf("wireText(%+v) = %q, want %q", e, got, tt.want)
			}
		})
	}
}

// TestNewNote_UsesWireTextNotBodyDirectly guards against a future
// regression where newNote is changed back to project e.Body directly,
// which would silently drop the reply/follow-up marker again.
func TestNewNote_UsesWireTextNotBodyDirectly(t *testing.T) {
	e := domain.Entry{ID: "entry-1", Kind: domain.EntryLLMFollowUp, Body: "what do you mean?"}
	n := newNote(e, userLite{ID: "actor-1", Username: "assistant"})
	if n.Text == nil {
		t.Fatal("Text = nil, want non-nil")
	}
	want := "[follow-up question]\n\nwhat do you mean?"
	if *n.Text != want {
		t.Errorf("newNote(...).Text = %q, want %q", *n.Text, want)
	}
}
