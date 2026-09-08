// Package userlist implements the users/lists/* use cases: named,
// owner-curated groupings of local actors, and resolving one list's
// membership for notes/user-list-timeline (Issue #115). It depends only
// on internal/domain, mirroring internal/timeline's own layer boundary
// (AGENTS.md: use-case code must not depend on HTTP handlers, SQLite, or
// an LLM provider).
package userlist

import (
	"context"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// Config supplies optional userlist service dependencies.
type Config struct {
	// Clock defaults to the real wall clock when nil.
	Clock Clock
}

// Service enforces user-list business rules on top of
// domain.UserListRepository. Unlike internal/timeline.Service it holds
// no UnitOfWork: no method here composes a write across more than one
// repository, so there is no multi-repository atomicity to buy (see
// domain.UserListRepository's own doc comment).
type Service struct {
	repos domain.Repos
	clock Clock
}

// NewService builds a userlist Service. repos commonly comes from one
// storage adapter, but no concrete adapter type crosses this package
// boundary.
func NewService(repos domain.Repos, cfg Config) *Service {
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Service{repos: repos, clock: clock}
}

// Create makes a new, empty list named name.
func (s *Service) Create(ctx context.Context, name string) (domain.UserList, error) {
	now := s.clock.Now().UTC()
	l := domain.UserList{ID: domain.NewID(), Name: name, CreatedAt: now, UpdatedAt: now}
	if err := s.repos.UserLists.Create(ctx, l); err != nil {
		return domain.UserList{}, err
	}
	return l, nil
}

// Get returns one list by ID, not including its membership — callers
// that need userIds call MemberActorIDs separately (mirroring
// timeline.Service.GetEntry/AttachedFiles' own split).
func (s *Service) Get(ctx context.Context, listID string) (domain.UserList, error) {
	return s.repos.UserLists.Get(ctx, listID)
}

// ListAll returns every list this (single-owner) deployment has created.
func (s *Service) ListAll(ctx context.Context) ([]domain.UserList, error) {
	return s.repos.UserLists.ListAll(ctx)
}

// Update applies a partial change to listID: a nil name or isPublic
// leaves that field unchanged.
func (s *Service) Update(ctx context.Context, listID string, name *string, isPublic *bool) (domain.UserList, error) {
	return s.repos.UserLists.Update(ctx, listID, name, isPublic, s.clock.Now().UTC())
}

// Delete removes listID and its membership.
func (s *Service) Delete(ctx context.Context, listID string) error {
	return s.repos.UserLists.Delete(ctx, listID)
}

// AddMember adds actorID to listID's membership, idempotently (see
// domain.UserListRepository.AddMember). It checks listID exists first
// (mirroring timeline's createReplyEntry checking its parent entry
// exists before writing) so an unknown listID reports domain.ErrNotFound
// instead of a raw foreign-key constraint error; it does not itself
// validate actorID — internal/httpserver checks that against the known
// actor set before ever calling this (plan-115 §2.2).
func (s *Service) AddMember(ctx context.Context, listID, actorID string) error {
	if _, err := s.repos.UserLists.Get(ctx, listID); err != nil {
		return err
	}
	return s.repos.UserLists.AddMember(ctx, listID, actorID, s.clock.Now().UTC())
}

// RemoveMember removes actorID from listID's membership, if present. It
// checks listID exists first, the same as AddMember.
func (s *Service) RemoveMember(ctx context.Context, listID, actorID string) error {
	if _, err := s.repos.UserLists.Get(ctx, listID); err != nil {
		return err
	}
	return s.repos.UserLists.RemoveMember(ctx, listID, actorID)
}

// MemberActorIDs returns listID's member actor IDs in the order they
// were added.
func (s *Service) MemberActorIDs(ctx context.Context, listID string) ([]string, error) {
	return s.repos.UserLists.MemberActorIDs(ctx, listID)
}
