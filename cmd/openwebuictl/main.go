// Command openwebuictl provides host-local owner recovery for Issue
// #53's Open WebUI outbound turn bridge (plan §5.5): listing and
// inspecting conversation links, and resolving an ambiguous or stuck one
// by confirming, abandoning, or freezing it. It also provides Issue
// #77 PR7's VirtualActor avatar management (avatar-set/avatar-clear,
// plan-77 v2 §2.5) — a model is never a Drive API caller, so it has no
// other write path to its own avatar_file_id. It deliberately exposes no
// HTTP surface — the roadmap's "Manual resolution ... is an explicit
// owner/operator action" — matching jobsctl's and miauthctl's own
// operator boundary: permission to run this binary against this
// deployment's database is the permission it grants.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/drive"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
	owuiprovider "github.com/nananek/miauth-private-portal/internal/provider/openwebui"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

const (
	defaultListLimit = 50
	maxListLimit     = 1000
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: openwebuictl <links|show|confirm|abandon|freeze|avatar-set|avatar-clear> [arguments]")
	}
	sub := args[0]
	switch sub {
	case "links", "show", "confirm", "abandon", "freeze", "avatar-set", "avatar-clear":
	default:
		return fmt.Errorf("unknown subcommand %q; want links, show, confirm, abandon, freeze, avatar-set, or avatar-clear", sub)
	}

	cfg, err := config.Load(config.LoadOptions{ConfigFilePath: configFilePath()})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.OpenWebUI.Enabled {
		return errors.New("OPENWEBUI_ENABLED is false; openwebuictl has nothing to operate on")
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

	// avatar-set/avatar-clear (Issue #77 PR7, plan-77 v2 §2.5: "モデル
	// ごとに専用の管理コマンドをopenwebuictlに追加する") operate on a
	// model actor directly and need none of the conversation-link
	// recovery machinery (Registry/provider/timelineSvc) below.
	switch sub {
	case "avatar-set":
		return runAvatarSet(ctx, db, cfg, args[1:], stdout)
	case "avatar-clear":
		return runAvatarClear(ctx, db, cfg, args[1:], stdout)
	}

	owner, err := db.Actors.GetByType(ctx, domain.ActorOwner)
	if err != nil {
		return fmt.Errorf("resolve owner actor: %w", err)
	}

	// Only confirm ever talks to the provider (plan §5.5: "confirm だけ
	// provider に接続する"); every other subcommand passes a nil
	// Provider into Registry, which recovery.go's other four methods
	// never touch.
	var provider openwebui.Provider
	if sub == "confirm" {
		client, err := owuiprovider.NewClient(owuiprovider.ConfigFrom(cfg.OpenWebUI))
		if err != nil {
			return fmt.Errorf("build openwebui provider client: %w", err)
		}
		provider = client
	}

	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{OwnerUsername: cfg.Auth.OwnerUsername})
	registry := openwebui.NewRegistry(db, db.Repos, openwebui.RegistryConfig{
		Enabled:           cfg.OpenWebUI.Enabled,
		BaseURL:           cfg.OpenWebUI.BaseURL,
		SecretRef:         config.KeyOpenWebUIAPIKey,
		WorkspaceName:     cfg.OpenWebUI.WorkspaceName,
		PresentationHost:  cfg.OpenWebUI.PresentationHost,
		DefaultModelID:    cfg.OpenWebUI.DefaultModelID,
		OwnerUsername:     cfg.Auth.OwnerUsername,
		GenerationEnabled: cfg.OpenWebUI.GenerationEnabled,
	}, nil, timelineSvc, provider)

	switch sub {
	case "links":
		return runLinks(ctx, registry, owner.ID, args[1:], stdout)
	case "show":
		return runShow(ctx, registry, owner.ID, args[1:], stdout)
	case "confirm":
		return runConfirm(ctx, registry, owner.ID, args[1:], stdout)
	case "abandon":
		return runAbandon(ctx, registry, owner.ID, args[1:], stdout)
	case "freeze":
		return runFreeze(ctx, registry, owner.ID, args[1:], stdout)
	default:
		panic("unreachable")
	}
}

func runLinks(ctx context.Context, reg *openwebui.Registry, ownerID string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("links", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateText := fs.String("state", "", "link state")
	threadID := fs.String("thread", "", "thread id")
	limit := fs.Int("limit", defaultListLimit, "maximum rows")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse links flags: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("usage: openwebuictl links [--state=<state>] [--thread=<thread-id>] [--limit=N]")
	}
	if *limit < 1 || *limit > maxListLimit {
		return fmt.Errorf("--limit must be between 1 and %d", maxListLimit)
	}

	filter := domain.OpenWebUILinkFilter{Limit: *limit}
	if *stateText != "" {
		state := domain.LinkState(*stateText)
		if !validLinkState(state) {
			return fmt.Errorf("invalid --state %q; want creation_pending, ready, ambiguous, failed, or dead", *stateText)
		}
		filter.State = &state
	}
	if *threadID != "" {
		filter.ThreadID = threadID
	}

	links, err := reg.ListLinks(ctx, ownerID, filter)
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTHREAD_ID\tSTATE\tFAILURE_CATEGORY\tCLAIMED_AT\tLAST_TRANSITION_AT")
	for _, link := range links {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			safeCell(link.ID), safeCell(link.ThreadID), link.State, safeCell(orDash(link.FailureCategory)),
			link.ClaimedAt.UTC().Format(time.RFC3339Nano), link.LastTransitionAt.UTC().Format(time.RFC3339Nano),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write links output: %w", err)
	}
	return nil
}

func runShow(ctx context.Context, reg *openwebui.Registry, ownerID string, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: openwebuictl show <link-id>")
	}
	link, turns, err := reg.DescribeLink(ctx, ownerID, args[0])
	if err != nil {
		return fmt.Errorf("show link %s: %w", args[0], err)
	}

	fmt.Fprintf(stdout, "id:                 %s\n", safeCell(link.ID))
	fmt.Fprintf(stdout, "thread_id:          %s\n", safeCell(link.ThreadID))
	fmt.Fprintf(stdout, "branch_id:          %s\n", safeCell(link.BranchID))
	fmt.Fprintf(stdout, "state:              %s\n", link.State)
	fmt.Fprintf(stdout, "failure_category:   %s\n", safeCell(orDash(link.FailureCategory)))
	fmt.Fprintf(stdout, "has_remote_chat_id: %t\n", link.RemoteChatID != nil)
	fmt.Fprintf(stdout, "has_remote_current: %t\n", link.RemoteCurrentID != nil)
	fmt.Fprintf(stdout, "claimed_at:         %s\n", link.ClaimedAt.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(stdout, "last_transition_at: %s\n", link.LastTransitionAt.UTC().Format(time.RFC3339Nano))
	fmt.Fprintln(stdout)

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TURN_ID\tSTATUS\tATTEMPT\tFAILURE_CATEGORY\tHAS_ASSISTANT_ENTRY\tHAS_REMOTE_CHAT_ID\tCREATED_AT")
	for _, t := range turns {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%t\t%t\t%s\n",
			safeCell(t.ID), t.Status, t.Attempt, safeCell(orDash(t.FailureCategory)),
			t.AssistantEntryID != nil, t.RemoteChatID != nil, t.CreatedAt.UTC().Format(time.RFC3339Nano),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write turns output: %w", err)
	}
	return nil
}

func runConfirm(ctx context.Context, reg *openwebui.Registry, ownerID string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("confirm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	remoteChatID := fs.String("remote-chat-id", "", "remote chat id, if the link does not already have one recorded")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse confirm flags: %w", err)
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return errors.New("usage: openwebuictl confirm <link-id> [--remote-chat-id=<id>]")
	}
	var chatID *string
	if *remoteChatID != "" {
		chatID = remoteChatID
	}

	entry, err := reg.ConfirmLink(ctx, ownerID, fs.Arg(0), chatID)
	if err != nil {
		return fmt.Errorf("confirm link %s: %w", fs.Arg(0), err)
	}
	if entry.ID != "" {
		fmt.Fprintf(stdout, "confirmed %s: recovered reply %s\n", safeCell(fs.Arg(0)), safeCell(entry.ID))
	} else {
		fmt.Fprintf(stdout, "confirmed %s: no reply recovered (the turn's outcome was not a success)\n", safeCell(fs.Arg(0)))
	}
	return nil
}

func runAbandon(ctx context.Context, reg *openwebui.Registry, ownerID string, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: openwebuictl abandon <link-id>")
	}
	if err := reg.AbandonLink(ctx, ownerID, args[0]); err != nil {
		return fmt.Errorf("abandon link %s: %w", args[0], err)
	}
	fmt.Fprintf(stdout, "abandoned %s\n", safeCell(args[0]))
	return nil
}

func runFreeze(ctx context.Context, reg *openwebui.Registry, ownerID string, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: openwebuictl freeze <link-id>")
	}
	if err := reg.FreezeLink(ctx, ownerID, args[0]); err != nil {
		return fmt.Errorf("freeze link %s: %w", args[0], err)
	}
	fmt.Fprintf(stdout, "froze %s\n", safeCell(args[0]))
	return nil
}

// resolveModelBySlug finds the model actor behind slug in this
// deployment's single enabled Open WebUI workspace (there is at most
// one, per Registry.Seed's own enabled-workspace invariant) — the same
// handle Aria's own @mention UI already shows the owner for this model,
// so it is the natural argument for an operator to type rather than the
// model's opaque local ID (which no existing openwebuictl subcommand
// even lists).
func resolveModelBySlug(ctx context.Context, db *sqlite.DB, slug string) (domain.OpenWebUIModel, error) {
	workspace, err := db.OpenWebUIWorkspaces.GetEnabled(ctx)
	if err != nil {
		return domain.OpenWebUIModel{}, fmt.Errorf("resolve enabled workspace: %w", err)
	}
	model, err := db.OpenWebUIModels.GetByActorSlug(ctx, workspace.ID, slug)
	if err != nil {
		return domain.OpenWebUIModel{}, fmt.Errorf("resolve model %q: %w", slug, err)
	}
	return model, nil
}

// buildDriveService wires the same internal/drive.Service shape
// cmd/server builds, selected by DRIVE_BACKEND. avatar-set is the only
// caller; its httpClient is required by NewService's signature but never
// exercised (CreateSystemFile never fetches a URL), so it is built with
// the same fixed, conservative policy cmd/server's own upload-from-url
// client uses rather than a caller-configurable one.
func buildDriveService(cfg *config.Config, db *sqlite.DB) (*drive.Service, error) {
	var storage drive.Storage
	switch cfg.Drive.Backend {
	case "s3compat":
		s3, err := drive.NewS3(drive.S3Config{
			Endpoint:        cfg.Drive.S3Endpoint,
			Bucket:          cfg.Drive.S3Bucket,
			AccessKeyID:     cfg.Drive.S3AccessKeyID,
			SecretAccessKey: cfg.Drive.S3SecretAccessKey,
			UseSSL:          cfg.Drive.S3UseSSL,
			Region:          cfg.Drive.S3Region,
		})
		if err != nil {
			return nil, fmt.Errorf("build drive S3 client: %w", err)
		}
		storage = s3
	default:
		storage = drive.NewLocal(cfg.Drive.DataDir)
	}
	return drive.NewService(
		storage,
		safehttp.NewClient(safehttp.Config{MaxRedirects: 3, AllowInsecureHTTP: false}),
		db.Repos,
		drive.Config{
			MaxFileBytes:   cfg.Drive.MaxFileBytes,
			MaxImageWidth:  cfg.Drive.MaxImageWidth,
			MaxImageHeight: cfg.Drive.MaxImageHeight,
		},
	), nil
}

// runAvatarSet reads a local raster image file, validates and stores it
// through internal/drive.Service exactly like any other Drive upload
// (Issue #77 v2's raster-only scope decision applies here too — an SVG
// or oversized image is rejected the same way), and points slug's model
// actor at it — Issue #77 AC "ユーザーとVirtualActorがプロフィール画像を
// 設定・変更・削除できる"'s VirtualActor half, which PR5 deliberately left
// unimplemented (a model is never a Drive API caller, so it needed its
// own write path rather than reusing POST /api/i/update).
func runAvatarSet(ctx context.Context, db *sqlite.DB, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) != 2 || args[0] == "" || args[1] == "" {
		return errors.New("usage: openwebuictl avatar-set <model-slug> <image-path>")
	}
	slug, imagePath := args[0], args[1]

	model, err := resolveModelBySlug(ctx, db, slug)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return fmt.Errorf("read image file %s: %w", imagePath, err)
	}
	driveSvc, err := buildDriveService(cfg, db)
	if err != nil {
		return err
	}
	// FilePurposeAvatar (domain.FilePurpose's own closed enum) is used
	// here for the first time in this codebase: the owner's own avatar
	// (POST /api/i/update) points at an ordinary FilePurposeAttachment
	// file already uploaded through the Drive API (matching Aria's real
	// upload-then-set-avatarId sequence, PR0's trace), but a model has no
	// Drive of its own to upload into — FilePurposeAvatar is the purpose
	// this exact case exists for.
	file, err := driveSvc.CreateSystemFile(ctx, domain.FilePurposeAvatar, filepath.Base(imagePath), data)
	if err != nil {
		return fmt.Errorf("store avatar image: %w", err)
	}
	if err := db.Actors.SetAvatarFileID(ctx, model.ActorID, &file.ID); err != nil {
		return fmt.Errorf("set model avatar: %w", err)
	}
	fmt.Fprintf(stdout, "set avatar for model %s (actor %s): file %s\n", safeCell(slug), safeCell(model.ActorID), safeCell(file.ID))
	return nil
}

// runAvatarClear clears slug's model actor's avatar — the explicit-null
// counterpart to avatar-set, matching POST /api/i/update's own
// avatarId: null "remove avatar" convention (PR0's trace).
func runAvatarClear(ctx context.Context, db *sqlite.DB, _ *config.Config, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: openwebuictl avatar-clear <model-slug>")
	}
	slug := args[0]

	model, err := resolveModelBySlug(ctx, db, slug)
	if err != nil {
		return err
	}
	if err := db.Actors.SetAvatarFileID(ctx, model.ActorID, nil); err != nil {
		return fmt.Errorf("clear model avatar: %w", err)
	}
	fmt.Fprintf(stdout, "cleared avatar for model %s (actor %s)\n", safeCell(slug), safeCell(model.ActorID))
	return nil
}

func validLinkState(state domain.LinkState) bool {
	switch state {
	case domain.LinkCreationPending, domain.LinkReady, domain.LinkAmbiguous, domain.LinkFailed, domain.LinkDead, domain.LinkStateless:
		return true
	default:
		return false
	}
}

func orDash(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}

// safeCell strips non-printable characters the same way jobsctl's own
// safeCell does, so nothing from a value this deployment does not fully
// control (a local id is safe today, but this is the one place list/show
// output is rendered) can smuggle control characters into a terminal.
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
