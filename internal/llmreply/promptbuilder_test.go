package llmreply

import (
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func entryAt(id string, kind domain.EntryKind, body string, at time.Time) domain.Entry {
	return domain.Entry{ID: id, Kind: kind, Body: body, CreatedAt: at, UpdatedAt: at}
}

func TestBuildThreadContext_ExcludesTargetAndLaterEntries(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := []domain.Entry{
		entryAt("root", domain.EntryUserPost, "root body", base),
		entryAt("target", domain.EntryUserPost, "target body", base.Add(time.Minute)),
		entryAt("later", domain.EntryUserPost, "later body", base.Add(2*time.Minute)),
	}

	got := BuildThreadContext(entries, "target", ContextBudget{})
	if len(got) != 1 || got[0].ID != "root" {
		t.Errorf("BuildThreadContext = %v, want only [root]", got)
	}
}

func TestBuildThreadContext_ExcludesHiddenAndArchived(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hiddenAt := base
	archivedAt := base
	entries := []domain.Entry{
		entryAt("visible", domain.EntryUserPost, "visible body", base),
		{ID: "hidden", Kind: domain.EntryUserPost, Body: "hidden body", CreatedAt: base.Add(time.Minute), HiddenAt: &hiddenAt},
		{ID: "archived", Kind: domain.EntryUserPost, Body: "archived body", CreatedAt: base.Add(2 * time.Minute), ArchivedAt: &archivedAt},
		entryAt("target", domain.EntryUserPost, "target body", base.Add(3*time.Minute)),
	}

	got := BuildThreadContext(entries, "target", ContextBudget{})
	if len(got) != 1 || got[0].ID != "visible" {
		t.Errorf("BuildThreadContext = %v, want only [visible]", got)
	}
}

func TestBuildThreadContext_BoundsByMaxMessages(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := []domain.Entry{
		entryAt("e1", domain.EntryUserPost, "1", base),
		entryAt("e2", domain.EntryUserPost, "2", base.Add(time.Minute)),
		entryAt("e3", domain.EntryUserPost, "3", base.Add(2*time.Minute)),
		entryAt("target", domain.EntryUserPost, "target", base.Add(3*time.Minute)),
	}

	got := BuildThreadContext(entries, "target", ContextBudget{MaxMessages: 2})
	if len(got) != 2 || got[0].ID != "e2" || got[1].ID != "e3" {
		t.Errorf("BuildThreadContext = %v, want the last 2 entries [e2, e3]", got)
	}
}

func TestBuildThreadContext_BoundsByMaxChars(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := []domain.Entry{
		entryAt("e1", domain.EntryUserPost, "aaaaa", base),                  // 5 chars
		entryAt("e2", domain.EntryUserPost, "bbbbb", base.Add(time.Minute)), // 5 chars
		entryAt("target", domain.EntryUserPost, "target", base.Add(2*time.Minute)),
	}

	got := BuildThreadContext(entries, "target", ContextBudget{MaxChars: 5})
	if len(got) != 1 || got[0].ID != "e2" {
		t.Errorf("BuildThreadContext = %v, want only the most recent entry [e2] under a 5-char budget", got)
	}
}

func TestBuildThreadContext_MissingTargetReturnsEverythingBeforeEnd(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := []domain.Entry{
		entryAt("e1", domain.EntryUserPost, "1", base),
		entryAt("e2", domain.EntryUserPost, "2", base.Add(time.Minute)),
	}
	got := BuildThreadContext(entries, "does-not-exist", ContextBudget{})
	if len(got) != 2 {
		t.Errorf("BuildThreadContext with missing target = %v, want both entries (no break triggered)", got)
	}
}

func TestBuildMessages_SystemPromptFirstAndTargetLast(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	context := []domain.Entry{
		entryAt("root", domain.EntryUserPost, "root body", base),
		entryAt("prior-reply", domain.EntryLLMReply, "assistant said this", base.Add(time.Minute)),
	}
	target := entryAt("target", domain.EntryUserPost, "target body", base.Add(2*time.Minute))

	messages := BuildMessages(domain.GenerationReply, context, target)
	if len(messages) != 4 {
		t.Fatalf("len(messages) = %d, want 4 (system + 2 context + target)", len(messages))
	}
	if messages[0].Role != "system" {
		t.Errorf("messages[0].Role = %q, want system", messages[0].Role)
	}
	if messages[1].Role != "user" || messages[1].Content != "root body" {
		t.Errorf("messages[1] = %+v, want user/root body", messages[1])
	}
	if messages[2].Role != "assistant" || messages[2].Content != "assistant said this" {
		t.Errorf("messages[2] = %+v, want assistant/assistant said this", messages[2])
	}
	last := messages[len(messages)-1]
	if last.Role != "user" || last.Content != "target body" {
		t.Errorf("last message = %+v, want user/target body", last)
	}
}

func TestBuildMessages_NonUserPostContextBecomesUserRole(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	context := []domain.Entry{
		entryAt("news", domain.EntryNews, "news body", base),
		entryAt("mail", domain.EntryMail, "mail body", base.Add(time.Minute)),
		entryAt("sys", domain.EntrySystem, "system body", base.Add(2*time.Minute)),
	}
	target := entryAt("target", domain.EntryUserPost, "target body", base.Add(3*time.Minute))

	messages := BuildMessages(domain.GenerationReply, context, target)
	for i, e := range context {
		if messages[i+1].Role != "user" {
			t.Errorf("messages[%d] (kind %q) Role = %q, want user", i+1, e.Kind, messages[i+1].Role)
		}
	}
}

func TestBuildMessages_HighRiskTargetAddsDisclaimer(t *testing.T) {
	target := entryAt("target", domain.EntryUserPost, "Should I get legal advice about this contract?", time.Now())
	messages := BuildMessages(domain.GenerationReply, nil, target)
	if !strings.Contains(messages[0].Content, highRiskDisclaimerInstruction) {
		t.Errorf("system prompt = %q, want it to include the high-risk disclaimer", messages[0].Content)
	}
}
