package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
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
		// ReloadSessionTTL is left nil: sessionTTL is only ever consulted
		// by FinishLogin, an HTTP-only code path (POST
		// /admin/login/finish) this CLI process never runs — web-login
		// list-credentials/revoke-credential need no session TTL at all.
		// The static SessionTTL default below still matters for
		// Config.Redacted()-style consistency even though nothing here
		// reads it back.
		SessionCookie: webadmin.SessionCookieConfig{SessionTTL: cfg.WebAdmin.SessionTTL},
	})
	if err != nil {
		return nil, &cliExitError{code: exitValidation, err: fmt.Errorf(
			"web-login requires LOCAL_ORIGIN's host to be a real domain (WebAuthn cannot use an IP address as its Relying Party ID): %w", err,
		)}
	}
	return svc, nil
}

// runWebLogin dispatches the web-login subcommand family. Phase 1
// (Issue #136) added "issue"; Phase 2 adds "list-credentials" and
// "revoke-credential" for recovery (plan-136 §3, Phase 2; plan-136-phase2
// §8).
func runWebLogin(ctx context.Context, svc *webadmin.Service, db *sqlite.DB, localOrigin string, args []string, stdout io.Writer) error {
	const usage = "usage: miauthctl web-login <issue|list-credentials|revoke-credential> [arguments]"
	if len(args) == 0 {
		return &cliExitError{code: exitUsage, err: errors.New(usage)}
	}
	switch args[0] {
	case "issue":
		return webLoginIssue(ctx, svc, db, localOrigin, stdout)
	case "list-credentials":
		return webLoginListCredentials(ctx, svc, db, stdout)
	case "revoke-credential":
		return webLoginRevokeCredential(ctx, svc, args[1:], stdout)
	default:
		return &cliExitError{code: exitUsage, err: errors.New(usage)}
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

// webLoginListCredentials is `miauthctl web-login list-credentials`: a
// tabwriter table (ID, CREATED_AT, LAST_USED_AT), safeCell-sanitized,
// mirroring listTokens' own shape exactly — no CredentialJSON dump,
// matching this CLI's existing "never print more than an operator needs
// to identify a row" restraint (jobsctl/openwebuictl's own precedent).
func webLoginListCredentials(ctx context.Context, svc *webadmin.Service, db *sqlite.DB, stdout io.Writer) error {
	ownerID, err := ownerActorID(ctx, db)
	if err != nil {
		return err
	}
	creds, err := svc.ListCredentials(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("list web admin credentials: %w", err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCREATED_AT\tLAST_USED_AT")
	for _, cred := range creds {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", safeCell(cred.ID),
			cred.CreatedAt.UTC().Format(time.RFC3339), formatOptionalTime(cred.LastUsedAt))
	}
	return tw.Flush()
}

// webLoginRevokeCredential is `miauthctl web-login revoke-credential
// <id>`. Reports both what it deleted and how many active sessions it
// cascaded onto, so an operator revoking a lost device's credential can
// see that device's session was actually cut off, not just that future
// logins from it will fail.
func webLoginRevokeCredential(ctx context.Context, svc *webadmin.Service, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl web-login revoke-credential <id>")}
	}
	result, err := svc.RevokeCredential(ctx, args[0])
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return &cliExitError{code: exitNotFound, err: fmt.Errorf("credential %s does not exist", args[0])}
		}
		return fmt.Errorf("revoke credential: %w", err)
	}
	fmt.Fprintf(stdout, "Revoked credential %s (and %d active session(s)).\n", safeCell(args[0]), result.RevokedSessions)
	return nil
}
