package timeline

import (
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestCreateRoot_RecordsSelfMentionForUserPostContainingOwnUsername(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")

	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "hey @owner check this out", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(got) != 1 || got[0].ID != entry.ID {
		t.Fatalf("ListMentions = %v, want exactly [%s]", entryIDsForTest(got), entry.ID)
	}
}

func TestCreateRoot_DoesNotRecordMentionForNonMatchingBody(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")

	if _, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "just a normal post", nil); err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListMentions = %v, want empty", entryIDsForTest(got))
	}
}

// TestCreateRoot_DoesNotRecordSelfMentionForNonUserPostKinds pins that
// detection only ever scans user_post bodies: an ingested news/mail item
// or a system entry whose body happens to contain "@owner" must never be
// recorded as a mention (see plan-issue-23's PR5 notes and
// recordSelfMentionIfAny's doc comment).
func TestCreateRoot_DoesNotRecordSelfMentionForNonUserPostKinds(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")

	for _, kind := range []domain.EntryKind{domain.EntryNews, domain.EntryMail, domain.EntrySystem} {
		t.Run(string(kind), func(t *testing.T) {
			if _, err := ts.CreateRoot(t.Context(), kind, "@owner mentioned in ingested content", nil); err != nil {
				t.Fatalf("CreateRoot: %v", err)
			}
		})
	}

	got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListMentions = %v, want empty (non-user_post kinds must never be scanned)", entryIDsForTest(got))
	}
}

func TestCreateReply_RecordsSelfMentionForUserPostReply(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")

	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	reply, err := ts.CreateReply(t.Context(), root.ID, domain.EntryUserPost, "reminder for @owner", nil)
	if err != nil {
		t.Fatalf("CreateReply: %v", err)
	}

	got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(got) != 1 || got[0].ID != reply.ID {
		t.Fatalf("ListMentions = %v, want exactly [%s] (root itself does not mention anyone)", entryIDsForTest(got), reply.ID)
	}
}

// TestCreateGeneratedReply_DoesNotRecordMention pins that an LLM-generated
// reply/follow-up is never scanned even if its body happens to contain
// "@owner": Issue #23's own text treats an LLM reply deliberately
// mentioning the owner as overlapping with the existing
// EntryLLMFollowUp concept, so this stays out of PR5's scope.
func TestCreateGeneratedReply_DoesNotRecordMention(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")

	root, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "root", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	gen := newTestGeneration(root.ID, domain.GenerationReply, ts.clock.Now())
	if err := ts.db.Generations.Create(t.Context(), gen); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	if _, err := ts.CreateGeneratedReply(t.Context(), root.ID, domain.EntryLLMReply, "@owner you should look at this", gen.ID, nil, nil); err != nil {
		t.Fatalf("CreateGeneratedReply: %v", err)
	}

	got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListMentions = %v, want empty (llm_reply must never be scanned)", entryIDsForTest(got))
	}
}

// TestCreateExternalEntry_DoesNotRecordMention mirrors
// TestCreateRoot_DoesNotRecordSelfMentionForNonUserPostKinds for the
// ingestion creation path specifically, since CreateExternalEntry builds
// its own domain.Entry independently of CreateRoot.
func TestCreateExternalEntry_DoesNotRecordMention(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")

	source := mustCreateExternalSourceForTest(t, ts, "rss", "https://example.com/feed.xml")
	item := domain.ExternalItem{SourceID: source.ID, ExternalID: "ext-1", DedupeKey: "dedupe-1"}
	if _, _, err := ts.CreateExternalEntry(t.Context(), domain.EntryNews, item, "breaking news for @owner"); err != nil {
		t.Fatalf("CreateExternalEntry: %v", err)
	}

	got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListMentions = %v, want empty (external entries must never be scanned)", entryIDsForTest(got))
	}
}

// TestSelfMentionDetection_WordBoundary pins recordSelfMentionIfAny's
// boundary rule: an "@" immediately followed by the owner's username
// only counts when the character before "@" (if any) and the character
// after the username (if any) are both outside Misskey's username
// charset ([A-Za-z0-9_]). This keeps a longer @-handle that merely
// starts with the owner's username, or an email-like "name@owner.example"
// token, from false-positiving.
func TestSelfMentionDetection_WordBoundary(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantMatch bool
	}{
		{"plain mention with trailing punctuation", "hi @owner!", true},
		{"mention alone", "@owner", true},
		{"mention followed by more username characters", "@ownership", false},
		{"mention preceded by an alphanumeric character (email-like)", "x@owner", false},
		{"mention preceded by an underscore", "x_@owner", false},
		{"mention preceded by punctuation", "(@owner)", true},
		{"no mention at all", "just a normal post", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServiceWithOwnerUsername(t, "owner")
			entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, tt.body, nil)
			if err != nil {
				t.Fatalf("CreateRoot: %v", err)
			}

			got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
			if err != nil {
				t.Fatalf("ListMentions: %v", err)
			}
			matched := len(got) == 1 && got[0].ID == entry.ID
			if matched != tt.wantMatch {
				t.Errorf("body %q: matched = %v, want %v (got %v)", tt.body, matched, tt.wantMatch, entryIDsForTest(got))
			}
		})
	}
}

// TestSelfMentionDetection_DisabledWhenOwnerUsernameUnset pins Config's
// zero value (every timeline.NewService caller before PR5): mention
// detection must stay off, not match every post, when OwnerUsername is
// unset.
func TestSelfMentionDetection_DisabledWhenOwnerUsernameUnset(t *testing.T) {
	ts := newTestService(t) // OwnerUsername unset

	entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "hey @owner check this out", nil)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	got, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 10)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListMentions = %v, want empty (detection disabled, entry %s must not be recorded)", entryIDsForTest(got), entry.ID)
	}
}

func TestListMentions_NewestFirstAndPaginated(t *testing.T) {
	ts := newTestServiceWithOwnerUsername(t, "owner")

	var ids []string
	for i := 0; i < 3; i++ {
		entry, err := ts.CreateRoot(t.Context(), domain.EntryUserPost, "ping @owner", nil)
		if err != nil {
			t.Fatalf("CreateRoot: %v", err)
		}
		ids = append(ids, entry.ID) // ids[0] oldest ... ids[2] newest
		ts.clock.Advance(time.Minute)
	}

	first, err := ts.ListMentions(t.Context(), ts.owner.ID, nil, 2)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(first) != 2 || first[0].ID != ids[2] || first[1].ID != ids[1] {
		t.Fatalf("first page = %v, want newest-first [%s, %s]", entryIDsForTest(first), ids[2], ids[1])
	}

	second, err := ts.ListMentions(t.Context(), ts.owner.ID, &domain.Cursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID}, 2)
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if len(second) != 1 || second[0].ID != ids[0] {
		t.Fatalf("second page = %v, want [%s]", entryIDsForTest(second), ids[0])
	}
}

func entryIDsForTest(entries []domain.Entry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}
