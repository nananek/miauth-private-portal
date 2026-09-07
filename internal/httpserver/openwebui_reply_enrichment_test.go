package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// This file backs Issues #81 (citation footnotes) and #84 (chat title,
// viewer link) wire-projection enrichment (internal/httpserver/
// noteapi_wire.go's projectNote/enrichOpenWebUIReplyText). Its baseline
// regression guarantee — a reply with no title/sources/viewer link
// configured projects byte-identical to before this enrichment existed —
// is TestOpenWebUIEndToEnd_PostToProjectedVirtualActorReply
// (openwebui_e2e_test.go), which now runs with Options.OpenWebUITurnLinks
// wired (see newNoteAPITestServerOpenWebUIEnabledAt) and still asserts
// the exact "[reply]\n\n..." text unchanged.

// runOpenWebUITurnJobForWithConfig is runOpenWebUITurnJobFor
// (openwebui_restart_test.go) generalized to take an explicit
// openwebui.TurnJobConfig, so a test can set ViewerBaseURL (Issue #84,
// ADR-0005 D23) without disturbing that shared helper's simpler callers.
func runOpenWebUITurnJobForWithConfig(t *testing.T, ts *noteAPITestServer, provider openwebui.Provider, sourceEntryID string, cfg openwebui.TurnJobConfig) {
	t.Helper()
	turnJob := findOpenWebUITurnJobFor(t, ts, sourceEntryID)
	handler := openwebui.NewTurnJob(ts.db.Repos, ts.timeline, provider, nil, nil, cfg, ts.clock, nil)
	if err := handler.Handle(t.Context(), turnJob); err != nil {
		t.Fatalf("TurnJob.Handle: %v", err)
	}
}

// showNoteText posts /api/notes/show for noteID and returns its
// projected text, failing the test on any error.
func showNoteText(t *testing.T, ts *noteAPITestServer, noteID string) string {
	t.Helper()
	rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": noteID})
	if rec.Code != http.StatusOK {
		t.Fatalf("notes/show(%s): %d %s", noteID, rec.Code, rec.Body.String())
	}
	var n note
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatalf("decode note: %v", err)
	}
	if n.Text == nil {
		t.Fatalf("note %s has nil text", noteID)
	}
	return *n.Text
}

// TestOpenWebUIReplyEnrichment_TitleSourcesAndViewerLink drives one
// generated reply through a fake provider that returns a Title, one
// Source, and (via OnChatCreated) a remote chat id, with
// OPENWEBUI_VIEWER_BASE_URL configured — and checks all three
// enrichments land in the projected note text together: the title
// replaces the "[reply]" marker, footnotes follow the body, and the
// viewer link is last.
func TestOpenWebUIReplyEnrichment_TitleSourcesAndViewerLink(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	// Options.OpenWebUIViewerBaseURL has no test-server constructor
	// parameter of its own (only two tests in this package need it set
	// at all); setting the unexported Server field directly, from this
	// same-package test file, is simpler than adding a rarely-used
	// parameter to the shared constructor every other OpenWebUI test
	// calls.
	ts.openWebUIViewerBaseURL = "https://viewer.example.net"
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ask the model"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	provider := &e2eFakeProvider{
		startChat: func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
			if !req.EnableTitleGeneration {
				t.Error("StartChat EnableTitleGeneration = false, want true when OpenWebUIViewerBaseURL is configured")
			}
			if err := req.OnChatCreated(ctx, "remote-chat-1"); err != nil {
				return openwebui.TurnResult{}, err
			}
			title := "Weekend trip planning"
			url := "https://go.dev/doc/go1.24"
			return openwebui.TurnResult{
				Content: "the model's reply", RemoteCurrentID: strPtrForTest("remote-msg-1"),
				Title:   &title,
				Sources: []openwebui.Source{{Kind: openwebui.SourceKindWebSearch, DisplayName: "web_search", URL: &url}},
			}, nil
		},
	}
	runOpenWebUITurnJobForWithConfig(t, ts, provider, resp.CreatedNote.ID, openwebui.TurnJobConfig{
		MaxAttempts: 8, MaxContextMessages: 100, ViewerBaseURL: "https://viewer.example.net",
	})

	children := notesChildren(t, ts, resp.CreatedNote.ID)
	if len(children) != 1 {
		t.Fatalf("children = %v, want exactly 1 reply", children)
	}
	reply := children[0]
	if reply.Text == nil {
		t.Fatal("reply.Text = nil")
	}
	text := *reply.Text
	const want = "[reply] Weekend trip planning\n\nthe model's reply" +
		"\n\n[1] web_search (https://go.dev/doc/go1.24)" +
		"\n\nhttps://viewer.example.net/c/remote-chat-1"
	if text != want {
		t.Errorf("reply.Text = %q, want %q", text, want)
	}
}

// TestOpenWebUIReplyEnrichment_ViewerBaseURLUnset_NoTitleOrLink backs
// ADR-0005 D23's "leaving OPENWEBUI_VIEWER_BASE_URL unset reproduces
// pre-#84 behavior exactly": the same scenario as above, minus
// ViewerBaseURL, must show no title and no viewer link — but a Source
// (Issue #81, unconditional) still renders, proving the two features are
// gated independently.
func TestOpenWebUIReplyEnrichment_ViewerBaseURLUnset_NoTitleOrLink(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ask the model"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	provider := &e2eFakeProvider{
		startChat: func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
			if req.EnableTitleGeneration {
				t.Error("StartChat EnableTitleGeneration = true, want false when ViewerBaseURL is unset")
			}
			if err := req.OnChatCreated(ctx, "remote-chat-1"); err != nil {
				return openwebui.TurnResult{}, err
			}
			title := "a title the provider sent anyway"
			return openwebui.TurnResult{
				Content: "the model's reply", RemoteCurrentID: strPtrForTest("remote-msg-1"),
				Title:   &title,
				Sources: []openwebui.Source{{Kind: openwebui.SourceKindTool, DisplayName: "get_weather"}},
			}, nil
		},
	}
	runOpenWebUITurnJobFor(t, ts, provider, resp.CreatedNote.ID)

	children := notesChildren(t, ts, resp.CreatedNote.ID)
	if len(children) != 1 {
		t.Fatalf("children = %v, want exactly 1 reply", children)
	}
	if children[0].Text == nil {
		t.Fatal("reply.Text = nil")
	}
	text := *children[0].Text
	const want = "[reply]\n\nthe model's reply\n\n[1] get_weather"
	if text != want {
		t.Errorf("reply.Text = %q, want %q (no title, no viewer link)", text, want)
	}
}

// TestOpenWebUIReplyEnrichment_PlainLLMReplyUnaffected is Issue #9's own
// regression guard: a plain llm_reply (assistant-authored, no Open
// WebUI turn at all) projects exactly as wireText(e) alone already
// produces it, even on a server where Options.OpenWebUITurnLinks is
// wired — GetByAssistantEntry's domain.ErrNotFound must fall through to
// the unmodified text, never a decode error or a changed marker.
func TestOpenWebUIReplyEnrichment_PlainLLMReplyUnaffected(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	reply := mustCreateGeneratedReplyForTest(t, ts, resp.CreatedNote.ID, domain.EntryLLMReply, "a plain assistant reply")

	text := showNoteText(t, ts, reply.ID)
	if text != "[reply]\n\na plain assistant reply" {
		t.Errorf("reply.Text = %q, want the unmodified wireText output", text)
	}
}
