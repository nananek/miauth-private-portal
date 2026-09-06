package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// mustIssueReactionToken issues a token carrying both reaction scopes
// (and write:notes, so the same token can also create notes to react
// to) for a unique routeSessionID.
func mustIssueReactionToken(t *testing.T, ts *noteAPITestServer, routeSessionID string) string {
	t.Helper()
	token, _ := mustIssueToken(t, ts.Server, routeSessionID, "write:notes,read:reactions,write:reactions")
	return token
}

func TestHandleNotesReactionsCreate_SuccessPopulatesReactionsAndMyReaction(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-create-session")

	createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "hello"})
	var created createdNoteResponse
	mustDecode(t, createRec, &created)

	rec := ts.post(t, "/api/notes/reactions/create", map[string]any{
		"i": reactionToken, "noteId": created.CreatedNote.ID, "reaction": "👍",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	showRec := ts.post(t, "/api/notes/show", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
	var shown note
	mustDecode(t, showRec, &shown)
	if shown.Reactions["👍"] != 1 {
		t.Errorf("reactions[👍] = %d, want 1", shown.Reactions["👍"])
	}
	if shown.MyReaction == nil || *shown.MyReaction != "👍" {
		t.Errorf("myReaction = %v, want 👍", shown.MyReaction)
	}
}

func TestHandleNotesReactionsCreate_SecondCallOverwritesReaction(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-overwrite-session")

	createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "hello"})
	var created createdNoteResponse
	mustDecode(t, createRec, &created)

	if rec := ts.post(t, "/api/notes/reactions/create", map[string]any{
		"i": reactionToken, "noteId": created.CreatedNote.ID, "reaction": "👍",
	}); rec.Code != http.StatusOK {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ts.post(t, "/api/notes/reactions/create", map[string]any{
		"i": reactionToken, "noteId": created.CreatedNote.ID, "reaction": "❤️",
	}); rec.Code != http.StatusOK {
		t.Fatalf("second create (changeReaction): %d %s", rec.Code, rec.Body.String())
	}

	showRec := ts.post(t, "/api/notes/show", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
	var shown note
	mustDecode(t, showRec, &shown)
	if len(shown.Reactions) != 1 || shown.Reactions["❤️"] != 1 {
		t.Errorf("reactions = %v, want exactly one ❤️ (no leftover 👍)", shown.Reactions)
	}
	if shown.MyReaction == nil || *shown.MyReaction != "❤️" {
		t.Errorf("myReaction = %v, want ❤️", shown.MyReaction)
	}
}

// TestHandleNotesReactionsCreate_AllowsReactingToNonOwnAuthoredNote pins
// plan-issue-23 §1 PR4's recommended target-note policy: reactions are
// not restricted to the owner's own user_post entries the way
// /api/notes/delete is — an assistant-authored llm_reply is a valid
// reaction target.
func TestHandleNotesReactionsCreate_AllowsReactingToNonOwnAuthoredNote(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-non-own-session")

	rootRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "root"})
	var root createdNoteResponse
	mustDecode(t, rootRec, &root)

	gen := domain.LLMGeneration{
		ID: domain.NewID(), TargetEntryID: root.CreatedNote.ID, Kind: domain.GenerationReply,
		Provider: "test-provider", Model: "test-model", PromptVersion: "test-v1",
		Status: domain.GenerationPending, RequestedAt: ts.clock.Now(),
	}
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	reply, err := ts.timeline.CreateGeneratedReply(t.Context(), root.CreatedNote.ID, domain.EntryLLMReply, "llm reply body", gen.ID, nil, nil)
	if err != nil {
		t.Fatalf("create generated reply: %v", err)
	}

	rec := ts.post(t, "/api/notes/reactions/create", map[string]any{"i": reactionToken, "noteId": reply.ID, "reaction": "👍"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d (reacting to an assistant-authored note must be allowed)", rec.Code, rec.Body.String(), http.StatusOK)
	}
}

func TestHandleNotesReactionsCreate_RejectsCustomEmojiShortcode(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-shortcode-session")

	createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "hello"})
	var created createdNoteResponse
	mustDecode(t, createRec, &created)

	for _, shortcode := range []string{":blush:", ":blush@remote.example:"} {
		t.Run(shortcode, func(t *testing.T) {
			rec := ts.post(t, "/api/notes/reactions/create", map[string]any{
				"i": reactionToken, "noteId": created.CreatedNote.ID, "reaction": shortcode,
			})
			assertWireError(t, rec, http.StatusBadRequest, "UNSUPPORTED_FEATURE")
		})
	}
}

func TestHandleNotesReactionsCreate_MissingFieldsAndUnknownOrHiddenNote(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-invalid-session")

	t.Run("missing_reaction", func(t *testing.T) {
		createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "hello"})
		var created createdNoteResponse
		mustDecode(t, createRec, &created)
		rec := ts.post(t, "/api/notes/reactions/create", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
		assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
	})

	t.Run("missing_note_id", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions/create", map[string]any{"i": reactionToken, "reaction": "👍"})
		assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
	})

	t.Run("unknown_note_id", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions/create", map[string]any{"i": reactionToken, "noteId": "does-not-exist", "reaction": "👍"})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})

	t.Run("hidden_note", func(t *testing.T) {
		createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "will be hidden"})
		var created createdNoteResponse
		mustDecode(t, createRec, &created)
		if err := ts.timeline.SetHidden(t.Context(), created.CreatedNote.ID, true); err != nil {
			t.Fatal(err)
		}
		rec := ts.post(t, "/api/notes/reactions/create", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID, "reaction": "👍"})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})
}

func TestHandleNotesReactionsDelete_SuccessAndIdempotentOnAbsent(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-delete-session")

	createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "hello"})
	var created createdNoteResponse
	mustDecode(t, createRec, &created)

	// Deleting a reaction that was never created must not be an error
	// (RemoveReaction is idempotent).
	rec := ts.post(t, "/api/notes/reactions/delete", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete absent reaction: %d %s, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	if rec := ts.post(t, "/api/notes/reactions/create", map[string]any{
		"i": reactionToken, "noteId": created.CreatedNote.ID, "reaction": "👍",
	}); rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec = ts.post(t, "/api/notes/reactions/delete", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	showRec := ts.post(t, "/api/notes/show", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
	var shown note
	mustDecode(t, showRec, &shown)
	if len(shown.Reactions) != 0 {
		t.Errorf("reactions after delete = %v, want empty", shown.Reactions)
	}
	if shown.MyReaction != nil {
		t.Errorf("myReaction after delete = %v, want nil", shown.MyReaction)
	}

	// Deleting again (already absent) is still not an error.
	rec = ts.post(t, "/api/notes/reactions/delete", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("second delete: %d %s, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
}

func TestHandleNotesReactionsDelete_UnknownOrHiddenNoteIsNoSuchNote(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-delete-invalid-session")

	t.Run("missing_note_id", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions/delete", map[string]any{"i": reactionToken})
		assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
	})

	t.Run("unknown_note_id", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions/delete", map[string]any{"i": reactionToken, "noteId": "does-not-exist"})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})

	t.Run("hidden_note", func(t *testing.T) {
		createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "will be hidden"})
		var created createdNoteResponse
		mustDecode(t, createRec, &created)
		if err := ts.timeline.SetHidden(t.Context(), created.CreatedNote.ID, true); err != nil {
			t.Fatal(err)
		}
		rec := ts.post(t, "/api/notes/reactions/delete", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})
}

func TestHandleNotesReactions_ListsNewestFirstFilteredByTypeAndPaginated(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-list-session")

	createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "hello"})
	var created createdNoteResponse
	mustDecode(t, createRec, &created)

	if rec := ts.post(t, "/api/notes/reactions/create", map[string]any{
		"i": reactionToken, "noteId": created.CreatedNote.ID, "reaction": "👍",
	}); rec.Code != http.StatusOK {
		t.Fatalf("create reaction: %d %s", rec.Code, rec.Body.String())
	}

	rec := ts.post(t, "/api/notes/reactions", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got []reactionUser
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Type == nil || *got[0].Type != "👍" {
		t.Errorf("type = %v, want 👍", got[0].Type)
	}
	if got[0].User.Username != "owner" {
		t.Errorf("user.username = %q, want owner", got[0].User.Username)
	}
	if got[0].ID == "" || got[0].CreatedAt == "" {
		t.Errorf("id/createdAt must be non-empty: %+v", got[0])
	}

	t.Run("filtered by non-matching type returns empty", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID, "type": "❤️"})
		var got []reactionUser
		mustDecode(t, rec, &got)
		if len(got) != 0 {
			t.Errorf("got = %+v, want empty for a non-matching type filter", got)
		}
	})

	t.Run("unknown untilId returns empty page", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID, "untilId": "does-not-exist"})
		var got []reactionUser
		mustDecode(t, rec, &got)
		if len(got) != 0 {
			t.Errorf("got = %+v, want empty for an unknown untilId", got)
		}
	})
}

func TestHandleNotesReactions_UnknownOrHiddenNoteIsNoSuchNote(t *testing.T) {
	ts := newNoteAPITestServer(t)
	reactionToken := mustIssueReactionToken(t, ts, "reactions-list-invalid-session")

	t.Run("missing_note_id", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions", map[string]any{"i": reactionToken})
		assertWireError(t, rec, http.StatusBadRequest, "INVALID_PARAM")
	})

	t.Run("unknown_note_id", func(t *testing.T) {
		rec := ts.post(t, "/api/notes/reactions", map[string]any{"i": reactionToken, "noteId": "does-not-exist"})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})

	t.Run("hidden_note", func(t *testing.T) {
		createRec := ts.post(t, "/api/notes/create", map[string]any{"i": reactionToken, "text": "will be hidden"})
		var created createdNoteResponse
		mustDecode(t, createRec, &created)
		if err := ts.timeline.SetHidden(t.Context(), created.CreatedNote.ID, true); err != nil {
			t.Fatal(err)
		}
		rec := ts.post(t, "/api/notes/reactions", map[string]any{"i": reactionToken, "noteId": created.CreatedNote.ID})
		assertWireError(t, rec, http.StatusBadRequest, "NO_SUCH_NOTE")
	})
}
