package integration

import (
	"net/http"
	"testing"
	"time"
)

// TestServerE2E_OpenWebUIDisabledLeavesOrdinaryNoteProjectionUnchanged is
// Issue #52's flag-off regression evidence over the real binary: with
// OPENWEBUI_ENABLED left unset (its safe default, the same baseline
// every other integration test in this package already runs with), an
// ordinary owner-authored note's wire projection must be exactly what
// it was before this feature's cmd/server wiring (the VirtualActors
// field, the conditional Registry construction and Seed call) existed —
// a null host and the configured owner username, not a leftover or
// partially-applied Open WebUI projection.
//
// The DB-level half of this same regression (fresh migrations leave
// every openwebui_* table empty; a disabled Registry.Seed writes
// nothing) is covered where it belongs by unit tests closer to the
// code — internal/storage/sqlite's TestMigrate_FreshDatabase and
// internal/openwebui's TestSeed_DisabledReturnsErrDisabledAndWritesNothing
// — rather than by opening the subprocess's database file here, which
// no other test in this black-box, HTTP/OS-signal-boundary package does
// (see doc.go).
func TestServerE2E_OpenWebUIDisabledLeavesOrdinaryNoteProjectionUnchanged(t *testing.T) {
	serverBin := buildBinary(t, "./cmd/server", "server")
	miauthctlBin := buildBinary(t, "./cmd/miauthctl", "miauthctl")

	ts := startServer(t, serverBin, nil) // no OPENWEBUI_* env at all
	ts.waitForReady(t, 10*time.Second)
	defer ts.terminateAndWait(t, 5*time.Second)

	token := approveMiAuthSession(t, miauthctlBin, ts, "read:account,read:notes,write:notes")

	var created struct {
		CreatedNote struct {
			ID   string `json:"id"`
			User struct {
				ID       string  `json:"id"`
				Username string  `json:"username"`
				Host     *string `json:"host"`
			} `json:"user"`
		} `json:"createdNote"`
	}
	resp := postJSON(t, ts.baseURL+"/api/notes/create", map[string]any{
		"text": "ordinary owner post with the feature off",
		"i":    token,
	}, &created)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("notes/create status = %d, want 200", resp.StatusCode)
	}
	if created.CreatedNote.User.Host != nil {
		t.Errorf("owner note user.host = %v, want nil", created.CreatedNote.User.Host)
	}
	if created.CreatedNote.User.Username != "owner" {
		t.Errorf("owner note user.username = %q, want the default OWNER_USERNAME %q", created.CreatedNote.User.Username, "owner")
	}

	var shown struct {
		User struct {
			ID       string  `json:"id"`
			Username string  `json:"username"`
			Host     *string `json:"host"`
		} `json:"user"`
	}
	showResp := postJSON(t, ts.baseURL+"/api/notes/show", map[string]any{
		"noteId": created.CreatedNote.ID,
		"i":      token,
	}, &shown)
	if showResp.StatusCode != http.StatusOK {
		t.Fatalf("notes/show status = %d, want 200", showResp.StatusCode)
	}
	if shown.User.Host != nil || shown.User.Username != "owner" || shown.User.ID != created.CreatedNote.User.ID {
		t.Errorf("notes/show user projection = %+v, want the same unchanged owner projection notes/create returned", shown.User)
	}
}
