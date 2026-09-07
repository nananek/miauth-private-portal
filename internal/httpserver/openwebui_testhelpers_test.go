package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// e2eFakeProvider is a scriptable openwebui.Provider shared by this
// package's httpserver-level Open WebUI end-to-end tests
// (openwebui_e2e_test.go, openwebui_branch_isolation_test.go,
// openwebui_outage_test.go, openwebui_ambiguity_test.go): each of the
// three Provider calls delegates to a settable func field and records how
// many times it ran, the same shape internal/openwebui/turnjob_test.go's
// own fakeProvider uses — minus that one's concurrency bookkeeping, which
// nothing here needs, since every call in these tests happens on the
// test goroutine, one at a time. Restart continuity
// (openwebui_restart_test.go) keeps its own restartFakeProvider instead:
// it mints its own fixed remote ids across two independent harness
// instances rather than scripting per-scenario responses.
type e2eFakeProvider struct {
	startChatCalls    int
	continueTurnCalls int
	lookupCalls       int

	startChat     func(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error)
	continueTurn  func(ctx context.Context, req openwebui.ContinueTurnRequest) (openwebui.TurnResult, error)
	lookupOutcome func(ctx context.Context, remoteChatID, assistantMessageID string) (openwebui.TurnOutcome, error)
}

func (p *e2eFakeProvider) StartChat(ctx context.Context, req openwebui.StartChatRequest) (openwebui.TurnResult, error) {
	p.startChatCalls++
	if p.startChat == nil {
		return openwebui.TurnResult{}, errNotScripted
	}
	return p.startChat(ctx, req)
}

func (p *e2eFakeProvider) ContinueTurn(ctx context.Context, req openwebui.ContinueTurnRequest) (openwebui.TurnResult, error) {
	p.continueTurnCalls++
	if p.continueTurn == nil {
		return openwebui.TurnResult{}, errNotScripted
	}
	return p.continueTurn(ctx, req)
}

func (p *e2eFakeProvider) LookupTurnOutcome(ctx context.Context, remoteChatID, assistantMessageID string) (openwebui.TurnOutcome, error) {
	p.lookupCalls++
	if p.lookupOutcome == nil {
		return openwebui.TurnOutcome{}, errNotScripted
	}
	return p.lookupOutcome(ctx, remoteChatID, assistantMessageID)
}

func strPtrForTest(s string) *string { return &s }

var errNotScripted = &notScriptedError{}

type notScriptedError struct{}

func (*notScriptedError) Error() string { return "e2eFakeProvider: call not scripted for this test" }

// findOpenWebUITurnJobFor returns the pending "openwebui_turn" job
// enqueued for sourceEntryID, failing the test if there is not exactly
// one such job. It backs both runOpenWebUITurnJobFor
// (openwebui_restart_test.go) and this file's own
// runOpenWebUITurnJobForIgnoringError.
func findOpenWebUITurnJobFor(t *testing.T, ts *noteAPITestServer, sourceEntryID string) domain.Job {
	t.Helper()
	jobRows, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var turnJob domain.Job
	for _, j := range jobRows {
		if j.JobType == openwebui.JobType && j.SourceEntryID != nil && *j.SourceEntryID == sourceEntryID {
			turnJob = j
		}
	}
	if turnJob.ID == "" {
		t.Fatalf("no %q job for source entry %q among %v", openwebui.JobType, sourceEntryID, jobRows)
	}
	return turnJob
}

// runOpenWebUITurnJobForIgnoringError is runOpenWebUITurnJobFor's variant
// for scenarios that legitimately end a turn ambiguous or failed:
// TurnJob.Handle returns a *jobs.PermanentError even when it has already
// durably written that terminal outcome, since a "create" phase failure
// is never retried at the job-queue level either (see
// TestTurnJob_StartChat_TimeoutFreezesLinkAmbiguousAndNeverRecreates in
// internal/openwebui). Callers assert the resulting turn/link state
// themselves and may inspect the returned error.
func runOpenWebUITurnJobForIgnoringError(t *testing.T, ts *noteAPITestServer, provider openwebui.Provider, sourceEntryID string) error {
	t.Helper()
	turnJob := findOpenWebUITurnJobFor(t, ts, sourceEntryID)
	handler := openwebui.NewTurnJob(ts.db.Repos, ts.timeline, provider, nil, openwebui.TurnJobConfig{MaxAttempts: 8, MaxContextMessages: 100}, ts.clock, nil)
	return handler.Handle(t.Context(), turnJob)
}

// notesChildren posts /api/notes/children for parentID and returns the
// decoded replies, in whatever order the endpoint returns them. Backs
// onlyChildOf (openwebui_restart_test.go), for callers needing an exact
// single-child assertion, and this package's other Open WebUI E2E tests
// needing to assert on more than one child (branch isolation) or on none
// at all (outage, ambiguity).
func notesChildren(t *testing.T, ts *noteAPITestServer, parentID string) []note {
	t.Helper()
	rec := ts.post(t, "/api/notes/children", map[string]any{"noteId": parentID})
	if rec.Code != http.StatusOK {
		t.Fatalf("notes/children(%s): %d %s", parentID, rec.Code, rec.Body.String())
	}
	var children []note
	if err := json.Unmarshal(rec.Body.Bytes(), &children); err != nil {
		t.Fatalf("decode children: %v", err)
	}
	return children
}
