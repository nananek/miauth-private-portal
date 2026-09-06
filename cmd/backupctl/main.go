// Command backupctl takes and verifies online-safe SQLite backups for an
// operator with host access, matching miauthctl/jobsctl's boundary: it
// runs against the same host and DB_PATH as the server, requires no
// HTTP surface, and needs no external sqlite3 binary (this service's
// distroless runtime image ships neither a shell nor sqlite3). Backups
// use SQLite's own VACUUM INTO, safe to run against a live database in
// WAL mode without stopping the server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "backup":
		return runBackup(args[1:], stdout)
	case "verify":
		return runVerify(args[1:], stdout)
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: backupctl <backup|verify> [arguments]")
}

// runBackup opens this deployment's configured database (DB_PATH,
// resolved the same way as miauthctl/jobsctl) and snapshots it to
// destPath.
func runBackup(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: backupctl backup <dest-path>")
	}
	destPath := args[0]

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
	if err := db.Backup(ctx, destPath); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	fmt.Fprintf(stdout, "Backed up %s to %s\n", safeCell(cfg.DB.Path), safeCell(destPath))
	return nil
}

// runVerify inspects a database file (typically a backup produced by
// runBackup) without ever writing to it: schema_migrations checksum
// verification plus a core-table row count summary always run;
// --deep additionally runs a full PRAGMA integrity_check scan.
func runVerify(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	deep := fs.Bool("deep", false, "also run a full PRAGMA integrity_check scan (expensive: a full-database scan)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || fs.Arg(0) == "" {
		return errors.New("usage: backupctl verify [--deep] <path>")
	}
	path := fs.Arg(0)

	ctx := context.Background()
	db, err := sqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()

	if err := db.VerifyMigrations(ctx); err != nil {
		return fmt.Errorf("verify migrations: %w", err)
	}
	fmt.Fprintln(stdout, "Schema migrations: OK")

	if *deep {
		if err := db.IntegrityCheck(ctx); err != nil {
			return fmt.Errorf("integrity check: %w", err)
		}
		fmt.Fprintln(stdout, "Integrity check: OK")
	}

	counts, err := db.TableCounts(ctx)
	if err != nil {
		return fmt.Errorf("count tables: %w", err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tROWS")
	for _, table := range []string{"actors", "entries", "jobs", "external_sources"} {
		fmt.Fprintf(tw, "%s\t%d\n", table, counts[table])
	}
	return tw.Flush()
}

// safeCell prevents an untrusted path from injecting terminal controls
// into operator-facing output, matching miauthctl/jobsctl's safeCell.
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
