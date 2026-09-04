// Command jobsctl provides host-local inspection and manual recovery for
// durable jobs. It deliberately exposes no HTTP surface: permission to operate
// on the queue is the permission to access this deployment's database and run
// this binary, matching bootstrapctl's operator boundary.
package main

import (
	"context"
	"encoding/json"
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
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

const (
	defaultListLimit = 50
	maxListLimit     = 1000
	listErrorRunes   = 80
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: jobsctl <list|show|retry> [arguments]")
	}
	if args[0] != "list" && args[0] != "show" && args[0] != "retry" {
		return fmt.Errorf("unknown subcommand %q; want list, show, or retry", args[0])
	}

	cfg, err := config.Load(config.LoadOptions{ConfigFilePath: configFilePath()})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{
		Path:         cfg.DB.Path,
		BusyTimeout:  cfg.DB.BusyTimeout,
		MaxOpenConns: cfg.DB.MaxOpenConns,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	switch args[0] {
	case "list":
		return runList(ctx, db.Jobs, args[1:], stdout)
	case "show":
		return runShow(ctx, db.Jobs, args[1:], stdout)
	case "retry":
		return runRetry(ctx, db.Jobs, args[1:], stdout)
	default:
		panic("unreachable")
	}
}

func runList(ctx context.Context, repo domain.JobRepository, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateText := fs.String("state", "", "job state")
	jobType := fs.String("type", "", "job type")
	limit := fs.Int("limit", defaultListLimit, "maximum rows")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse list flags: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("usage: jobsctl list [--state=<state>] [--type=<job_type>] [--limit=N]")
	}
	if *limit < 1 || *limit > maxListLimit {
		return fmt.Errorf("--limit must be between 1 and %d", maxListLimit)
	}

	filter := domain.JobFilter{Limit: *limit}
	if *stateText != "" {
		state := domain.JobState(*stateText)
		if !validState(state) {
			return fmt.Errorf("invalid --state %q; want pending, running, succeeded, failed, or dead", *stateText)
		}
		filter.State = &state
	}
	if *jobType != "" {
		filter.JobType = jobType
	}

	jobs, err := repo.List(ctx, filter)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTYPE\tSTATE\tATTEMPT\tNEXT_RUN_AT\tLAST_ERROR")
	for _, job := range jobs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			safeCell(job.ID), safeCell(job.JobType), job.State, job.Attempt,
			job.NextRunAt.UTC().Format(time.RFC3339Nano), abbreviatedError(job.LastError),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write list output: %w", err)
	}
	return nil
}

func runShow(ctx context.Context, repo domain.JobRepository, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: jobsctl show <job-id>")
	}
	job, err := repo.Get(ctx, args[0])
	if err != nil {
		return fmt.Errorf("show job %s: %w", args[0], err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(job); err != nil {
		return fmt.Errorf("write job: %w", err)
	}
	return nil
}

func runRetry(ctx context.Context, repo domain.JobRepository, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: jobsctl retry <job-id>")
	}
	if err := repo.Requeue(ctx, args[0], time.Now().UTC()); err != nil {
		return fmt.Errorf("retry job %s: %w", args[0], err)
	}
	fmt.Fprintf(stdout, "requeued %s\n", safeCell(args[0]))
	return nil
}

func validState(state domain.JobState) bool {
	switch state {
	case domain.JobPending, domain.JobRunning, domain.JobSucceeded, domain.JobFailed, domain.JobDead:
		return true
	default:
		return false
	}
}

func abbreviatedError(value *string) string {
	if value == nil {
		return "-"
	}
	runes := []rune(*value)
	if len(runes) > listErrorRunes {
		runes = append(runes[:listErrorRunes-1], '…')
	}
	return safeCell(string(runes))
}

func safeCell(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, value)
}

func configFilePath() string {
	if v, ok := os.LookupEnv("CONFIG_FILE"); ok && v != "" {
		return v
	}
	return ".env"
}
