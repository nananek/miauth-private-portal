package openwebui

import (
	"errors"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

func TestBuildTurnPath_RootHasEmptyPath(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "hello")

	path, err := BuildTurnPath(t.Context(), env.db.Repos, root, PathBounds{MaxContextMessages: 100})
	if err != nil {
		t.Fatalf("BuildTurnPath: %v", err)
	}
	if len(path.Nodes) != 0 {
		t.Errorf("Nodes = %v, want empty for a root entry", path.Nodes)
	}
}

func TestBuildTurnPath_LinearOwnerAndAssistantRoles(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "message 0")
	a0 := env.mustCreateReplyAs(t, m0, env.assistantActorID(t), domain.EntryLLMReply, "assistant reply 0")
	m1 := env.mustCreateReply(t, a0, "message 1")

	path, err := BuildTurnPath(t.Context(), env.db.Repos, m1, PathBounds{MaxContextMessages: 100})
	if err != nil {
		t.Fatalf("BuildTurnPath: %v", err)
	}
	if len(path.Nodes) != 2 {
		t.Fatalf("Nodes = %v, want 2 (root, assistant reply)", path.Nodes)
	}
	if path.Nodes[0].Entry.ID != m0.ID || path.Nodes[0].Role != pathRoleUser {
		t.Errorf("Nodes[0] = %+v, want {%s, user}", path.Nodes[0], m0.ID)
	}
	if path.Nodes[1].Entry.ID != a0.ID || path.Nodes[1].Role != pathRoleAssistant {
		t.Errorf("Nodes[1] = %+v, want {%s, assistant}", path.Nodes[1], a0.ID)
	}

	messages := ProviderMessages(path)
	if len(messages) != 2 || messages[0].Content != "message 0" || messages[1].Content != "assistant reply 0" {
		t.Errorf("ProviderMessages = %+v", messages)
	}
	if messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Errorf("ProviderMessages roles = %q, %q", messages[0].Role, messages[1].Role)
	}
}

// TestProviderMessages_NeverProducesSystemRole is Issue #74 Phase 3's
// regression coverage for ADR-0005 D4's "no system prompt" rule: every
// PathNode.Role ProviderMessages can ever see is pathRoleUser or
// pathRoleAssistant (buildPathRole's only two return values), so a
// locally assembled turn can never accidentally carry a role:"system"
// message toward the provider — no code change was needed for this
// (roadmap: "The sequence carries no local IDs, Misskey metadata,
// credentials, system prompt"), only this explicit assertion that it
// stays true.
func TestProviderMessages_NeverProducesSystemRole(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "message 0")
	a0 := env.mustCreateReplyAs(t, m0, env.assistantActorID(t), domain.EntryLLMReply, "assistant reply 0")
	m1 := env.mustCreateReply(t, a0, "message 1")

	path, err := BuildTurnPath(t.Context(), env.db.Repos, m1, PathBounds{MaxContextMessages: 100})
	if err != nil {
		t.Fatalf("BuildTurnPath: %v", err)
	}

	for _, m := range ProviderMessages(path) {
		if m.Role == "system" {
			t.Fatalf("ProviderMessages produced a role:%q message (%+v), want never \"system\"", m.Role, m)
		}
		if m.Role != pathRoleUser && m.Role != pathRoleAssistant {
			t.Fatalf("ProviderMessages produced an unexpected role %q (%+v)", m.Role, m)
		}
	}
}

func TestBuildTurnPath_IneligibleKindFailsClosed(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "root")
	systemActor, err := env.db.Actors.GetByType(t.Context(), domain.ActorSystem)
	if err != nil {
		t.Fatalf("get system actor: %v", err)
	}
	newsEntry := env.mustCreateReplyAs(t, root, systemActor.ID, domain.EntryNews, "ingested item")
	target := env.mustCreateReply(t, newsEntry, "reply to news")

	_, err = BuildTurnPath(t.Context(), env.db.Repos, target, PathBounds{MaxContextMessages: 100})
	if !errors.Is(err, ErrPathIneligible) {
		t.Errorf("err = %v, want ErrPathIneligible", err)
	}
}

func TestBuildTurnPath_HiddenNodeFailsClosed(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "root")
	reply := env.mustCreateReply(t, root, "will be hidden")
	target := env.mustCreateReply(t, reply, "reply to hidden")

	if err := env.db.Entries.SetHidden(t.Context(), reply.ID, true, env.clock.Now()); err != nil {
		t.Fatalf("hide entry: %v", err)
	}

	_, err := BuildTurnPath(t.Context(), env.db.Repos, target, PathBounds{MaxContextMessages: 100})
	if !errors.Is(err, ErrPathIneligible) {
		t.Errorf("err = %v, want ErrPathIneligible", err)
	}
}

func TestBuildTurnPath_ArchivedNodeFailsClosed(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "root")
	reply := env.mustCreateReply(t, root, "will be archived")
	target := env.mustCreateReply(t, reply, "reply to archived")

	if err := env.db.Entries.SetArchived(t.Context(), reply.ID, true, env.clock.Now()); err != nil {
		t.Fatalf("archive entry: %v", err)
	}

	_, err := BuildTurnPath(t.Context(), env.db.Repos, target, PathBounds{MaxContextMessages: 100})
	if !errors.Is(err, ErrPathIneligible) {
		t.Errorf("err = %v, want ErrPathIneligible", err)
	}
}

// TestBuildTurnPath_OrphanWhenThreadMismatches backs the walk's other
// orphan case: entries.parent_entry_id carries its own foreign key, so a
// legitimate write can never point it at a truly missing entry (that
// branch of BuildTurnPath's ErrNotFound handling stays defense-in-depth,
// like ErrPathCycle's — see the comment above). What a legitimate write
// *can* produce is an entry whose thread_id does not match the parent it
// names, which the walk must equally refuse to treat as connected.
func TestBuildTurnPath_OrphanWhenThreadMismatches(t *testing.T) {
	env := newTurnTestEnv(t)
	rootA := env.mustCreateRoot(t, "root A")
	rootB := env.mustCreateRoot(t, "root B")

	crossThread := domain.Entry{
		ID: domain.NewID(), ThreadID: rootB.ThreadID, ParentEntryID: &rootA.ID, Kind: domain.EntryUserPost,
		AuthorActorID: env.ownerID, Body: "orphan", ProcessingStatus: domain.ProcessingNone,
		CreatedAt: env.clock.Now(), UpdatedAt: env.clock.Now(),
	}
	if err := env.db.Entries.Create(t.Context(), crossThread); err != nil {
		t.Fatalf("create cross-thread entry: %v", err)
	}

	_, err := BuildTurnPath(t.Context(), env.db.Repos, crossThread, PathBounds{MaxContextMessages: 100})
	if !errors.Is(err, ErrPathOrphan) {
		t.Errorf("err = %v, want ErrPathOrphan", err)
	}
}

// ErrPathCycle's own guard is exercised indirectly: entries.parent_entry_id
// carries a REFERENCES(entries.id) foreign key and a CHECK tying nullness
// to being the thread root, and EntryRepository exposes no write that
// alters an entry's parent after creation — so no legitimate sequence of
// repository calls can ever construct a cycle for BuildTurnPath to walk
// into. The guard (visited-set check in BuildTurnPath) stays as
// defense-in-depth against a future storage bug, not something this
// suite can trigger through the public repository interface alone.

func TestBuildTurnPath_TooLongFailsClosed(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "root")
	last := root
	for i := 0; i < 3; i++ {
		last = env.mustCreateReply(t, last, "reply")
	}
	target := env.mustCreateReply(t, last, "one too many")

	// root + 3 replies = 4 path nodes; +1 for target itself = 5, which
	// exceeds a bound of 4.
	_, err := BuildTurnPath(t.Context(), env.db.Repos, target, PathBounds{MaxContextMessages: 4})
	if !errors.Is(err, ErrPathTooLong) {
		t.Errorf("err = %v, want ErrPathTooLong", err)
	}

	if _, err := BuildTurnPath(t.Context(), env.db.Repos, target, PathBounds{MaxContextMessages: 5}); err != nil {
		t.Errorf("BuildTurnPath at the exact bound: %v, want nil", err)
	}
}

func TestSelectBranch_RootReturnsNoContinuation(t *testing.T) {
	env := newTurnTestEnv(t)
	root := env.mustCreateRoot(t, "root")

	continuation, err := SelectBranch(t.Context(), env.db.Repos, root, env.model.ID)
	if err != nil {
		t.Fatalf("SelectBranch: %v", err)
	}
	if continuation != nil {
		t.Errorf("continuation = %+v, want nil for a thread root", continuation)
	}
}

func TestSelectBranch_ContinuesReadyLinkAtItsHead(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	m1 := env.mustCreateReply(t, a0, "m1")
	continuation, err := SelectBranch(t.Context(), env.db.Repos, m1, env.model.ID)
	if err != nil {
		t.Fatalf("SelectBranch: %v", err)
	}
	if continuation == nil || continuation.Link.ID != link.ID {
		t.Errorf("continuation = %+v, want link %q", continuation, link.ID)
	}
}

// TestSelectBranch_NoContinuationForDifferentTargetModel is Issue #75's
// cross-model-reply rule: a reply to the exact same head, from the exact
// same parent, still must not continue the link when the caller has
// already resolved (by @mention) that this post addresses a *different*
// model than the one the link is bound to — it must read as starting a
// fresh conversation with that other model instead.
func TestSelectBranch_NoContinuationForDifferentTargetModel(t *testing.T) {
	env := newTurnTestEnv(t)
	other := env.mustCreateSecondModel(t, "gpt-oss:120b", "other_model")
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	m1 := env.mustCreateReply(t, a0, "@other_model take over")
	continuation, err := SelectBranch(t.Context(), env.db.Repos, m1, other.ID)
	if err != nil {
		t.Fatalf("SelectBranch: %v", err)
	}
	if continuation != nil {
		t.Errorf("continuation = %+v, want nil when the resolved model differs from the link's own", continuation)
	}

	// The same reply, resolved against the *original* model, still
	// continues normally — this is purely the model-mismatch rule, not a
	// change to the existing head/parent checks.
	sameModel, err := SelectBranch(t.Context(), env.db.Repos, m1, env.model.ID)
	if err != nil {
		t.Fatalf("SelectBranch: %v", err)
	}
	if sameModel == nil || sameModel.Link.ID != link.ID {
		t.Errorf("continuation against the original model = %+v, want link %q", sameModel, link.ID)
	}
}

func TestSelectBranch_NoContinuationForReplyToEarlierNode(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	// Replying to m0 (an earlier node, not the head a0) must not continue.
	again := env.mustCreateReply(t, m0, "reply to an earlier node")
	continuation, err := SelectBranch(t.Context(), env.db.Repos, again, env.model.ID)
	if err != nil {
		t.Fatalf("SelectBranch: %v", err)
	}
	if continuation != nil {
		t.Errorf("continuation = %+v, want nil for a reply to an earlier node", continuation)
	}
}

func TestSelectBranch_NoContinuationForSecondReplyToSameHead(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	m1 := env.mustCreateReply(t, a0, "m1")
	// Record m1's own (still-pending) turn against the same link, exactly
	// as Bridge.EnqueueTurn would when it selects this link as m1's
	// continuation.
	env.mustPendingTurn(t, link, a0, m1)

	// A second reply to the same head a0 must not also continue.
	m1Again := env.mustCreateReply(t, a0, "second reply to a0")
	continuation, err := SelectBranch(t.Context(), env.db.Repos, m1Again, env.model.ID)
	if err != nil {
		t.Fatalf("SelectBranch: %v", err)
	}
	if continuation != nil {
		t.Errorf("continuation = %+v, want nil for a second reply to the same head", continuation)
	}
}

func TestSelectBranch_NoContinuationAfterFailedTurn(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "m0")
	a0 := env.mustCreateReplyAs(t, m0, env.model.ActorID, domain.EntryLLMReply, "a0")
	link := env.mustReadyLink(t, m0.ThreadID)
	env.mustSucceededTurn(t, link, m0, a0)

	m1 := env.mustCreateReply(t, a0, "m1")
	turn := env.mustPendingTurn(t, link, a0, m1)
	now := env.clock.Now()
	category := domain.FailureCategoryTurnFailed
	if err := env.db.OpenWebUITurnLinks.RecordOutcome(t.Context(), turn.ID, domain.TurnOutcomeRecord{
		Status: domain.TurnFailed, FailureCategory: &category,
	}, now); err != nil {
		t.Fatalf("record outcome: %v", err)
	}

	// Re-asking off the same parent (a0) after a failed turn must start a
	// new branch, not attach to the same remote chat.
	reask := env.mustCreateReply(t, a0, "re-ask after failure")
	continuation, err := SelectBranch(t.Context(), env.db.Repos, reask, env.model.ID)
	if err != nil {
		t.Fatalf("SelectBranch: %v", err)
	}
	if continuation != nil {
		t.Errorf("continuation = %+v, want nil for a re-ask after a failed turn", continuation)
	}
}

func TestStripMentionTagsForProvider(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "username and host",
			body: "Hey @luna@ai.tail2c8c7.ts.net, what do you think?",
			want: "Hey , what do you think?",
		},
		{
			// The single space that separated "cc" and "@owner" and the
			// single space that separated "@owner" and "please" collapse
			// to the one space that would have separated "cc" and
			// "please" had the mention never been there.
			name: "bare username without host, surrounding spaces collapse to one",
			body: "cc @owner please review",
			want: "cc please review",
		},
		{
			name: "multiple mentions separated by whitespace",
			body: "@luna@ai.tail2c8c7.ts.net and @owner both @luna@ai.tail2c8c7.ts.net",
			want: "and both",
		},
		{
			// The boundary bug this test guards against: a single
			// non-overlapping regexp.ReplaceAllString pass's own
			// "boundary" group consumed the byte that would have let a
			// second mention starting right where the first one's host
			// ended qualify as a boundary, so it fell through completely
			// unstripped (both "@" and host leaking to the provider).
			name: "back-to-back mentions with no separator",
			body: "cc @owner@host@luna@ai.tail2c8c7.ts.net done",
			want: "cc done",
		},
		{
			// A third mention chained on with no separator still ends up
			// fully masked, even though the middle match's own optional
			// host group ends up absorbing the third mention's leading
			// "@luna2" as if it were the second mention's host — no raw
			// "@"/username/host token survives into the provider-facing
			// text either way, which is the property that actually
			// matters here.
			name: "three back-to-back mentions with no separator",
			body: "@luna@host@owner@luna2@host2 chain",
			want: "chain",
		},
		{
			// No surrounding whitespace is involved here (the mention sits
			// between "(" and ")"), so nothing is left to collapse or trim.
			name: "mention immediately followed by closing punctuation",
			body: "reply(@luna@host) ok",
			want: "reply() ok",
		},
		{
			// The host label grammar (labels of letters/digits/hyphens
			// joined by ".") never lets a sentence-ending period after the
			// host be swallowed into the match, since it is not followed
			// by another label. The single space before "Thanks" is left
			// as-is: it was never doubled, and adjusting spacing around
			// punctuation is out of scope for the whitespace cleanup.
			name: "sentence-ending period right after the host is preserved",
			body: "Talk to @luna@host.example.com. Thanks",
			want: "Talk to . Thanks",
		},
		{
			name: "host with an explicit port is fully stripped",
			body: "@luna@host.example.com:8080 with port",
			want: "with port",
		},
		{
			name: "bracketed IPv6 literal host is fully stripped",
			body: "@luna@[::1] ipv6-ish",
			want: "ipv6-ish",
		},
		{
			name: "bracketed IPv6 literal host with a port is fully stripped",
			body: "@luna@[::1]:8080 ipv6 with port",
			want: "ipv6 with port",
		},
		{
			name: "no mention is unchanged",
			body: "no mentions here, just text",
			want: "no mentions here, just text",
		},
		{
			name: "email address is not a mention",
			body: "contact me at foo@example.com please",
			want: "contact me at foo@example.com please",
		},
		{
			name: "message is nothing but a mention, result is empty",
			body: "@owner",
			want: "",
		},
		{
			name: "mention at the very start, leading whitespace is trimmed",
			body: "@owner hello team",
			want: "hello team",
		},
		{
			name: "mention at the very end, trailing whitespace is trimmed",
			body: "goodbye @owner",
			want: "goodbye",
		},
		{
			name: "tab whitespace around a mention also collapses to one space",
			body: "cc\t@owner\tplease",
			want: "cc please",
		},
		{
			// U+3000 IDEOGRAPHIC SPACE (a full-width space) is a common
			// word separator in Japanese input and plays the same role a
			// regular space does around a mention.
			name: "full-width space around a mention also collapses to one space",
			body: "cc　@owner　please",
			want: "cc please",
		},
		{
			name: "mixed ascii and full-width space around a mention collapses to one space",
			body: "cc 　@owner　 please",
			want: "cc please",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripMentionTagsForProvider(tt.body); got != tt.want {
				t.Errorf("stripMentionTagsForProvider(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestProviderMessages_StripsMentionTagsButLeavesEntryBodyUntouched(t *testing.T) {
	env := newTurnTestEnv(t)
	m0 := env.mustCreateRoot(t, "hey @luna@ai.tail2c8c7.ts.net look at this")
	a0 := env.mustCreateReplyAs(t, m0, env.assistantActorID(t), domain.EntryLLMReply, "assistant reply 0")
	m1 := env.mustCreateReply(t, a0, "message 1")

	path, err := BuildTurnPath(t.Context(), env.db.Repos, m1, PathBounds{MaxContextMessages: 100})
	if err != nil {
		t.Fatalf("BuildTurnPath: %v", err)
	}

	messages := ProviderMessages(path)
	if len(messages) != 2 || messages[0].Content != "hey look at this" {
		t.Errorf("ProviderMessages = %+v, want mention omitted in messages[0]", messages)
	}

	stored, err := env.db.Entries.Get(t.Context(), m0.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Body != "hey @luna@ai.tail2c8c7.ts.net look at this" {
		t.Errorf("stored Entry.Body = %q, want the original unmodified mention", stored.Body)
	}
}
