package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/llmclassify"
)

// TestNotesCreate_LLMClassificationDisabledNeverEnqueuesJob is the
// acceptance-criteria regression test for LLM_CLASSIFICATION_ENABLED's
// safe default: no "llm_classification" job is ever enqueued while the
// feature is off, even though classification (unlike reply generation)
// has no body-dependent policy that could otherwise skip it.
func TestNotesCreate_LLMClassificationDisabledNeverEnqueuesJob(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "an ordinary post"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}

	jobRows, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobRows) != 0 {
		t.Errorf("jobs = %v, want none enqueued while LLM_CLASSIFICATION_ENABLED=false", jobRows)
	}
}

// TestNotesCreate_LLMClassificationEnabledEnqueuesJobUnconditionally
// backs classification's v1 scope: unlike llmreply's policy-gated reply
// job, classification is enqueued for every user_post regardless of
// content.
func TestNotesCreate_LLMClassificationEnabledEnqueuesJobUnconditionally(t *testing.T) {
	ts := newNoteAPITestServerLLMClassificationEnabled(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "Refactored the auth module today."})
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
	if job.JobType != llmclassify.JobType {
		t.Errorf("job.JobType = %q, want %q", job.JobType, llmclassify.JobType)
	}
	if job.SourceEntryID == nil || *job.SourceEntryID != resp.CreatedNote.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", job.SourceEntryID, resp.CreatedNote.ID)
	}
	if job.State != domain.JobPending {
		t.Errorf("job.State = %q, want pending", job.State)
	}
}

// TestNotesCreate_ReplyAndClassificationJobsCoexistForSamePost backs the
// timeline.Service multi-job enqueue change: a single post with both
// features enabled must enqueue both an "llm_generation" job (when the
// v1 reply policy matches) and an "llm_classification" job, as two
// separate job rows for the same entry.
func TestNotesCreate_ReplyAndClassificationJobsCoexistForSamePost(t *testing.T) {
	ts := newNoteAPITestServerWithOptions(t, true, true)
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
	if len(jobRows) != 2 {
		t.Fatalf("jobs = %v, want exactly 2 (reply + classification)", jobRows)
	}
	gotTypes := map[string]bool{}
	for _, j := range jobRows {
		gotTypes[j.JobType] = true
		if j.SourceEntryID == nil || *j.SourceEntryID != resp.CreatedNote.ID {
			t.Errorf("job %s SourceEntryID = %v, want %q", j.ID, j.SourceEntryID, resp.CreatedNote.ID)
		}
	}
	if !gotTypes["llm_generation"] || !gotTypes[llmclassify.JobType] {
		t.Errorf("job types = %v, want both llm_generation and %s", gotTypes, llmclassify.JobType)
	}
}

// TestNotesCreate_LLMClassificationEnqueuedForReplySourcedFromChildEntry
// mirrors llmreply's own child-entry enqueue regression test: the job
// must target the actually-created reply entry, not its parent root.
func TestNotesCreate_LLMClassificationEnqueuedForReplySourcedFromChildEntry(t *testing.T) {
	ts := newNoteAPITestServerLLMClassificationEnabled(t)
	rootRec := ts.post(t, "/api/notes/create", map[string]any{"text": "unrelated root post"})
	if rootRec.Code != http.StatusOK {
		t.Fatalf("create root: %d %s", rootRec.Code, rootRec.Body.String())
	}
	var root createdNoteResponse
	if err := json.Unmarshal(rootRec.Body.Bytes(), &root); err != nil {
		t.Fatalf("decode root: %v", err)
	}

	replyRec := ts.post(t, "/api/notes/create", map[string]any{"text": "a reply", "replyId": root.CreatedNote.ID})
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
	if len(jobRows) != 2 {
		t.Fatalf("jobs = %v, want exactly 2 (one classification job per post)", jobRows)
	}
	for _, j := range jobRows {
		if j.SourceEntryID == nil {
			t.Fatalf("job %s has nil SourceEntryID", j.ID)
		}
	}
	sourceIDs := map[string]bool{*jobRows[0].SourceEntryID: true, *jobRows[1].SourceEntryID: true}
	if !sourceIDs[root.CreatedNote.ID] || !sourceIDs[reply.CreatedNote.ID] {
		t.Errorf("job source entry IDs = %v, want {%s, %s}", sourceIDs, root.CreatedNote.ID, reply.CreatedNote.ID)
	}
}
