package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/llmreply"
)

// TestNotesCreate_LLMDisabledNeverEnqueuesJob is the acceptance-criteria
// regression test for LLM_ENABLED's safe default: even a post that would
// otherwise trigger the v1 reply policy must never enqueue an
// "llm_generation" job while the feature is off.
func TestNotesCreate_LLMDisabledNeverEnqueuesJob(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "what do you think about this?"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}

	jobs, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("jobs = %v, want none enqueued while LLM_ENABLED=false", jobs)
	}
}

func TestNotesCreate_LLMEnabledEnqueuesJobForExplicitRequest(t *testing.T) {
	ts := newNoteAPITestServerLLMEnabled(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "what do you think about this?"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}
	var resp createdNoteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	jobRows, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 1 {
		t.Fatalf("jobs = %v, want exactly 1", jobRows)
	}
	job := jobRows[0]
	if job.JobType != llmreply.JobType {
		t.Errorf("job.JobType = %q, want %q", job.JobType, llmreply.JobType)
	}
	if job.SourceEntryID == nil || *job.SourceEntryID != resp.CreatedNote.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", job.SourceEntryID, resp.CreatedNote.ID)
	}
	if job.State != domain.JobPending {
		t.Errorf("job.State = %q, want pending", job.State)
	}
}

func TestNotesCreate_LLMEnabledSkipsNonTriggeringPost(t *testing.T) {
	ts := newNoteAPITestServerLLMEnabled(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "Refactored the auth module today."})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}

	jobRows, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 0 {
		t.Errorf("jobs = %v, want none for a post that does not match the v1 policy", jobRows)
	}
}

func TestNotesCreate_LLMEnabledEnqueuesJobForReplySourcedFromChildEntry(t *testing.T) {
	ts := newNoteAPITestServerLLMEnabled(t)
	rootRec := ts.post(t, "/api/notes/create", map[string]any{"text": "unrelated root post"})
	if rootRec.Code != http.StatusOK {
		t.Fatalf("create root: %d %s", rootRec.Code, rootRec.Body.String())
	}
	var root createdNoteResponse
	if err := json.Unmarshal(rootRec.Body.Bytes(), &root); err != nil {
		t.Fatalf("decode root: %v", err)
	}

	replyRec := ts.post(t, "/api/notes/create", map[string]any{"text": "any thoughts on this reply?", "replyId": root.CreatedNote.ID})
	if replyRec.Code != http.StatusOK {
		t.Fatalf("create reply: %d %s", replyRec.Code, replyRec.Body.String())
	}
	var reply createdNoteResponse
	if err := json.Unmarshal(replyRec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}

	jobRows, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 1 {
		t.Fatalf("jobs = %v, want exactly 1 (only the reply triggers the policy)", jobRows)
	}
	if jobRows[0].SourceEntryID == nil || *jobRows[0].SourceEntryID != reply.CreatedNote.ID {
		t.Errorf("job.SourceEntryID = %v, want the reply entry %q, not the root", jobRows[0].SourceEntryID, reply.CreatedNote.ID)
	}
}
