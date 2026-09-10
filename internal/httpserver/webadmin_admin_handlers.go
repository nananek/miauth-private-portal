package httpserver

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/webadmin"
)

// handleAdminIndex serves GET /admin/ (behind RequireAdminSession only —
// a read, no CSRF token needed to view it): Issue #136 Phase 3's real
// dashboard, replacing Phase 2's placeholder. It lists every pending
// MiAuth session and API token, each with its own approve/reject/revoke/
// reflect-scopes action, plus the CSRF token Phase 2 already embeds.
// s.miauth.ListPendingSessions/ListAPITokens are read fresh on every
// request — no caching, this is a low-traffic admin surface.
func (s *Server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.miauth.ListPendingSessions(r.Context())
	if err != nil {
		s.logger.Error("list pending sessions for admin dashboard failed",
			"request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeWebAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	tokens, err := s.miauth.ListAPITokens(r.Context())
	if err != nil {
		s.logger.Error("list API tokens for admin dashboard failed",
			"request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeWebAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	rssSnapshot, err := s.webadmin.ListRSSFeeds(r.Context())
	if err != nil {
		s.logger.Error("list RSS feeds for admin dashboard failed",
			"request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeWebAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	session := AdminSessionFromContext(r.Context())
	csrfToken := ""
	if session.CSRFToken != nil {
		csrfToken = *session.CSRFToken
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// This response embeds the caller's own CSRF token (unchanged from
	// Phase 2) plus now-current session/token detail — no-store keeps
	// both out of disk/shared caches.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = adminDashboardTemplate.Execute(w, adminDashboardData{
		CSRFToken: csrfToken, Sessions: sessions, Tokens: tokens,
		RSSFeeds: rssSnapshot.Feeds, RSSFeedsFromOverride: rssSnapshot.FromOverride,
		RSSEnabled: s.webadmin.RSSEnabled(), RSSFilterScriptPathConfigured: s.webadmin.RSSFilterScriptPathConfigured(),
	})
}

type adminDashboardData struct {
	CSRFToken string
	Sessions  []domain.LocalMiAuthSession
	Tokens    []domain.APIToken
	// Issue #136 Phase 4:
	RSSFeeds                      []webadmin.RSSFeed
	RSSFeedsFromOverride          bool
	RSSEnabled                    bool
	RSSFilterScriptPathConfigured bool
}

// handleAdminSessionsApprove serves POST /admin/sessions/approve (behind
// RequireAdminSession + RequireAdminCSRF). Body: {"routeSessionId": "..."}.
func (s *Server) handleAdminSessionsApprove(w http.ResponseWriter, r *http.Request) {
	routeSessionID, ok := decodeAdminActionTarget(w, r, "routeSessionId")
	if !ok {
		return
	}
	if err := s.miauth.ApproveSession(r.Context(), routeSessionID); err != nil {
		s.writeAdminActionMiAuthError(w, r, "approve session", err)
		return
	}
	s.recordAdminAction(r, domain.WebAdminActionApproveSession, routeSessionID,
		strPtr(string(domain.MiAuthCreated)), strPtr(string(domain.MiAuthAuthorized)))
	writeWebAdminOK(w)
}

// handleAdminSessionsReject serves POST /admin/sessions/reject. Same
// shape as Approve above.
func (s *Server) handleAdminSessionsReject(w http.ResponseWriter, r *http.Request) {
	routeSessionID, ok := decodeAdminActionTarget(w, r, "routeSessionId")
	if !ok {
		return
	}
	if err := s.miauth.RejectSession(r.Context(), routeSessionID); err != nil {
		s.writeAdminActionMiAuthError(w, r, "reject session", err)
		return
	}
	s.recordAdminAction(r, domain.WebAdminActionRejectSession, routeSessionID,
		strPtr(string(domain.MiAuthCreated)), strPtr(string(domain.MiAuthDenied)))
	writeWebAdminOK(w)
}

// handleAdminTokensRevoke serves POST /admin/tokens/revoke. Body:
// {"tokenId": "..."}.
func (s *Server) handleAdminTokensRevoke(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := decodeAdminActionTarget(w, r, "tokenId")
	if !ok {
		return
	}
	if err := s.miauth.RevokeAPIToken(r.Context(), tokenID); err != nil {
		s.writeAdminActionMiAuthError(w, r, "revoke token", err)
		return
	}
	s.recordAdminAction(r, domain.WebAdminActionRevokeToken, tokenID, nil, strPtr("revoked"))
	writeWebAdminOK(w)
}

// handleAdminTokensReflectScopes serves POST /admin/tokens/reflect-scopes.
// Body: {"tokenId": "..."}. Mirrors miauthctl tokens reflect-scopes' own
// single-token path (cmd/miauthctl/tokens.go) — same service call, same
// additive-only semantics, just a second caller.
func (s *Server) handleAdminTokensReflectScopes(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := decodeAdminActionTarget(w, r, "tokenId")
	if !ok {
		return
	}
	session := AdminSessionFromContext(r.Context())
	result, err := s.miauth.ReflectScopes(r.Context(), tokenID, session.OwnerActorID)
	if err != nil {
		s.writeAdminActionMiAuthError(w, r, "reflect scopes", err)
		return
	}
	if result.Changed {
		s.recordAdminAction(r, domain.WebAdminActionReflectScopes, tokenID,
			strPtr(result.OldScopes), strPtr(result.NewScopes))
	}
	// Unlike the three handlers above, this one reports something besides
	// {"ok":true}: the dashboard's own JS shows a one-line "no new
	// scopes" vs "updated" status before reloading, mirroring
	// miauthctl tokens reflect-scopes' own two-line CLI output.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "changed": result.Changed})
}

// handleAdminRSSAdd serves POST /admin/rss/add. Body:
// {"url": "https://...", "username": "optional, empty means none"}.
// Decoded inline rather than via decodeAdminActionTarget: that helper
// is hardcoded for exactly one string field, and this route needs two
// (url, optional username).
func (s *Server) handleAdminRSSAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL      string `json:"url"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeWebAdminError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	body.Username = strings.TrimSpace(body.Username)
	if body.URL == "" {
		writeWebAdminError(w, http.StatusBadRequest, "missing url")
		return
	}
	var username *string
	if body.Username != "" {
		username = &body.Username
	}
	session := AdminSessionFromContext(r.Context())
	if err := s.webadmin.AddRSSFeed(r.Context(), session.OwnerActorID, body.URL, username); err != nil {
		s.writeAdminActionRSSError(w, r, "add RSS feed", err)
		return
	}
	s.recordAdminAction(r, domain.WebAdminActionAddRSSFeed, body.URL, nil, username)
	writeWebAdminOK(w)
}

// handleAdminRSSRemove serves POST /admin/rss/remove. Body:
// {"url": "https://..."}.
func (s *Server) handleAdminRSSRemove(w http.ResponseWriter, r *http.Request) {
	url, ok := decodeAdminActionTarget(w, r, "url")
	if !ok {
		return
	}
	session := AdminSessionFromContext(r.Context())
	removedUsername, err := s.webadmin.RemoveRSSFeed(r.Context(), session.OwnerActorID, url)
	if err != nil {
		s.writeAdminActionRSSError(w, r, "remove RSS feed", err)
		return
	}
	s.recordAdminAction(r, domain.WebAdminActionRemoveRSSFeed, url, removedUsername, nil)
	writeWebAdminOK(w)
}

// recordAdminAction is every mutating handler's shared best-effort audit
// call (see internal/webadmin.Service.RecordAction's own doc comment for
// why this never fails the HTTP response): logged at Warn, not Error,
// since a failure here does not indicate anything actually went wrong
// for the operator's request — only that its bookkeeping trail is
// incomplete.
func (s *Server) recordAdminAction(r *http.Request, action domain.WebAdminAction, target string, before, after *string) {
	session := AdminSessionFromContext(r.Context())
	if err := s.webadmin.RecordAction(r.Context(), session, action, target, before, after); err != nil {
		s.logger.Warn("admin action audit write failed", "request_id", logging.RequestIDFromContext(r.Context()),
			"action", string(action), "target", target, "error", err.Error())
	}
}

// decodeAdminActionTarget reads {"<field>": "..."} from the request body
// and requires a non-empty value, writing a 400 and returning ok=false
// otherwise. All four mutating handlers above share this exact
// one-field-body shape.
func decodeAdminActionTarget(w http.ResponseWriter, r *http.Request, field string) (value string, ok bool) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body[field] == "" {
		writeWebAdminError(w, http.StatusBadRequest, "missing "+field)
		return "", false
	}
	return body[field], true
}

// writeAdminActionMiAuthError maps internal/miauth.Service's own
// sentinel errors to specific, named responses — deliberately NOT the
// generic-401 pattern writeWebAdminCeremonyError uses: that pattern is
// for pre-authentication failures where hiding the reason denies an
// attacker information; this caller is already an authenticated Owner
// session, so telling them exactly why their action didn't apply — the
// target changed state underneath them, e.g. someone else already
// approved it, or it's since expired — is strictly better UX with no
// security cost.
func (s *Server) writeAdminActionMiAuthError(w http.ResponseWriter, r *http.Request, logMsg string, err error) {
	switch {
	case errors.Is(err, miauth.ErrSessionUnavailable):
		writeWebAdminError(w, http.StatusConflict, "session is no longer pending — it may have already been approved, rejected, or expired")
	case errors.Is(err, miauth.ErrTokenRevoked):
		writeWebAdminError(w, http.StatusConflict, "token is revoked; reflect-scopes only applies to active tokens")
	case errors.Is(err, miauth.ErrOriginatingSessionGone):
		writeWebAdminError(w, http.StatusConflict, "token has no recoverable originating session; re-issue a new token via a fresh Aria login")
	case errors.Is(err, domain.ErrNotFound):
		writeWebAdminError(w, http.StatusNotFound, "not found")
	default:
		s.logger.Error(logMsg+" failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeWebAdminError(w, http.StatusInternalServerError, "internal error")
	}
}

// writeAdminActionRSSError maps internal/webadmin's RSS sentinel errors
// to specific, named responses — same "specific once authenticated"
// reasoning as writeAdminActionMiAuthError (plan-136-phase3 §1 Decision
// 2), extended with one more case (domain.ErrConflict) this phase
// introduces for the first time in the Web UI (MiAuth's own actions
// have no equivalent optimistic-concurrency path today).
func (s *Server) writeAdminActionRSSError(w http.ResponseWriter, r *http.Request, logMsg string, err error) {
	switch {
	case errors.Is(err, webadmin.ErrRSSFeedAlreadyExists):
		writeWebAdminError(w, http.StatusConflict, "that RSS feed URL is already configured")
	case errors.Is(err, webadmin.ErrRSSFeedNotFound):
		writeWebAdminError(w, http.StatusNotFound, "that RSS feed URL is not currently configured")
	case errors.Is(err, webadmin.ErrRSSFeedCannotRemoveLastBootstrapFeed):
		writeWebAdminError(w, http.StatusConflict, err.Error())
	case errors.Is(err, webadmin.ErrRSSFeedInvalid):
		writeWebAdminError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrNotFound):
		// domain.ErrNotFound here (as opposed to webadmin.ErrRSSFeedNotFound
		// above) can only reach this branch through the narrow TOCTOU window
		// in RemoveRSSFeed/unsetRSSFeedURLs where the app_config row was
		// concurrently unset between currentRSSFeedURLs' read and this
		// request's own write — same "someone else changed it, reload" story
		// as ErrConflict, so it gets the same message rather than a 404 that
		// would misleadingly suggest the URL itself was never configured.
		writeWebAdminError(w, http.StatusConflict, "RSS_FEED_URLS was changed concurrently; reload the page and try again")
	default:
		s.logger.Error(logMsg+" failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeWebAdminError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeWebAdminOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func strPtr(s string) *string { return &s }

// adminDashboardTemplate is Issue #136 Phase 3's dashboard markup.
// Every Aria-supplied field (RequestedPermissions, ClientCallback) goes
// through plain {{.Field}} interpolation — html/template's contextual
// auto-escaping handles the untrusted-data case correctly by
// construction, the same guarantee Phase 2's CSRF-token rendering
// already relies on. This is the direct web-UI analog of ADR-0002's
// `miauthctl approve` displaying the same fields through sanitized
// terminal output before an operator approves: showing them lets the
// operator verify a pending session's details before acting, and must
// never be bypassed with a template.HTML-style escape hatch.
var adminDashboardTemplate = template.Must(template.New("adminDashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="admin-csrf-token" content="{{.CSRFToken}}">
<title>Admin</title>
</head>
<body>
<p>Logged in as the Owner. <button id="logout">Log out</button></p>

<h2>Pending MiAuth sessions</h2>
{{if not .Sessions}}<p>No pending sessions.</p>{{end}}
{{range .Sessions}}
<div class="session-row" data-route-session-id="{{.RouteSessionID}}">
  <p>Session {{.RouteSessionID}} — created {{.CreatedAt}}, expires {{.ExpiresAt}}</p>
  <p>Requested permissions: {{.RequestedPermissions}}</p>
  {{if .ClientCallback}}<p>Callback: {{.ClientCallback}}</p>{{end}}
  <button class="approve">Approve</button>
  <button class="reject">Reject</button>
</div>
{{end}}

<h2>API tokens</h2>
{{if not .Tokens}}<p>No tokens.</p>{{end}}
{{range .Tokens}}
<div class="token-row" data-token-id="{{.ID}}">
  <p>Token {{.ID}} — scopes: {{.Scopes}}{{if .RevokedAt}} (revoked){{end}}</p>
  <button class="revoke">Revoke</button>
  <button class="reflect-scopes">Reflect scopes</button>
</div>
{{end}}

<h2>RSS feeds</h2>
{{if not .RSSEnabled}}<p><em>RSS ingestion is currently disabled (RSS_ENABLED=false); changes here take effect once it is enabled and the service is restarted.</em></p>{{end}}
<p>Source: {{if .RSSFeedsFromOverride}}database override{{else}}bootstrap (config file/environment){{end}}.
{{if .RSSFilterScriptPathConfigured}}A filter script is configured (RSS_FILTER_SCRIPT_PATH); editing it stays CLI/file-only.{{else}}No filter script is configured.{{end}}</p>
{{if not .RSSFeeds}}<p>No feeds configured.</p>{{end}}
{{range .RSSFeeds}}
<div class="rss-feed-row" data-url="{{.URL}}">
  <p>{{.URL}}{{if .Username}} (posts as {{.Username}}){{end}}</p>
  <button class="remove-feed">Remove</button>
</div>
{{end}}
<form id="rss-add-form">
  <input type="url" id="rss-add-url" placeholder="https://example.com/feed.xml" required>
  <input type="text" id="rss-add-username" placeholder="username (optional)">
  <button type="submit">Add feed</button>
</form>

<p id="status"></p>
<script>
(function () {
  "use strict";
  function csrfToken() { return document.querySelector('meta[name="admin-csrf-token"]').content; }
  function setStatus(text) { document.getElementById("status").textContent = text; }

  async function postAction(url, body, confirmText) {
    if (confirmText && !window.confirm(confirmText)) return;
    var resp = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Admin-CSRF-Token": csrfToken() },
      body: JSON.stringify(body)
    });
    if (!resp.ok) {
      var err = await resp.json().catch(function () { return {}; });
      setStatus("Action failed: " + (err.error || resp.status));
      return;
    }
    document.location.reload();
  }

  document.getElementById("logout").addEventListener("click", function () {
    postAction("/admin/logout", {}, null);
  });
  document.querySelectorAll(".session-row").forEach(function (row) {
    var id = row.dataset.routeSessionId;
    row.querySelector(".approve").addEventListener("click", function () {
      postAction("/admin/sessions/approve", { routeSessionId: id }, "Approve session " + id + "?");
    });
    row.querySelector(".reject").addEventListener("click", function () {
      postAction("/admin/sessions/reject", { routeSessionId: id }, "Reject session " + id + "?");
    });
  });
  document.querySelectorAll(".token-row").forEach(function (row) {
    var id = row.dataset.tokenId;
    row.querySelector(".revoke").addEventListener("click", function () {
      postAction("/admin/tokens/revoke", { tokenId: id }, "Revoke token " + id + "? This cannot be undone.");
    });
    row.querySelector(".reflect-scopes").addEventListener("click", function () {
      postAction("/admin/tokens/reflect-scopes", { tokenId: id }, null);
    });
  });
  document.querySelectorAll(".rss-feed-row").forEach(function (row) {
    var url = row.dataset.url;
    row.querySelector(".remove-feed").addEventListener("click", function () {
      postAction("/admin/rss/remove", { url: url }, "Remove feed " + url + "?");
    });
  });
  document.getElementById("rss-add-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var url = document.getElementById("rss-add-url").value;
    var username = document.getElementById("rss-add-username").value;
    postAction("/admin/rss/add", { url: url, username: username }, null);
  });
})();
</script>
</body>
</html>
`))
