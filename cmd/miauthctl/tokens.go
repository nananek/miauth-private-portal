package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// runTokens dispatches the tokens subcommand family. Bare "tokens" (no
// further args) keeps its pre-existing meaning: list issued API tokens.
func runTokens(ctx context.Context, svc *miauth.Service, db *sqlite.DB, args []string, stdout io.Writer, now time.Time) error {
	if len(args) == 0 {
		return listTokens(ctx, svc, stdout)
	}
	switch args[0] {
	case "reflect-scopes":
		return tokensReflectScopes(ctx, svc, db, args[1:], stdout, now)
	default:
		return &cliExitError{code: exitUsage, err: errors.New(
			"usage: miauthctl tokens [reflect-scopes [--token-id <id> | --all] [--dry-run]]",
		)}
	}
}

// tokensReflectScopes is Issue #133's `miauthctl tokens reflect-scopes`:
// recomputes a token's (or every non-revoked token's) effective scopes
// against the current grantableScopes and writes any newly-granted
// scopes back, without requiring a fresh Aria login. See
// miauth.Service.ReflectScopes' doc comment for the additive-only
// design.
func tokensReflectScopes(ctx context.Context, svc *miauth.Service, db *sqlite.DB, args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("reflect-scopes", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tokenID := fs.String("token-id", "", "")
	all := fs.Bool("all", false, "")
	dryRun := fs.Bool("dry-run", false, "")
	usage := "usage: miauthctl tokens reflect-scopes [--token-id <id> | --all] [--dry-run]"
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return &cliExitError{code: exitUsage, err: errors.New(usage)}
	}
	if (*tokenID == "") == !*all {
		return &cliExitError{code: exitUsage, err: errors.New(usage)}
	}

	if *all {
		return tokensReflectScopesAll(ctx, svc, db, stdout, *dryRun)
	}
	return tokensReflectScopesOne(ctx, svc, db, *tokenID, stdout, *dryRun)
}

func tokensReflectScopesOne(ctx context.Context, svc *miauth.Service, db *sqlite.DB, tokenID string, stdout io.Writer, dryRun bool) error {
	if dryRun {
		result, err := svc.PreviewReflectScopes(ctx, tokenID)
		if err != nil {
			return reflectScopesError(tokenID, err)
		}
		fmt.Fprint(stdout, reflectScopesLine(tokenID, result, true))
		return nil
	}

	ownerID, err := ownerActorID(ctx, db)
	if err != nil {
		return err
	}
	result, err := svc.ReflectScopes(ctx, tokenID, ownerID)
	if err != nil {
		return reflectScopesError(tokenID, err)
	}
	fmt.Fprint(stdout, reflectScopesLine(tokenID, result, false))
	return nil
}

func tokensReflectScopesAll(ctx context.Context, svc *miauth.Service, db *sqlite.DB, stdout io.Writer, dryRun bool) error {
	tokens, err := svc.ListAPITokens(ctx)
	if err != nil {
		return fmt.Errorf("list API tokens: %w", err)
	}

	var ownerID string
	if !dryRun {
		ownerID, err = ownerActorID(ctx, db)
		if err != nil {
			return err
		}
	}

	var updated, unchanged, failed int
	for _, tok := range tokens {
		if tok.RevokedAt != nil {
			continue
		}
		var result miauth.ReflectScopesResult
		var itemErr error
		if dryRun {
			result, itemErr = svc.PreviewReflectScopes(ctx, tok.ID)
		} else {
			result, itemErr = svc.ReflectScopes(ctx, tok.ID, ownerID)
		}
		if itemErr != nil {
			failed++
			fmt.Fprintf(stdout, "Token %s: FAILED: %v\n", safeCell(tok.ID), reflectScopesError(tok.ID, itemErr))
			continue
		}
		if result.Changed {
			updated++
		} else {
			unchanged++
		}
		fmt.Fprint(stdout, reflectScopesLine(tok.ID, result, dryRun))
	}

	fmt.Fprintf(stdout, "Reflected %d token(s): %d updated, %d unchanged, %d failed.\n",
		updated+unchanged+failed, updated, unchanged, failed)
	if failed > 0 {
		return &cliExitError{code: exitValidation, err: fmt.Errorf("%d token(s) failed to reflect scopes; see above", failed)}
	}
	return nil
}

// reflectScopesLine renders one token's reflect-scopes outcome. dryRun
// only changes the wording ("would update"/"no new scopes"), never the
// underlying data — nothing has been written either way when dryRun is
// true.
func reflectScopesLine(tokenID string, result miauth.ReflectScopesResult, dryRun bool) string {
	id := safeCell(tokenID)
	if !result.Changed {
		return fmt.Sprintf("Token %s: no new scopes (already up to date).\n", id)
	}
	verb := "scopes updated"
	if dryRun {
		verb = "scopes would be updated"
	}
	return fmt.Sprintf("Token %s: %s %q -> %q.\n", id, verb, safeCell(result.OldScopes), safeCell(result.NewScopes))
}

// reflectScopesError maps miauth.Service's ReflectScopes/PreviewReflectScopes
// sentinel errors to this CLI's exit-code convention (config.go's, not
// main.go's older flat exitUsage-only one — reflect-scopes has the same
// class of outcomes as config set: usage error / not-found / already-
// revoked / no recoverable session).
func reflectScopesError(tokenID string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return &cliExitError{code: exitNotFound, err: fmt.Errorf("token %s does not exist", tokenID)}
	case errors.Is(err, miauth.ErrTokenRevoked):
		return &cliExitError{code: exitValidation, err: fmt.Errorf(
			"token %s is revoked; reflect-scopes only applies to active tokens", tokenID,
		)}
	case errors.Is(err, miauth.ErrOriginatingSessionGone):
		return &cliExitError{code: exitValidation, err: fmt.Errorf(
			"token %s has no recoverable originating session; its requested_permissions cannot be replayed — re-issue a new token via a fresh Aria login and miauthctl approve instead", tokenID,
		)}
	default:
		return fmt.Errorf("reflect scopes for %s: %w", tokenID, err)
	}
}
