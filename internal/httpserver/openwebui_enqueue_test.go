package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
)

// TestNotesCreate_OpenWebUIBridgeNilNeverEnqueues is the acceptance-
// criteria regression test for Options.OpenWebUIBridge's safe default
// (nil, matching OPENWEBUI_ENABLED or its generation gate being off):
// even a plain root post must never enqueue an "openwebui_turn" job
// while no hook is wired.
func TestNotesCreate_OpenWebUIBridgeNilNeverEnqueues(t *testing.T) {
	ts := newNoteAPITestServer(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create note: %d %s", rec.Code, rec.Body.String())
	}

	jobRows, err := ts.db.Jobs.List(t.Context(), domain.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobRows {
		if j.JobType == openwebui.JobType {
			t.Errorf("found an %q job %+v, want none while OpenWebUIBridge is nil", openwebui.JobType, j)
		}
	}
}

// TestNotesCreate_OpenWebUIBridgeEnqueuesAtomicallyWithPost wires a real
// *openwebui.Bridge (backed by a seeded, generation-enabled workspace)
// through Options.OpenWebUIBridge exactly as cmd/server does, and checks
// that a plain POST /api/notes/create commits the post together with its
// "openwebui_turn" job, conversation link, and turn row in the same
// transaction — the wiring plan §5.1 describes, as opposed to
// internal/openwebui's own (already-covered) enqueue-decision logic.
func TestNotesCreate_OpenWebUIBridgeEnqueuesAtomicallyWithPost(t *testing.T) {
	ts := newNoteAPITestServerOpenWebUIEnabled(t)
	rec := ts.post(t, "/api/notes/create", map[string]any{"text": "ask the model"})
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
	var turnJobs []domain.Job
	for _, j := range jobRows {
		if j.JobType == openwebui.JobType {
			turnJobs = append(turnJobs, j)
		}
	}
	if len(turnJobs) != 1 {
		t.Fatalf("openwebui_turn jobs = %v, want exactly 1", turnJobs)
	}
	if turnJobs[0].SourceEntryID == nil || *turnJobs[0].SourceEntryID != resp.CreatedNote.ID {
		t.Errorf("job.SourceEntryID = %v, want %q", turnJobs[0].SourceEntryID, resp.CreatedNote.ID)
	}

	links, err := ts.db.OpenWebUILinks.ListByThread(t.Context(), resp.CreatedNote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].State != domain.LinkCreationPending {
		t.Fatalf("links = %+v, want exactly 1 creation_pending link", links)
	}

	turns, err := ts.db.OpenWebUITurnLinks.ListByLink(t.Context(), links[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].LocalMessageID != resp.CreatedNote.ID {
		t.Fatalf("turns = %+v, want exactly 1 for %q", turns, resp.CreatedNote.ID)
	}
}
