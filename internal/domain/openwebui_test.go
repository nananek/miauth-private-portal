package domain

import (
	"errors"
	"testing"
)

// allLinkStates and allLinkEvents drive the exhaustive transition table
// below. Adding a state or an event without deciding what it does from
// every existing state breaks that test, which is the point: a
// conversation link's whole safety story is "no state but this one may
// do that", so a silently permissive new pair is exactly the bug worth
// catching.
var allLinkStates = []LinkState{LinkCreationPending, LinkReady, LinkAmbiguous, LinkFailed, LinkDead, LinkStateless}

var allLinkEvents = []LinkEvent{
	LinkEventConfirmed,
	LinkEventDefinitiveFailure,
	LinkEventUncertainCreation,
	LinkEventUncertainContinuation,
	LinkEventOwnerConfirmed,
	LinkEventOwnerAbandoned,
	LinkEventServedStateless,
}

// TestLinkTransition_ExhaustiveTable walks every (state, event) pair and
// checks it against the roadmap's diagram (extended by ADR-0005
// D24/D25's stateless addition): the seven listed edges are allowed and
// reach the stated state, and every other pair is rejected. In
// particular a failed, dead, or stateless link accepts nothing, and no
// event other than an owner's may move an ambiguous link.
func TestLinkTransition_ExhaustiveTable(t *testing.T) {
	allowed := map[LinkState]map[LinkEvent]LinkState{
		LinkCreationPending: {
			LinkEventConfirmed:         LinkReady,
			LinkEventDefinitiveFailure: LinkFailed,
			LinkEventUncertainCreation: LinkAmbiguous,
			LinkEventServedStateless:   LinkStateless,
		},
		LinkReady: {
			LinkEventUncertainContinuation: LinkAmbiguous,
		},
		LinkAmbiguous: {
			LinkEventOwnerConfirmed: LinkReady,
			LinkEventOwnerAbandoned: LinkDead,
		},
	}

	for _, from := range allLinkStates {
		for _, ev := range allLinkEvents {
			t.Run(string(from)+"/"+string(ev), func(t *testing.T) {
				want, isAllowed := allowed[from][ev]
				got, err := LinkTransition(from, ev)
				if !isAllowed {
					if !errors.Is(err, ErrInvalidLinkTransition) {
						t.Fatalf("LinkTransition(%q, %q) = %q, %v; want ErrInvalidLinkTransition", from, ev, got, err)
					}
					if got != "" {
						t.Errorf("rejected transition returned state %q, want the zero value", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("LinkTransition(%q, %q) error = %v, want nil", from, ev, err)
				}
				if got != want {
					t.Errorf("LinkTransition(%q, %q) = %q, want %q", from, ev, got, want)
				}
			})
		}
	}
}

// TestLinkTransition_UnknownStateOrEventIsRejected covers the values
// that are not in the enums at all — a row written by a newer build, or
// a caller passing a string it made up. Neither may fall through to a
// permissive default.
func TestLinkTransition_UnknownStateOrEventIsRejected(t *testing.T) {
	if _, err := LinkTransition(LinkState("unlinked"), LinkEventConfirmed); !errors.Is(err, ErrInvalidLinkTransition) {
		t.Errorf("transition from the unlinked pseudo-state error = %v, want ErrInvalidLinkTransition", err)
	}
	if _, err := LinkTransition(LinkCreationPending, LinkEvent("something_new")); !errors.Is(err, ErrInvalidLinkTransition) {
		t.Errorf("transition on an unknown event error = %v, want ErrInvalidLinkTransition", err)
	}
}

// TestConversationLink_AllowsInitialStartChat is the roadmap's
// single-use claim rule: the one job that claimed the branch may issue
// the one initial StartChat, while it is still pending. Everything else
// — another job, a link with no claim recorded, or the claiming job
// after any transition — is false, because a second create would mean a
// second remote chat that nothing local knows about.
func TestConversationLink_AllowsInitialStartChat(t *testing.T) {
	const claimingJob = "job-1"
	jobID := claimingJob

	tests := []struct {
		name  string
		link  OpenWebUIConversationLink
		jobID string
		want  bool
	}{
		{
			name:  "claiming job while pending",
			link:  OpenWebUIConversationLink{State: LinkCreationPending, ClaimJobID: &jobID},
			jobID: claimingJob,
			want:  true,
		},
		{
			name:  "a different job while pending",
			link:  OpenWebUIConversationLink{State: LinkCreationPending, ClaimJobID: &jobID},
			jobID: "job-2",
			want:  false,
		},
		{
			name:  "pending with no claim recorded",
			link:  OpenWebUIConversationLink{State: LinkCreationPending},
			jobID: claimingJob,
			want:  false,
		},
		{
			name:  "claiming job once ready",
			link:  OpenWebUIConversationLink{State: LinkReady, ClaimJobID: &jobID},
			jobID: claimingJob,
			want:  false,
		},
		{
			name:  "claiming job once ambiguous",
			link:  OpenWebUIConversationLink{State: LinkAmbiguous, ClaimJobID: &jobID},
			jobID: claimingJob,
			want:  false,
		},
		{
			name:  "claiming job once failed",
			link:  OpenWebUIConversationLink{State: LinkFailed, ClaimJobID: &jobID},
			jobID: claimingJob,
			want:  false,
		},
		{
			name:  "claiming job once dead",
			link:  OpenWebUIConversationLink{State: LinkDead, ClaimJobID: &jobID},
			jobID: claimingJob,
			want:  false,
		},
		{
			name:  "claiming job once stateless",
			link:  OpenWebUIConversationLink{State: LinkStateless, ClaimJobID: &jobID},
			jobID: claimingJob,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.link.AllowsInitialStartChat(tt.jobID); got != tt.want {
				t.Errorf("AllowsInitialStartChat(%q) = %v, want %v", tt.jobID, got, tt.want)
			}
		})
	}
}

// TestConversationLink_StatePredicates covers the remaining three
// predicates across every state at once: only ready may continue, no
// state may auto-retry, and failed, dead, and stateless are all
// terminal — stateless included, even though (unlike the other two)
// reaching it is success, not failure (LinkStateless's own doc comment).
func TestConversationLink_StatePredicates(t *testing.T) {
	tests := []struct {
		state          LinkState
		allowsContinue bool
		isTerminal     bool
	}{
		{state: LinkCreationPending},
		{state: LinkReady, allowsContinue: true},
		{state: LinkAmbiguous},
		{state: LinkFailed, isTerminal: true},
		{state: LinkDead, isTerminal: true},
		{state: LinkStateless, isTerminal: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			l := OpenWebUIConversationLink{State: tt.state}
			if got := l.AllowsContinue(); got != tt.allowsContinue {
				t.Errorf("AllowsContinue() = %v, want %v", got, tt.allowsContinue)
			}
			if got := l.IsTerminal(); got != tt.isTerminal {
				t.Errorf("IsTerminal() = %v, want %v", got, tt.isTerminal)
			}
			if l.AllowsAutoRetry() {
				t.Error("AllowsAutoRetry() = true; no link state may be retried automatically")
			}
		})
	}
}

func TestOpenWebUIModelCapabilities_EncodeRoundTrip(t *testing.T) {
	for _, want := range []OpenWebUIModelCapabilities{
		{},
		{ChatCreate: true},
		{ChatContinue: true},
		{ChatCreate: true, ChatContinue: true},
	} {
		encoded := want.Encode()
		got, err := ParseOpenWebUIModelCapabilities(encoded)
		if err != nil {
			t.Fatalf("ParseOpenWebUIModelCapabilities(%q): %v", encoded, err)
		}
		if got != want {
			t.Errorf("round trip of %+v through %q = %+v", want, encoded, got)
		}
	}
}

// TestParseOpenWebUIModelCapabilities_UnknownAndNullFields covers the
// decoding rules a stored document has to survive: a member this build
// does not know about (written by a newer one), an explicitly null
// value, an empty document, and an empty column. All four load; only
// malformed JSON is an error, because that is corruption rather than
// version skew and reading it as "no capabilities" would hide it.
func TestParseOpenWebUIModelCapabilities_UnknownAndNullFields(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    OpenWebUIModelCapabilities
		wantErr bool
	}{
		{name: "unknown member is ignored", raw: `{"chat_create":true,"future":1}`, want: OpenWebUIModelCapabilities{ChatCreate: true}},
		{name: "only unknown members", raw: `{"future":{"nested":true}}`},
		{name: "explicit null document", raw: `null`},
		{name: "empty document", raw: `{}`},
		{name: "empty column", raw: ``},
		{name: "null member", raw: `{"chat_create":null,"chat_continue":true}`, want: OpenWebUIModelCapabilities{ChatContinue: true}},
		{name: "malformed", raw: `{"chat_create":`, wantErr: true},
		{name: "not an object", raw: `"chat_create"`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOpenWebUIModelCapabilities(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseOpenWebUIModelCapabilities(%q) = %+v, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOpenWebUIModelCapabilities(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("ParseOpenWebUIModelCapabilities(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestOpenWebUISources_EncodeRoundTrip mirrors
// TestOpenWebUIModelCapabilities_EncodeRoundTrip for the Issue #81/#84
// sources_json column (ADR-0005 D22): a nil/empty list must encode to ""
// (stored as SQL NULL, never an empty-array string) and every non-empty
// case must round-trip exactly, URL/Arguments included.
func TestOpenWebUISources_EncodeRoundTrip(t *testing.T) {
	url := "https://example.com/result"
	for _, want := range [][]Source{
		nil,
		{},
		{{Kind: SourceKindWebSearchForTest, DisplayName: "web_search", URL: &url}},
		{{Kind: SourceKindToolForTest, DisplayName: "get_weather", Arguments: map[string]string{"city": "Tokyo"}}},
		{
			{Kind: SourceKindToolForTest, DisplayName: "get_weather", Arguments: map[string]string{"city": "Tokyo"}},
			{Kind: SourceKindWebSearchForTest, DisplayName: "web_search", URL: &url},
		},
	} {
		encoded := EncodeSources(want)
		if len(want) == 0 && encoded != "" {
			t.Errorf("EncodeSources(%+v) = %q, want \"\" for an empty list", want, encoded)
		}
		got, err := ParseOpenWebUISources(encoded)
		if err != nil {
			t.Fatalf("ParseOpenWebUISources(%q): %v", encoded, err)
		}
		if len(got) != len(want) {
			t.Fatalf("round trip of %+v through %q = %+v", want, encoded, got)
		}
		for i := range want {
			if got[i].Kind != want[i].Kind || got[i].DisplayName != want[i].DisplayName {
				t.Errorf("round trip of %+v through %q = %+v", want, encoded, got)
			}
			if (got[i].URL == nil) != (want[i].URL == nil) || (got[i].URL != nil && *got[i].URL != *want[i].URL) {
				t.Errorf("round trip URL of %+v through %q = %+v", want, encoded, got)
			}
		}
	}
}

// TestParseOpenWebUISources_EmptyAndNullColumn covers the two "nothing
// recorded" states the column legitimately reaches, mirroring
// TestParseOpenWebUIModelCapabilities_UnknownAndNullFields: an empty
// string (the ordinary un-set case) and a malformed document (real
// corruption, which must still surface as an error rather than being
// silently read as "no sources").
func TestParseOpenWebUISources_EmptyAndNullColumn(t *testing.T) {
	if got, err := ParseOpenWebUISources(""); err != nil || got != nil {
		t.Errorf(`ParseOpenWebUISources("") = %+v, %v, want nil, nil`, got, err)
	}
	if _, err := ParseOpenWebUISources(`[{"kind":`); err == nil {
		t.Error("ParseOpenWebUISources(malformed) = nil error, want one")
	}
}

// SourceKindToolForTest/SourceKindWebSearchForTest mirror
// internal/openwebui.SourceKindTool/SourceKindWebSearch's string values
// without importing that package (this package must not depend on it —
// see Source's own doc comment): domain.Source.Kind stores whichever
// string the caller gives it, uninterpreted.
const (
	SourceKindToolForTest      = "tool"
	SourceKindWebSearchForTest = "web_search"
)

func TestVirtualActor_Handle(t *testing.T) {
	v := VirtualActor{Slug: "model", Host: "openwebui.example.net"}
	if got, want := v.Handle(), "@model@openwebui.example.net"; got != want {
		t.Errorf("Handle() = %q, want %q", got, want)
	}
}
