package webadmin

import (
	"context"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// RecordAction writes one web_admin_action_audit row attributing action
// on target to session's own credential (ADR-0010 Decision 8). It is
// deliberately NOT part of the same transaction as the
// internal/miauth.Service call the caller already made: internal/miauth.
// Service's methods are each their own transaction boundary and take no
// audit-writing hook (this phase does not modify that package, full
// stop), so true same-write atomicity isn't available without changing
// internal/miauth's own tested surface. Callers must call this AFTER the
// underlying action has already succeeded, and must not fail the HTTP
// response if this call itself fails — log it and move on, the same
// "commit the important thing first, best-effort the rest" shape
// cmd/server's fetchAndSetSourceFavicon already establishes for a
// different feature (Issue #134): the action already happened and is
// real regardless of whether this bookkeeping write also succeeds.
func (s *Service) RecordAction(ctx context.Context, session domain.WebAdminSession, action domain.WebAdminAction, target string, before, after *string) error {
	entry := domain.WebAdminActionAuditEntry{
		ID: domain.NewID(), OwnerActorID: session.OwnerActorID, CredentialID: session.CredentialID,
		Action: action, Target: target, BeforeValue: before, AfterValue: after, ChangedAt: s.clock.Now(),
	}
	if err := s.repos.WebAdminActionAudit.Record(ctx, entry); err != nil {
		return fmt.Errorf("webadmin: record action audit: %w", err)
	}
	return nil
}
