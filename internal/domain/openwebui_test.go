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
var allLinkStates = []LinkState{LinkCreationPending, LinkReady, LinkAmbiguous, LinkFailed, LinkDead}

var allLinkEvents = []LinkEvent{
	LinkEventConfirmed,
	LinkEventDefinitiveFailure,
	LinkEventUncertainCreation,
	LinkEventUncertainContinuation,
	LinkEventOwnerConfirmed,
	LinkEventOwnerAbandoned,
}

// TestLinkTransition_ExhaustiveTable walks every (state, event) pair and
// checks it against the roadmap's diagram: the six listed edges are
// allowed and reach the stated state, and all twenty-four other pairs
// are rejected. In particular a failed or dead link accepts nothing, and
// no event other than an owner's may move an ambiguous link.
func TestLinkTransition_ExhaustiveTable(t *testing.T) {
	allowed := map[LinkState]map[LinkEvent]LinkState{
		LinkCreationPending: {
			LinkEventConfirmed:         LinkReady,
			LinkEventDefinitiveFailure: LinkFailed,
			LinkEventUncertainCreation: LinkAmbiguous,
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
// state may auto-retry, and failed and dead are the terminal pair.
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

func TestVirtualActor_Handle(t *testing.T) {
	v := VirtualActor{Slug: "model", Host: "openwebui.example.net"}
	if got, want := v.Handle(), "@model@openwebui.example.net"; got != want {
		t.Errorf("Handle() = %q, want %q", got, want)
	}
}
