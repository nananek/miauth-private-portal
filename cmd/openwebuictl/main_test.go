package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/openwebui"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
)

// setOpenwebuictlTestEnv sets the minimal environment config.Load accepts
// (mirroring cmd/jobsctl's own setJobsctlTestEnv) plus the OpenWebUI keys
// OPENWEBUI_ENABLED requires — the same minimal set
// internal/config's own validOpenWebUIEnv test helper uses.
func setOpenwebuictlTestEnv(t *testing.T, dbPath string) {
	t.Helper()
	t.Setenv("APP_ENV", "development")
	t.Setenv("LOCAL_ORIGIN", "https://portal.example")
	t.Setenv("DB_PATH", dbPath)
	t.Setenv("OPENWEBUI_ENABLED", "true")
	t.Setenv("OPENWEBUI_BASE_URL", "https://openwebui.example.net")
	t.Setenv("OPENWEBUI_ALLOWED_ORIGINS", "https://openwebui.example.net")
	t.Setenv("OPENWEBUI_API_KEY", "sk-openwebui-secret")
	t.Setenv("OPENWEBUI_DEFAULT_MODEL_ID", "gpt-oss:20b")
	t.Setenv("OPENWEBUI_PRESENTATION_HOST", "openwebui.example.net")
}

// seedOpenwebuictlLink opens dbPath directly (bypassing run, which would
// re-open it itself) and inserts one seeded workspace/model plus one
// ambiguous conversation link with a pending turn recording body — the
// smallest fixture every subcommand test below can assert against
// without ever printing body itself, since the CLI's own output must
// never include it.
func seedOpenwebuictlLink(t *testing.T, dbPath, body string) (linkID string) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatal(err)
	}

	registry := openwebui.NewRegistry(db, db.Repos, openwebui.RegistryConfig{
		Enabled:          true,
		BaseURL:          "https://openwebui.example.net",
		SecretRef:        openwebui.SecretRefAPIKey,
		WorkspaceName:    "Open WebUI",
		PresentationHost: "openwebui.example.net",
		DefaultModelID:   "gpt-oss:20b",
		OwnerUsername:    "owner",
	}, nil, nil, nil)
	if err := registry.Seed(t.Context()); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	workspace, err := db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	owner := domain.Actor{ID: domain.NewID(), Type: domain.ActorOwner, CreatedAt: now}
	if err := db.Actors.Create(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	threadID := domain.NewID()
	if err := db.Threads.Create(t.Context(), domain.Thread{ID: threadID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	root := domain.Entry{
		ID: threadID, ThreadID: threadID, Kind: domain.EntryUserPost, AuthorActorID: owner.ID,
		Body: body, ProcessingStatus: domain.ProcessingNone, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Entries.Create(t.Context(), root); err != nil {
		t.Fatal(err)
	}

	link := domain.OpenWebUIConversationLink{
		ID: domain.NewID(), ThreadID: threadID, BranchID: domain.NewID(),
		WorkspaceID: workspace.ID, ModelID: *workspace.DefaultModelID,
		ClaimedAt: now, LastTransitionAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.OpenWebUILinks.Claim(t.Context(), link); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenWebUILinks.MarkAmbiguous(t.Context(), link.ID, now); err != nil {
		t.Fatal(err)
	}
	turn := domain.OpenWebUITurnLink{
		ID: domain.NewID(), LinkID: link.ID, BranchID: link.BranchID,
		LocalMessageID: root.ID, RequestID: domain.NewID(), Revision: 1, Attempt: 1, Status: domain.TurnAmbiguous,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.OpenWebUITurnLinks.Create(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	return link.ID
}

// seedOpenwebuictlModel seeds a workspace and its default model (the
// same Registry.Seed path seedOpenwebuictlLink uses) without the
// link/turn scaffolding that helper also builds — Issue #77 PR7's
// avatar-set/avatar-clear tests only need a model actor to exist.
// Returns the seeded model's actor slug, the argument these subcommands
// take.
func seedOpenwebuictlModel(t *testing.T, dbPath string) (slug string) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatal(err)
	}

	registry := openwebui.NewRegistry(db, db.Repos, openwebui.RegistryConfig{
		Enabled:          true,
		BaseURL:          "https://openwebui.example.net",
		SecretRef:        openwebui.SecretRefAPIKey,
		WorkspaceName:    "Open WebUI",
		PresentationHost: "openwebui.example.net",
		DefaultModelID:   "gpt-oss:20b",
		OwnerUsername:    "owner",
	}, nil, nil, nil)
	if err := registry.Seed(t.Context()); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	workspace, err := db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	model, err := db.OpenWebUIModels.GetByExternalID(t.Context(), workspace.ID, "gpt-oss:20b")
	if err != nil {
		t.Fatal(err)
	}
	return model.ActorSlug
}

// writeTestPNG writes a small valid PNG to path, for avatar-set tests
// that need a real raster image file on disk (internal/drive.
// ValidateImage rejects anything else).
func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write test PNG: %v", err)
	}
}

func TestRunAvatarSet_SetsAndClearsModelAvatar(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	t.Setenv("DRIVE_DATA_DIR", t.TempDir())
	slug := seedOpenwebuictlModel(t, dbPath)

	imagePath := filepath.Join(t.TempDir(), "avatar.png")
	writeTestPNG(t, imagePath)

	var out bytes.Buffer
	if err := run([]string{"avatar-set", slug, imagePath}, &out); err != nil {
		t.Fatalf("run(avatar-set): %v", err)
	}
	if !strings.Contains(out.String(), "set avatar for model "+slug) {
		t.Errorf("avatar-set output = %q", out.String())
	}

	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workspace, err := db.OpenWebUIWorkspaces.GetEnabled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	model, err := db.OpenWebUIModels.GetByActorSlug(t.Context(), workspace.ID, slug)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := db.Actors.Get(t.Context(), model.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	if actor.AvatarFileID == nil {
		t.Fatal("actor.AvatarFileID is nil after avatar-set, want it set")
	}
	file, err := db.Files.Get(t.Context(), *actor.AvatarFileID)
	if err != nil {
		t.Fatalf("Files.Get avatar file: %v", err)
	}
	if file.Purpose != domain.FilePurposeAvatar {
		t.Errorf("stored file purpose = %q, want %q", file.Purpose, domain.FilePurposeAvatar)
	}
	if file.OwnerActorID != nil {
		t.Errorf("stored file OwnerActorID = %v, want nil (a model has no Drive owner)", *file.OwnerActorID)
	}

	out.Reset()
	if err := run([]string{"avatar-clear", slug}, &out); err != nil {
		t.Fatalf("run(avatar-clear): %v", err)
	}
	if !strings.Contains(out.String(), "cleared avatar for model "+slug) {
		t.Errorf("avatar-clear output = %q", out.String())
	}
	actorAfterClear, err := db.Actors.Get(t.Context(), model.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	if actorAfterClear.AvatarFileID != nil {
		t.Errorf("actor.AvatarFileID = %v after avatar-clear, want nil", *actorAfterClear.AvatarFileID)
	}
}

func TestRunAvatarSet_UnknownSlugFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	t.Setenv("DRIVE_DATA_DIR", t.TempDir())
	seedOpenwebuictlModel(t, dbPath)

	imagePath := filepath.Join(t.TempDir(), "avatar.png")
	writeTestPNG(t, imagePath)

	err := run([]string{"avatar-set", "no-such-model", imagePath}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "resolve model") {
		t.Fatalf("run(avatar-set, unknown slug) error = %v, want a resolve-model complaint", err)
	}
}

func TestRunAvatarSet_UsageErrorOnMissingArguments(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	if err := run([]string{"avatar-set", "slug-only"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(avatar-set, missing image path) error = %v, want usage", err)
	}
	if err := run([]string{"avatar-clear"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(avatar-clear, missing slug) error = %v, want usage", err)
	}
}

func TestRunValidatesArguments(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(nil) error = %v, want usage", err)
	}
	if err := run([]string{"unknown"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("run(unknown) error = %v", err)
	}
}

func TestRunRejectsWhenOpenWebUIDisabled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	t.Setenv("APP_ENV", "development")
	t.Setenv("LOCAL_ORIGIN", "https://portal.example")
	t.Setenv("DB_PATH", dbPath)
	t.Setenv("OPENWEBUI_ENABLED", "false")

	err := run([]string{"links"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "OPENWEBUI_ENABLED") {
		t.Fatalf("run(links) error = %v, want an OPENWEBUI_ENABLED complaint", err)
	}
}

func TestRunLinks_ListsAndFiltersWithoutBody(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	linkID := seedOpenwebuictlLink(t, dbPath, "the owner's private post body")

	var out bytes.Buffer
	if err := run([]string{"links"}, &out); err != nil {
		t.Fatalf("run(links): %v", err)
	}
	if !strings.Contains(out.String(), linkID) {
		t.Errorf("links output = %q, want it to contain %q", out.String(), linkID)
	}
	if strings.Contains(out.String(), "the owner's private post body") {
		t.Errorf("links output leaked the post body: %q", out.String())
	}

	out.Reset()
	if err := run([]string{"links", "--state=ready"}, &out); err != nil {
		t.Fatalf("run(links --state=ready): %v", err)
	}
	if strings.Contains(out.String(), linkID) {
		t.Errorf("links --state=ready output = %q, want it to exclude the ambiguous link", out.String())
	}

	out.Reset()
	if err := run([]string{"links", "--state=bogus"}, &out); err == nil || !strings.Contains(err.Error(), "invalid --state") {
		t.Fatalf("run(links --state=bogus) error = %v, want an invalid --state complaint", err)
	}
}

func TestRunShow_PrintsLinkAndTurnsWithoutBody(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	linkID := seedOpenwebuictlLink(t, dbPath, "the owner's private post body")

	var out bytes.Buffer
	if err := run([]string{"show", linkID}, &out); err != nil {
		t.Fatalf("run(show): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, linkID) || !strings.Contains(got, "ambiguous") {
		t.Errorf("show output = %q, want the link id and its ambiguous state", got)
	}
	if strings.Contains(got, "the owner's private post body") {
		t.Errorf("show output leaked the post body: %q", got)
	}

	if err := run([]string{"show"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(show) with no id error = %v, want usage", err)
	}
}

func TestRunAbandon_MarksLinkDead(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	linkID := seedOpenwebuictlLink(t, dbPath, "body")

	var out bytes.Buffer
	if err := run([]string{"abandon", linkID}, &out); err != nil {
		t.Fatalf("run(abandon): %v", err)
	}
	if !strings.Contains(out.String(), "abandoned "+linkID) {
		t.Errorf("abandon output = %q", out.String())
	}

	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: dbPath, BusyTimeout: 5 * time.Second, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	link, err := db.OpenWebUILinks.Get(t.Context(), linkID)
	if err != nil {
		t.Fatal(err)
	}
	if link.State != domain.LinkDead {
		t.Errorf("link.State = %q, want dead", link.State)
	}
}

func TestRunAbandon_RejectsNonAmbiguousLink(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	linkID := seedOpenwebuictlLink(t, dbPath, "body")

	// Abandon it once (ambiguous -> dead); a second attempt must fail
	// rather than silently re-applying.
	if err := run([]string{"abandon", linkID}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first run(abandon): %v", err)
	}
	err := run([]string{"abandon", linkID}, &bytes.Buffer{})
	if !errors.Is(err, domain.ErrInvalidLinkTransition) {
		t.Fatalf("second run(abandon) error = %v, want domain.ErrInvalidLinkTransition", err)
	}
}

func TestRunConfirm_UsageErrorOnMissingArgument(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	// Argument validation happens after the owner actor is resolved (the
	// same order cmd/jobsctl's own subcommands use), so the database
	// needs an owner row before this usage check is ever reached.
	seedOpenwebuictlLink(t, dbPath, "body")
	if err := run([]string{"confirm"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(confirm) with no id error = %v, want usage", err)
	}
}

func TestRunFreeze_UsageErrorOnMissingArgument(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "owui.db")
	setOpenwebuictlTestEnv(t, dbPath)
	seedOpenwebuictlLink(t, dbPath, "body")
	if err := run([]string{"freeze"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("run(freeze) with no id error = %v, want usage", err)
	}
}
