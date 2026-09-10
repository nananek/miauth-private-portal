package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/webadmin"
)

// newWebAdminService constructs the webadmin.Service the web-login
// subcommand family needs, deriving RPID from LOCAL_ORIGIN's hostname
// the same way cmd/server does (see webadmin.Config.RPID's own doc
// comment). Unlike cmd/server (which degrades to leaving the admin Web
// UI unavailable rather than refusing to start over this), a CLI
// operator who explicitly ran web-login needs a clear, actionable error
// instead of a silent no-op — WebAuthn's RPID must be a real domain, not
// an IP address, which some LOCAL_ORIGIN values (local/IP-based
// deployments) can never satisfy.
func newWebAdminService(db *sqlite.DB, cfg *config.Config) (*webadmin.Service, error) {
	rpID, err := url.Parse(cfg.Auth.LocalOrigin)
	if err != nil {
		// Unreachable in practice: internal/config.Validate already
		// enforces LOCAL_ORIGIN is a well-formed origin at startup.
		return nil, fmt.Errorf("parse LOCAL_ORIGIN: %w", err)
	}
	svc, err := webadmin.NewService(db, db.Repos, webadmin.Config{
		RPID: rpID.Hostname(), RPDisplayName: "miauth-private-portal", RPOrigins: []string{cfg.Auth.LocalOrigin},
		OwnerUsername: cfg.Auth.OwnerUsername, OwnerDisplayName: cfg.Auth.OwnerDisplayName,
	})
	if err != nil {
		return nil, &cliExitError{code: exitValidation, err: fmt.Errorf(
			"web-login requires LOCAL_ORIGIN's host to be a real domain (WebAuthn cannot use an IP address as its Relying Party ID): %w", err,
		)}
	}
	return svc, nil
}

// runWebLogin dispatches the web-login subcommand family. Phase 1 (Issue
// #136) adds exactly one: "issue". list-credentials/revoke-credential
// are Phase 2's scope (plan-136 §3, Phase 2) — deliberately not stubbed
// here even as a "not yet implemented" case, to keep this PR's surface
// exactly Phase 1's.
func runWebLogin(ctx context.Context, svc *webadmin.Service, db *sqlite.DB, localOrigin string, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl web-login issue")}
	}
	switch args[0] {
	case "issue":
		return webLoginIssue(ctx, svc, db, localOrigin, stdout)
	default:
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl web-login issue")}
	}
}

// webLoginIssue is `miauthctl web-login issue`: mints a single-use
// bootstrap token bound to the Owner actor and prints the one-time setup
// URL. The raw token value is never logged, never written anywhere but
// this one stdout line — the same redaction rule as every other
// raw-secret CLI output in this codebase.
func webLoginIssue(ctx context.Context, svc *webadmin.Service, db *sqlite.DB, localOrigin string, stdout io.Writer) error {
	ownerID, err := ownerActorID(ctx, db)
	if err != nil {
		return err
	}
	raw, tok, err := svc.IssueBootstrapToken(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("issue bootstrap token: %w", err)
	}
	fmt.Fprintf(stdout, "Open this URL in a browser to register a passkey (valid until %s, single use):\n",
		tok.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(stdout, "%s/admin/setup?token=%s\n", localOrigin, raw)
	return nil
}
