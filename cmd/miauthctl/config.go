package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// Exit codes for the config subcommand (ADR-0006 §2-5): 0 success (the
// default for a nil error), 1 usage/general error, 2 validation error
// (an unstorable or malformed value), 3 an unrecognized or never-set
// key, 4 an optimistic-lock (compare-and-set) conflict from a
// concurrent update. Every other miauthctl subcommand keeps main's
// existing uniform exit(1)-on-any-error behavior; only a *cliExitError
// carries a different code.
const (
	exitUsage      = 1
	exitValidation = 2
	exitNotFound   = 3
	exitConflict   = 4
)

// cliExitError pairs an error with the process exit code it should
// produce.
type cliExitError struct {
	code int
	err  error
}

func (e *cliExitError) Error() string { return e.err.Error() }
func (e *cliExitError) Unwrap() error { return e.err }

// exitCodeFor returns err's cliExitError code, or exitUsage for any
// other error (matching every non-config miauthctl subcommand's
// existing plain os.Exit(1) behavior).
func exitCodeFor(err error) int {
	var e *cliExitError
	if errors.As(err, &e) {
		return e.code
	}
	return exitUsage
}

func runConfig(ctx context.Context, db *sqlite.DB, cfg *config.Config, args []string, stdout io.Writer, now time.Time) error {
	if len(args) == 0 {
		return configUsageError()
	}
	switch args[0] {
	case "list":
		return configList(ctx, db, cfg, args[1:], stdout)
	case "get":
		return configGet(ctx, db, cfg, args[1:], stdout)
	case "set":
		return configSet(ctx, db, args[1:], stdout, now)
	case "unset":
		return configUnset(ctx, db, args[1:], stdout, now)
	case "validate":
		return configValidate(args[1:], stdout)
	case "export":
		return configExport(ctx, db, args[1:], stdout)
	case "import":
		return configImport(ctx, db, cfg, args[1:], stdout, now)
	case "history":
		return configHistory(ctx, db, args[1:], stdout)
	case "rollback":
		return configRollback(ctx, db, args[1:], stdout, now)
	default:
		return configUsageError()
	}
}

func configUsageError() error {
	return &cliExitError{code: exitUsage, err: errors.New(
		"usage: miauthctl config <list|get|set|unset|validate|export|import|history|rollback> [arguments]",
	)}
}

func unknownKeyError(key string) error {
	return &cliExitError{code: exitNotFound, err: fmt.Errorf("unknown config key %s", key)}
}

// ownerActorID resolves the single owner actor's id for
// AppConfigEntry.UpdatedBy/AppConfigAuditEntry.ChangedBy, the same way
// cmd/openwebuictl already does (ADR-0002: a CLI operator with host
// access is always attributed to the one local owner actor).
func ownerActorID(ctx context.Context, db *sqlite.DB) (string, error) {
	owner, err := db.Actors.GetByType(ctx, domain.ActorOwner)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", &cliExitError{code: exitUsage, err: errors.New(
				"no owner actor is bound yet; complete MiAuth binding (miauthctl approve) before using config",
			)}
		}
		return "", fmt.Errorf("resolve owner actor: %w", err)
	}
	return owner.ID, nil
}

// --- list / get ---------------------------------------------------

// configRow is one key's effective value and where it came from: "db"
// (an ADR-0006 app_config override), or one of internal/config.Source's
// bootstrap values ("env", "file", "default"). A secret key's Value is
// always cfg.Redacted()'s <set>/<unset> marker, never the real value
// (Issue #76 AC3).
type configRow struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

func configList(ctx context.Context, db *sqlite.DB, cfg *config.Config, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config list [--json]")}
	}
	rows, err := buildConfigRows(ctx, db, cfg, config.KnownKeys())
	if err != nil {
		return err
	}
	return writeConfigRows(stdout, rows, *asJSON)
}

func configGet(ctx context.Context, db *sqlite.DB, cfg *config.Config, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config get [--json] <key>")}
	}
	key := fs.Arg(0)
	if !slices.Contains(config.KnownKeys(), key) {
		return unknownKeyError(key)
	}
	rows, err := buildConfigRows(ctx, db, cfg, []string{key})
	if err != nil {
		return err
	}
	return writeConfigRows(stdout, rows, *asJSON)
}

func buildConfigRows(ctx context.Context, db *sqlite.DB, cfg *config.Config, keys []string) ([]configRow, error) {
	redacted := cfg.Redacted()
	sources, err := config.Sources(config.LoadOptions{ConfigFilePath: configFilePath()})
	if err != nil {
		return nil, fmt.Errorf("resolve config sources: %w", err)
	}
	dbEntries, err := db.Config.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list db config overrides: %w", err)
	}
	dbByKey := make(map[string]domain.AppConfigEntry, len(dbEntries))
	for _, e := range dbEntries {
		dbByKey[e.Key] = e
	}

	rows := make([]configRow, 0, len(keys))
	for _, key := range keys {
		// dbByKey can only ever hold a db-eligible key: nothing else can
		// have been written through ConfigRepository.Set (secret keys
		// are rejected by configSet before it ever calls Set; the
		// startup auto-seed and config import both iterate
		// DBEligibleKeys() only).
		if entry, ok := dbByKey[key]; ok {
			rows = append(rows, configRow{Key: key, Value: entry.Value, Source: "db"})
			continue
		}
		rows = append(rows, configRow{Key: key, Value: redacted[key], Source: string(sources[key])})
	}
	return rows, nil
}

func writeConfigRows(stdout io.Writer, rows []configRow, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Key, safeCell(r.Value), r.Source)
	}
	return tw.Flush()
}

// --- set / unset ----------------------------------------------------

func configSet(ctx context.Context, db *sqlite.DB, args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config set [--dry-run] <key> <value>")}
	}
	key, value := fs.Arg(0), fs.Arg(1)

	if !slices.Contains(config.KnownKeys(), key) {
		return unknownKeyError(key)
	}
	if config.IsSecretKey(key) {
		return &cliExitError{code: exitValidation, err: fmt.Errorf(
			"%s is a secret key and is never stored in the database (ADR-0005 D10); set it via the config file/environment and restart instead", key,
		)}
	}
	if !config.IsDBEligibleKey(key) {
		return &cliExitError{code: exitValidation, err: fmt.Errorf(
			"%s is bootstrap-only and cannot be changed without a restart (ADR-0006)", key,
		)}
	}
	if err := config.ValidateKeyValue(key, value); err != nil {
		return &cliExitError{code: exitValidation, err: err}
	}

	if *dryRun {
		fmt.Fprintf(stdout, "OK: %s=%s is a valid value (dry run, nothing written).\n", key, value)
		return nil
	}

	ownerID, err := ownerActorID(ctx, db)
	if err != nil {
		return err
	}

	var oldValue *string
	expectedVersion := 0
	if existing, err := db.Config.Get(ctx, key); err == nil {
		v := existing.Value
		oldValue = &v
		expectedVersion = existing.Version
	} else if !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("read current value: %w", err)
	}

	writeErr := db.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Config.Set(ctx, key, value, expectedVersion, ownerID, now); err != nil {
			return err
		}
		return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
			ID: domain.NewID(), Key: key, OldValue: oldValue, NewValue: &value,
			Version: expectedVersion + 1, ChangedAt: now, ChangedBy: ownerID,
		})
	})
	if writeErr != nil {
		if errors.Is(writeErr, domain.ErrConflict) {
			return &cliExitError{code: exitConflict, err: fmt.Errorf(
				"%s was changed concurrently; re-read it with \"config get %s\" and try again", key, key,
			)}
		}
		return fmt.Errorf("set %s: %w", key, writeErr)
	}
	fmt.Fprintf(stdout, "Set %s (version %d).\n", key, expectedVersion+1)
	return nil
}

func configUnset(ctx context.Context, db *sqlite.DB, args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("unset", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config unset <key>")}
	}
	key := fs.Arg(0)
	if !slices.Contains(config.KnownKeys(), key) {
		return unknownKeyError(key)
	}

	existing, err := db.Config.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return &cliExitError{code: exitNotFound, err: fmt.Errorf("%s has no database override to unset", key)}
	}
	if err != nil {
		return fmt.Errorf("read current value: %w", err)
	}

	ownerID, err := ownerActorID(ctx, db)
	if err != nil {
		return err
	}

	oldValue := existing.Value
	writeErr := db.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Config.Unset(ctx, key); err != nil {
			return err
		}
		return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
			ID: domain.NewID(), Key: key, OldValue: &oldValue, NewValue: nil,
			Version: 0, ChangedAt: now, ChangedBy: ownerID,
		})
	})
	if writeErr != nil {
		if errors.Is(writeErr, domain.ErrNotFound) {
			return &cliExitError{code: exitNotFound, err: fmt.Errorf("%s has no database override to unset", key)}
		}
		return fmt.Errorf("unset %s: %w", key, writeErr)
	}
	fmt.Fprintf(stdout, "Unset %s; it now falls back to the bootstrap value.\n", key)
	return nil
}

// --- validate ---------------------------------------------------------

// configValidate checks either one key=value pair (ValidateKeyValue) or,
// with --file, a whole dotenv-style file: the latter delegates entirely
// to config.Load with an empty Getenv, so a file's own "unset key means
// default" semantics match Load's real behavior exactly, rather than
// re-implementing it against ValidateKeyValue's stricter single-pair
// rules (see ValidateKeyValue's own doc comment).
func configValidate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "")
	if err := fs.Parse(args); err != nil {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config validate [--file <path>] | <key> <value>")}
	}

	if *file != "" {
		if fs.NArg() != 0 {
			return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config validate --file <path>")}
		}
		noEnv := func(string) (string, bool) { return "", false }
		if _, err := config.Load(config.LoadOptions{ConfigFilePath: *file, Getenv: noEnv}); err != nil {
			return &cliExitError{code: exitValidation, err: err}
		}
		fmt.Fprintf(stdout, "OK: %s is a valid configuration file.\n", *file)
		return nil
	}

	if fs.NArg() != 2 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config validate <key> <value>")}
	}
	key, value := fs.Arg(0), fs.Arg(1)
	if !slices.Contains(config.KnownKeys(), key) {
		return unknownKeyError(key)
	}
	if err := config.ValidateKeyValue(key, value); err != nil {
		return &cliExitError{code: exitValidation, err: err}
	}
	fmt.Fprintf(stdout, "OK: %s=%s is valid.\n", key, value)
	return nil
}

// --- export / import ---------------------------------------------------

func configExport(ctx context.Context, db *sqlite.DB, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config export [--json]")}
	}
	entries, err := db.Config.List(ctx)
	if err != nil {
		return fmt.Errorf("list db config overrides: %w", err)
	}
	if *asJSON {
		out := make(map[string]string, len(entries))
		for _, e := range entries {
			out[e.Key] = e.Value
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	for _, e := range entries {
		fmt.Fprintf(stdout, "%s=%s\n", e.Key, e.Value)
	}
	return nil
}

// configImport is the manual counterpart to the startup auto-seed
// (ADR-0006 §2-4): for every db-eligible key with no existing app_config
// row, write one from a chosen source — an existing row is never
// overwritten, so re-running import after fixing a bad entry is always
// safe. The three sources are mutually exclusive: no flag reads the
// currently resolved bootstrap config (file+env+default merged, exactly
// what a fresh startup would auto-seed from); --from-env reads the
// process environment directly, ignoring the config file and defaults
// (capturing only what is explicitly overridden right now); --file
// reads a given dotenv-style file directly, for importing another
// host's settings when moving to a new one.
func configImport(ctx context.Context, db *sqlite.DB, cfg *config.Config, args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromEnv := fs.Bool("from-env", false, "")
	file := fs.String("file", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config import [--from-env] [--file <path>]")}
	}
	if *fromEnv && *file != "" {
		return &cliExitError{code: exitUsage, err: errors.New("--from-env and --file are mutually exclusive")}
	}

	values := map[string]string{}
	switch {
	case *file != "":
		f, err := os.Open(*file)
		if err != nil {
			return fmt.Errorf("open %s: %w", *file, err)
		}
		defer f.Close()
		raw, err := config.ParseEnvFile(f)
		if err != nil {
			return &cliExitError{code: exitValidation, err: fmt.Errorf("parse %s: %w", *file, err)}
		}
		for _, key := range config.DBEligibleKeys() {
			if v, ok := raw[key]; ok && v != "" {
				values[key] = v
			}
		}
	case *fromEnv:
		for _, key := range config.DBEligibleKeys() {
			if v, ok := os.LookupEnv(key); ok && v != "" {
				values[key] = v
			}
		}
	default:
		redacted := cfg.Redacted()
		for _, key := range config.DBEligibleKeys() {
			values[key] = redacted[key]
		}
	}

	ownerID, err := ownerActorID(ctx, db)
	if err != nil {
		return err
	}

	var imported, skipped int
	var failed []string
	for _, key := range config.DBEligibleKeys() {
		value, ok := values[key]
		if !ok {
			continue
		}
		if _, err := db.Config.Get(ctx, key); err == nil {
			skipped++
			continue
		} else if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("read %s: %w", key, err)
		}
		if err := config.ValidateKeyValue(key, value); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		writeErr := db.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
			if err := repos.Config.Set(ctx, key, value, 0, ownerID, now); err != nil {
				return err
			}
			return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
				ID: domain.NewID(), Key: key, OldValue: nil, NewValue: &value,
				Version: 1, ChangedAt: now, ChangedBy: ownerID,
			})
		})
		if writeErr != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", key, writeErr))
			continue
		}
		imported++
	}

	fmt.Fprintf(stdout, "Imported %d key(s), skipped %d already-set key(s).\n", imported, skipped)
	if len(failed) > 0 {
		for _, f := range failed {
			fmt.Fprintf(stdout, "FAILED: %s\n", f)
		}
		return &cliExitError{code: exitValidation, err: fmt.Errorf("%d key(s) failed to import", len(failed))}
	}
	return nil
}

// --- history / rollback ------------------------------------------------

func configHistory(ctx context.Context, db *sqlite.DB, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config history [--json] <key>")}
	}
	key := fs.Arg(0)
	if !slices.Contains(config.KnownKeys(), key) {
		return unknownKeyError(key)
	}
	history, err := db.ConfigAudit.ListByKey(ctx, key)
	if err != nil {
		return fmt.Errorf("list history for %s: %w", key, err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(history)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tCHANGED_AT\tCHANGED_BY\tOLD_VALUE\tNEW_VALUE")
	for _, h := range history {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			h.Version, h.ChangedAt.UTC().Format(time.RFC3339), safeCell(h.ChangedBy),
			safeCell(formatOptionalString(h.OldValue)), safeCell(formatOptionalString(h.NewValue)))
	}
	return tw.Flush()
}

func formatOptionalString(v *string) string {
	if v == nil {
		return "-"
	}
	return *v
}

// configRollback re-applies a past audit entry's value as a new write
// (a new Set or Unset, recorded as its own fresh audit entry — a
// rollback is never a history edit). --to-version names
// AppConfigAuditEntry.Version, which can repeat across a delete/recreate
// cycle (see its own doc comment), so the most recently recorded match
// wins.
func configRollback(ctx context.Context, db *sqlite.DB, args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	toVersion := fs.Int("to-version", -1, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *toVersion < 0 {
		return &cliExitError{code: exitUsage, err: errors.New("usage: miauthctl config rollback --to-version <N> <key>")}
	}
	key := fs.Arg(0)
	if !slices.Contains(config.KnownKeys(), key) {
		return unknownKeyError(key)
	}

	history, err := db.ConfigAudit.ListByKey(ctx, key)
	if err != nil {
		return fmt.Errorf("list history for %s: %w", key, err)
	}
	var target *domain.AppConfigAuditEntry
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Version == *toVersion {
			target = &history[i]
			break
		}
	}
	if target == nil {
		return &cliExitError{code: exitNotFound, err: fmt.Errorf("%s has no history entry at version %d", key, *toVersion)}
	}

	ownerID, err := ownerActorID(ctx, db)
	if err != nil {
		return err
	}

	var oldValue *string
	expectedVersion := 0
	if existing, err := db.Config.Get(ctx, key); err == nil {
		v := existing.Value
		oldValue = &v
		expectedVersion = existing.Version
	} else if !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("read current value: %w", err)
	}

	noop := target.NewValue == nil && expectedVersion == 0
	writeErr := db.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if noop {
			return nil
		}
		if target.NewValue == nil {
			if err := repos.Config.Unset(ctx, key); err != nil {
				return err
			}
			return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
				ID: domain.NewID(), Key: key, OldValue: oldValue, NewValue: nil,
				Version: 0, ChangedAt: now, ChangedBy: ownerID,
			})
		}
		if err := repos.Config.Set(ctx, key, *target.NewValue, expectedVersion, ownerID, now); err != nil {
			return err
		}
		return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
			ID: domain.NewID(), Key: key, OldValue: oldValue, NewValue: target.NewValue,
			Version: expectedVersion + 1, ChangedAt: now, ChangedBy: ownerID,
		})
	})
	if writeErr != nil {
		if errors.Is(writeErr, domain.ErrConflict) {
			return &cliExitError{code: exitConflict, err: fmt.Errorf("%s was changed concurrently; try again", key)}
		}
		return fmt.Errorf("rollback %s: %w", key, writeErr)
	}
	if noop {
		fmt.Fprintf(stdout, "%s is already unset, matching version %d.\n", key, *toVersion)
		return nil
	}
	fmt.Fprintf(stdout, "Rolled back %s to the value from version %d.\n", key, *toVersion)
	return nil
}
