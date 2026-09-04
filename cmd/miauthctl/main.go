// Command miauthctl lets an operator with host access inspect and decide
// local MiAuth requests and revoke local API tokens (ADR-0002).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "list", "approve", "reject", "tokens", "revoke":
	default:
		return usageError()
	}
	cfg, err := config.Load(config.LoadOptions{ConfigFilePath: configFilePath()})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{
		Path: cfg.DB.Path, BusyTimeout: cfg.DB.BusyTimeout, MaxOpenConns: cfg.DB.MaxOpenConns,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	svc := miauth.NewService(db, db.Repos, miauth.Config{})

	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: miauthctl list")
		}
		return listSessions(ctx, svc, stdout, time.Now().UTC())
	case "approve":
		return approveSession(ctx, svc, args[1:], stdin, stdout, time.Now().UTC())
	case "reject":
		if len(args) != 2 {
			return fmt.Errorf("usage: miauthctl reject <session-id>")
		}
		if err := svc.RejectSession(ctx, args[1]); err != nil {
			return fmt.Errorf("reject session: %w", err)
		}
		fmt.Fprintln(stdout, "Rejected MiAuth session.")
		return nil
	case "tokens":
		if len(args) != 1 {
			return fmt.Errorf("usage: miauthctl tokens")
		}
		return listTokens(ctx, svc, stdout)
	case "revoke":
		if len(args) != 2 {
			return fmt.Errorf("usage: miauthctl revoke <token-id>")
		}
		if err := svc.RevokeAPIToken(ctx, args[1]); err != nil {
			return fmt.Errorf("revoke token: %w", err)
		}
		fmt.Fprintln(stdout, "Revoked API token.")
		return nil
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: miauthctl <list|approve|reject|tokens|revoke> [arguments]")
}

func listSessions(ctx context.Context, svc *miauth.Service, out io.Writer, now time.Time) error {
	sessions, err := svc.ListPendingSessions(ctx)
	if err != nil {
		return fmt.Errorf("list pending sessions: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tCREATED_AT\tAGE\tPERMISSIONS\tCALLBACK")
	for _, session := range sessions {
		age := now.Sub(session.CreatedAt)
		if age < 0 {
			age = 0
		}
		callback := "-"
		if session.ClientCallback != nil {
			callback = *session.ClientCallback
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			safeCell(session.RouteSessionID), session.CreatedAt.UTC().Format(time.RFC3339),
			age.Round(time.Second), safeCell(session.RequestedPermissions), safeCell(callback))
	}
	return tw.Flush()
}

func approveSession(ctx context.Context, svc *miauth.Service, args []string, in io.Reader, out io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return fmt.Errorf("usage: miauthctl approve [--yes] <session-id>")
	}
	sessionID := fs.Arg(0)
	sessions, err := svc.ListPendingSessions(ctx)
	if err != nil {
		return fmt.Errorf("inspect session: %w", err)
	}
	var target *domain.LocalMiAuthSession
	for i := range sessions {
		if sessions[i].RouteSessionID == sessionID {
			target = &sessions[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("approve session: %w", miauth.ErrSessionUnavailable)
	}
	age := now.Sub(target.CreatedAt)
	if age < 0 {
		age = 0
	}
	callback := "-"
	if target.ClientCallback != nil {
		callback = *target.ClientCallback
	}
	fmt.Fprintf(out, "Session: %s\nCreated: %s (%s ago)\nPermissions: %s\nCallback: %s\n",
		safeCell(target.RouteSessionID), target.CreatedAt.UTC().Format(time.RFC3339), age.Round(time.Second),
		safeCell(target.RequestedPermissions), safeCell(callback))
	if !*yes {
		fmt.Fprint(out, "Approve this exact session? Type yes to continue: ")
		scanner := bufio.NewScanner(in)
		if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "yes" {
			return errors.New("approval cancelled")
		}
	}
	if err := svc.ApproveSession(ctx, sessionID); err != nil {
		return fmt.Errorf("approve session: %w", err)
	}
	fmt.Fprintln(out, "Approved MiAuth session.")
	return nil
}

func listTokens(ctx context.Context, svc *miauth.Service, out io.Writer) error {
	tokens, err := svc.ListAPITokens(ctx)
	if err != nil {
		return fmt.Errorf("list API tokens: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TOKEN_ID\tCREATED_AT\tSCOPES\tREVOKED_AT\tLAST_USED_AT")
	for _, token := range tokens {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", safeCell(token.ID),
			token.CreatedAt.UTC().Format(time.RFC3339), safeCell(token.Scopes), formatOptionalTime(token.RevokedAt),
			formatOptionalTime(token.LastUsedAt))
	}
	return tw.Flush()
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

// safeCell prevents untrusted session fields from injecting terminal
// controls or breaking the tabular layout.
func safeCell(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, value)
}

func configFilePath() string {
	if value, ok := os.LookupEnv("CONFIG_FILE"); ok && value != "" {
		return value
	}
	return ".env"
}
