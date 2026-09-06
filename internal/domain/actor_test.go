package domain

import "testing"

// TestActorCapabilityPredicates pins the capability rules Issue #52's
// VirtualActor work depends on: which actor types may log in, which may
// be bound by a local MiAuth approval, which are projected to Aria as
// remote users, and — for every type, including the owner — that none
// may hold a credential.
//
// These are derived from ActorType rather than stored as columns, so
// this table is the single place the rules are written down; a new actor
// type that forgets to answer one of them shows up here.
func TestActorCapabilityPredicates(t *testing.T) {
	tests := []struct {
		actorType          ActorType
		isLoginable        bool
		canMiAuth          bool
		canOwnSecret       bool
		isRemote           bool
		isPresentationOnly bool
	}{
		{actorType: ActorOwner, isLoginable: true, canMiAuth: true},
		{actorType: ActorAssistant, isPresentationOnly: true},
		{actorType: ActorSystem, isPresentationOnly: true},
		{actorType: ActorOpenWebUIModel, isRemote: true, isPresentationOnly: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.actorType), func(t *testing.T) {
			a := Actor{ID: "actor-id", Type: tt.actorType}
			if got := a.IsLoginable(); got != tt.isLoginable {
				t.Errorf("IsLoginable() = %v, want %v", got, tt.isLoginable)
			}
			if got := a.CanMiAuth(); got != tt.canMiAuth {
				t.Errorf("CanMiAuth() = %v, want %v", got, tt.canMiAuth)
			}
			if got := a.CanOwnSecret(); got != tt.canOwnSecret {
				t.Errorf("CanOwnSecret() = %v, want %v", got, tt.canOwnSecret)
			}
			if got := a.IsRemote(); got != tt.isRemote {
				t.Errorf("IsRemote() = %v, want %v", got, tt.isRemote)
			}
			if got := a.IsPresentationOnly(); got != tt.isPresentationOnly {
				t.Errorf("IsPresentationOnly() = %v, want %v", got, tt.isPresentationOnly)
			}
		})
	}
}

// TestActorCapabilityPredicates_UnknownTypeIsInert covers the defensive
// direction: a row whose actor_type this build does not know about (a
// database written by a newer binary, say) must fall through to the
// least-privileged answers rather than accidentally matching the owner.
func TestActorCapabilityPredicates_UnknownTypeIsInert(t *testing.T) {
	a := Actor{ID: "actor-id", Type: ActorType("something_new")}
	if a.IsLoginable() || a.CanMiAuth() || a.CanOwnSecret() || a.IsRemote() {
		t.Errorf("unknown actor type should have no capabilities, got %+v", a)
	}
	if !a.IsPresentationOnly() {
		t.Error("unknown actor type should be presentation-only")
	}
}
