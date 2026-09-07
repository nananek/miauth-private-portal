package openwebui

import (
	"strings"
	"testing"
)

func TestResolveModelMentions_NoMentionReturnsEmpty(t *testing.T) {
	env := newTurnTestEnv(t)
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, "just a plain message")
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestResolveModelMentions_SingleBareMentionMatches(t *testing.T) {
	env := newTurnTestEnv(t)
	body := "hey @" + env.model.ActorSlug + " what do you think?"
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, body)
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 1 || got[0].ID != env.model.ID {
		t.Errorf("got %+v, want exactly [%s]", got, env.model.ID)
	}
}

func TestResolveModelMentions_MentionWithMatchingHostMatches(t *testing.T) {
	env := newTurnTestEnv(t)
	body := "hey @" + env.model.ActorSlug + "@" + env.workspace.PresentationHost + " what do you think?"
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, body)
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 1 || got[0].ID != env.model.ID {
		t.Errorf("got %+v, want exactly [%s]", got, env.model.ID)
	}
}

func TestResolveModelMentions_MentionWithDifferentHostIsIgnored(t *testing.T) {
	env := newTurnTestEnv(t)
	body := "hey @" + env.model.ActorSlug + "@some-other-host.example.net what do you think?"
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, body)
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none (a different presentation host is not this workspace's model)", got)
	}
}

func TestResolveModelMentions_UnknownSlugIsIgnored(t *testing.T) {
	env := newTurnTestEnv(t)
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, "hey @no-such-model, hello")
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestResolveModelMentions_InactiveModelIsIgnored(t *testing.T) {
	env := newTurnTestEnv(t)
	if err := env.db.OpenWebUIModels.SetActive(t.Context(), env.model.ID, false, env.clock.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, "hey @"+env.model.ActorSlug)
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none (inactive folds into no-match)", got)
	}
}

func TestResolveModelMentions_TwoDistinctModelsBothMatch(t *testing.T) {
	env := newTurnTestEnv(t)
	second := env.mustCreateSecondModel(t, "gpt-oss:120b", "big_model")
	body := "hey @" + env.model.ActorSlug + " and @" + second.ActorSlug + ", which of you wants this?"

	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, body)
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want exactly 2 distinct models", got)
	}
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids[env.model.ID] || !ids[second.ID] {
		t.Errorf("got %+v, want %q and %q", got, env.model.ID, second.ID)
	}
}

// TestResolveModelMentions_RepeatedMentionOfSameModelCountsOnce backs the
// decision table's "2 or more *distinct* models" wording: mentioning the
// same model twice must not itself trigger the ambiguous case.
func TestResolveModelMentions_RepeatedMentionOfSameModelCountsOnce(t *testing.T) {
	env := newTurnTestEnv(t)
	body := "@" + env.model.ActorSlug + " @" + env.model.ActorSlug + " are you there?"
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, body)
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 1 || got[0].ID != env.model.ID {
		t.Errorf("got %+v, want exactly [%s]", got, env.model.ID)
	}
}

func TestResolveModelMentions_CaseInsensitiveSlugMatch(t *testing.T) {
	env := newTurnTestEnv(t)
	body := "@" + strings.ToUpper(env.model.ActorSlug)
	got, err := ResolveModelMentions(t.Context(), env.db.Repos, env.workspace, body)
	if err != nil {
		t.Fatalf("ResolveModelMentions: %v", err)
	}
	if len(got) != 1 || got[0].ID != env.model.ID {
		t.Errorf("got %+v, want exactly [%s] for a differently-cased mention", got, env.model.ID)
	}
}
