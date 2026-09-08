package httpserver

import (
	"net/http"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// mustCreateExternalSourceActorForTest seeds an ActorExternalSource actor
// plus its owning rss-kind domain.ExternalSource (Issue #77 PR4/
// ADR-0008's design-A host display), mirroring what
// cmd/server's ensureRSSSourcesWithActors creates at startup.
func mustCreateExternalSourceActorForTest(t *testing.T, ts *noteAPITestServer, host, username string) domain.Actor {
	t.Helper()
	actor := domain.Actor{ID: domain.NewID(), Type: domain.ActorExternalSource, CreatedAt: ts.clock.Now()}
	if err := ts.db.Actors.Create(t.Context(), actor); err != nil {
		t.Fatalf("create external source actor: %v", err)
	}
	source := domain.ExternalSource{
		ID: domain.NewID(), Kind: "rss", URI: "https://" + host + "/feed.xml",
		ActorID: &actor.ID, Username: &username, Host: &host, CreatedAt: ts.clock.Now(),
	}
	if err := ts.db.ExternalSources.Create(t.Context(), source); err != nil {
		t.Fatalf("create external source: %v", err)
	}
	return actor
}

// TestResolveUserLite_ProjectsExternalSourceRealHost is Issue #77 PR4/
// ADR-0008's core contract test: an entry authored by an
// ActorExternalSource actor must project host as that source's own real
// origin and username as its registered (or derived) username — not the
// single synthetic OPENWEBUI_PRESENTATION_HOST value Issue #52's
// VirtualActor uses.
func TestResolveUserLite_ProjectsExternalSourceRealHost(t *testing.T) {
	ts := newNoteAPITestServer(t)
	actor := mustCreateExternalSourceActorForTest(t, ts, "note.example.com", "myfeed")

	provenanceURL := "https://note.example.com/articles/1"
	id := domain.NewID()
	entry := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryNews,
		AuthorActorID: actor.ID, Body: "[news] headline\n\nbody", ProcessingStatus: domain.ProcessingNone,
		ProvenanceURL: &provenanceURL, CreatedAt: ts.clock.Now(), UpdatedAt: ts.clock.Now(),
	}
	if err := ts.db.Threads.Create(t.Context(), domain.Thread{ID: entry.ThreadID, CreatedAt: ts.clock.Now(), UpdatedAt: ts.clock.Now()}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := ts.db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}

	rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": entry.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got map[string]any
	mustDecode(t, rec, &got)

	user, ok := got["user"].(map[string]any)
	if !ok {
		t.Fatalf("user is not an object: %v", got["user"])
	}
	if user["username"] != "myfeed" {
		t.Errorf("user.username = %v, want myfeed", user["username"])
	}
	if user["host"] != "note.example.com" {
		t.Errorf("user.host = %v, want note.example.com (the feed's own real origin)", user["host"])
	}
	if got["url"] != provenanceURL {
		t.Errorf("note.url = %v, want %q", got["url"], provenanceURL)
	}
}

// TestResolveUserLite_ProjectsExternalSourceAvatar is Issue #77 PR5's
// favicon-as-avatar contract test: once an ActorExternalSource actor's
// avatar_file_id is set (fetchAndSetSourceFavicon's job at startup — this
// test sets it directly, exercising only the wire projection), an entry
// it authored must carry that file's absolute /files/{id} URL as
// note.user.avatarUrl, exactly like the owner's own avatar
// (avatarURLFromFileID).
func TestResolveUserLite_ProjectsExternalSourceAvatar(t *testing.T) {
	ts := newNoteAPITestServer(t)
	actor := mustCreateExternalSourceActorForTest(t, ts, "note.example.com", "myfeed")

	writeToken, _ := mustIssueToken(t, ts.Server, "avatar-upload", "write:drive")
	fileID := uploadTestDriveFile(t, ts, writeToken)
	if err := ts.db.Actors.SetAvatarFileID(t.Context(), actor.ID, &fileID); err != nil {
		t.Fatalf("set avatar file id: %v", err)
	}

	id := domain.NewID()
	entry := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryNews,
		AuthorActorID: actor.ID, Body: "[news] headline\n\nbody", ProcessingStatus: domain.ProcessingNone,
		CreatedAt: ts.clock.Now(), UpdatedAt: ts.clock.Now(),
	}
	if err := ts.db.Threads.Create(t.Context(), domain.Thread{ID: entry.ThreadID, CreatedAt: ts.clock.Now(), UpdatedAt: ts.clock.Now()}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := ts.db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}

	rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": entry.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got map[string]any
	mustDecode(t, rec, &got)
	user, ok := got["user"].(map[string]any)
	if !ok {
		t.Fatalf("user is not an object: %v", got["user"])
	}
	want := ts.localOrigin + "/files/" + fileID
	if user["avatarUrl"] != want {
		t.Errorf("user.avatarUrl = %v, want %q", user["avatarUrl"], want)
	}
}

// TestResolveUserLite_ExternalSourceFallsBackWhenNotResolvable mirrors
// TestResolveUserLite_VirtualActorFallsBackWhenNotResolvable: an
// ActorExternalSource actor whose owning ExternalSource has since been
// deleted (or never existed) must fall back to resolveUserLite's plain
// projection (username = the actor's own opaque ID, host = null) rather
// than erroring the whole note out.
func TestResolveUserLite_ExternalSourceFallsBackWhenNotResolvable(t *testing.T) {
	ts := newNoteAPITestServer(t)
	actor := domain.Actor{ID: domain.NewID(), Type: domain.ActorExternalSource, CreatedAt: ts.clock.Now()}
	if err := ts.db.Actors.Create(t.Context(), actor); err != nil {
		t.Fatalf("create external source actor: %v", err)
	}
	// Deliberately no matching domain.ExternalSource row.

	id := domain.NewID()
	entry := domain.Entry{
		ID: id, ThreadID: id, Kind: domain.EntryNews,
		AuthorActorID: actor.ID, Body: "orphaned actor", ProcessingStatus: domain.ProcessingNone,
		CreatedAt: ts.clock.Now(), UpdatedAt: ts.clock.Now(),
	}
	if err := ts.db.Threads.Create(t.Context(), domain.Thread{ID: entry.ThreadID, CreatedAt: ts.clock.Now(), UpdatedAt: ts.clock.Now()}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := ts.db.Entries.Create(t.Context(), entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}

	rec := ts.post(t, "/api/notes/show", map[string]any{"noteId": entry.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var got map[string]any
	mustDecode(t, rec, &got)
	user, ok := got["user"].(map[string]any)
	if !ok {
		t.Fatalf("user is not an object: %v", got["user"])
	}
	if user["username"] != actor.ID {
		t.Errorf("user.username = %v, want the actor's own opaque ID %q (plain fallback)", user["username"], actor.ID)
	}
	if user["host"] != nil {
		t.Errorf("user.host = %v, want nil (plain fallback)", user["host"])
	}
}
