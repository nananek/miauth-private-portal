package integration

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// generatedReplyBody is the fixed content the fake LLM provider below
// returns once it is "recovered". It intentionally contains no
// substring that could collide with the provenance marker itself, so a
// mismatched/duplicated prefix is easy to spot in a test failure.
const generatedReplyBody = "here is a generated reply from the fake provider"

// fakeLLMServer is an OpenAI-compatible chat-completions endpoint that
// starts out failing (503, the retryable "server_error" category
// internal/provider/openai classifies) and can be flipped to succeed,
// standing in for an operator restarting/fixing a real LLM provider.
type fakeLLMServer struct {
	*httptest.Server
	up atomic.Bool
}

func newFakeLLMServer() *fakeLLMServer {
	f := &fakeLLMServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(generatedReplyBody) + `,"refusal":""},"finish_reason":"stop"}]}`))
	}))
	return f
}

// jsonString renders s as a JSON string literal without importing
// encoding/json into this small handler; it only ever encodes the fixed
// generatedReplyBody constant above, so a minimal escaper is enough.
func jsonString(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		if r == '"' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, string(r)...)
	}
	out = append(out, '"')
	return string(out)
}

// TestServerE2E_PostSucceedsWhileLLMDownAndReplyRecoversWithMarker is
// Issue #13 AC4's automated evidence, over the real cmd/server binary
// and real HTTP: a user post must succeed (201/200 from
// /api/notes/create) while the configured LLM provider is unreachable,
// and once the provider recovers, the durable "llm_generation" job must
// retry and succeed on its own — no jobsctl retry call — producing a
// reply visible through /api/notes/children.
//
// It doubles as the deferred AC5 evidence that Issue #13's Note.text
// provenance marker (docs/compat/aria-v1.5.11.md's "Note.text
// provenance markers") actually reaches the wire: the reply's text is
// asserted to start with the "[reply]\n\n" marker exactly as
// internal/httpserver's wireText produces it, fetched over a real HTTP
// call rather than a direct package-level assertion. This is
// deliberately a Go-level integration test rather than an addition to
// contract/aria_client's Dart suite: driving a real llm_generation job
// to completion needs a controllable fake LLM provider, and
// internal/provider/openai's HTTP client (unlike RSS/IMAP's safehttp)
// has no SSRF restriction that would prevent pointing LLM_BASE_URL at
// an in-process httptest.Server, making this the more direct path to
// real evidence than teaching the Dart/bash contract harness to also
// drive an asynchronous job to completion.
func TestServerE2E_PostSucceedsWhileLLMDownAndReplyRecoversWithMarker(t *testing.T) {
	serverBin := buildBinary(t, "./cmd/server", "server")
	miauthctlBin := buildBinary(t, "./cmd/miauthctl", "miauthctl")

	fakeLLM := newFakeLLMServer()
	defer fakeLLM.Close()

	ts := startServer(t, serverBin, map[string]string{
		"LLM_ENABLED":        "true",
		"LLM_BASE_URL":       fakeLLM.URL,
		"LLM_MODEL":          "fake-model",
		"LLM_TIMEOUT":        "2s",
		"JOBS_POLL_INTERVAL": "100ms",
		"JOBS_BACKOFF_BASE":  "100ms",
		"JOBS_BACKOFF_MAX":   "300ms",
		"JOBS_MAX_ATTEMPTS":  "50",
	})
	ts.waitForReady(t, 10*time.Second)
	defer ts.terminateAndWait(t, 5*time.Second)

	token := approveMiAuthSession(t, miauthctlBin, ts, "read:account,read:notes,write:notes")

	var created struct {
		CreatedNote struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"createdNote"`
	}
	// The question mark trips internal/llmreply/policy.go's
	// DecideReply heuristic into generating a direct reply.
	resp := postJSON(t, ts.baseURL+"/api/notes/create", map[string]any{
		"text": "Does posting still work while the LLM provider is down?",
		"i":    token,
	}, &created)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("notes/create status = %d, want 200 even while the LLM provider is down", resp.StatusCode)
	}
	rootID := created.CreatedNote.ID
	if rootID == "" {
		t.Fatal("notes/create did not return a note id")
	}

	// Give the durable job at least one attempt against the still-down
	// provider before recovering it, so success below cannot be
	// explained by the provider having been reachable all along.
	time.Sleep(400 * time.Millisecond)

	fakeLLM.up.Store(true)

	deadline := time.Now().Add(15 * time.Second)
	var replyText string
	for time.Now().Before(deadline) {
		var children []struct {
			Text    string  `json:"text"`
			ReplyID *string `json:"replyId"`
		}
		postJSON(t, ts.baseURL+"/api/notes/children", map[string]any{
			"noteId": rootID,
			"i":      token,
		}, &children)
		if len(children) > 0 {
			replyText = children[0].Text
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if replyText == "" {
		t.Fatalf("no reply appeared under %s/api/notes/children within timeout after the LLM provider recovered:\n%s", ts.baseURL, ts.output.String())
	}

	wantText := "[reply]\n\n" + generatedReplyBody
	if replyText != wantText {
		t.Errorf("reply text = %q, want %q (Note.text provenance marker missing or altered)", replyText, wantText)
	}
}
