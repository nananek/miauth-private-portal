// Command fakemisskey is a test-only stand-in for the upstream Misskey
// instance's MiAuth endpoints. It exists so contract tests (see
// scripts/run-contract-tests.sh and contract/aria_client) can drive a
// real MiAuth HTTP round trip through internal/provider/misskey.Client
// without depending on an actual Misskey instance.
//
// It unconditionally approves every session with a fixed user ID, so it
// must never be reachable from a production deployment: neither the
// Makefile's build target nor the Dockerfile builds or copies this
// package.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/url"
)

func main() {
	addr := flag.String("addr", ":18081", "listen address")
	fixedUserID := flag.String("fixed-user-id", "contract-test-owner", "upstream user id every check() call reports")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /miauth/{id}", handleMiAuthStart)
	mux.HandleFunc("POST /api/miauth/{id}/check", handleCheck(*fixedUserID))

	log.Printf("fakemisskey listening on %s (fixed-user-id=%s)", *addr, *fixedUserID)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// handleMiAuthStart handles GET /miauth/{id}, standing in for a real
// Misskey instance's MiAuth consent page. It never renders a consent
// screen: it immediately redirects to the caller-supplied callback,
// appending its own session parameter the way a real Misskey instance
// does (internal/httpserver/miauth_handlers.go's upstreamMiAuthURL doc
// comment: Misskey treats the callback URL as opaque and appends its own
// session= parameter when redirecting back, without stripping the
// parameters already present). Reproducing a human consent step would
// not exercise anything this package's callers need; only the HTTP round
// trip through internal/provider/misskey.Client matters here.
func handleMiAuthStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	callback := r.URL.Query().Get("callback")
	if callback == "" {
		http.Error(w, "missing callback parameter", http.StatusBadRequest)
		return
	}

	u, err := url.Parse(callback)
	if err != nil {
		http.Error(w, "invalid callback parameter", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("session", id)
	u.RawQuery = q.Encode()

	http.Redirect(w, r, u.String(), http.StatusFound)
}

// checkResponse is the shape internal/provider/misskey.Client.Check
// decodes (see its own checkResponse type): only ok/token/user.id are
// ever read from it, so that is the minimum this stub returns.
type checkResponse struct {
	OK    bool   `json:"ok"`
	Token string `json:"token"`
	User  struct {
		ID string `json:"id"`
	} `json:"user"`
}

// handleCheck handles POST /api/miauth/{id}/check, standing in for a
// real Misskey instance's MiAuth check endpoint. It unconditionally
// approves every session with the fixed user ID: there is no session
// state to track because this stub never renders a consent step for a
// caller to actually approve or deny in the first place.
func handleCheck(fixedUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := checkResponse{OK: true, Token: "fake-upstream-token"}
		resp.User.ID = fixedUserID

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
