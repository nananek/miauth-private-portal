package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/webadmin"
)

// handleAdminSetup serves GET /admin/setup?token=<raw>. Read-only: calls
// s.webadmin.CheckBootstrapToken and renders either the static
// registration page or a plain-text "this link is invalid or has
// expired" message. The registration page never interpolates the token
// (or any other attacker-influenced value) into its markup — it is
// served byte-for-byte identical regardless of which valid token was
// presented, and its own inline script reads the token back out of
// window.location.search at runtime instead, so there is nothing here
// for docs/operations/security-regression.md's XSS/escaping posture to
// regress on (mirrors handleMiAuthStart's own "never reflects query
// values" precedent, taken one step further: this page reflects
// nothing at all).
func (s *Server) handleAdminSetup(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if err := s.webadmin.CheckBootstrapToken(r.Context(), token); err != nil {
		if !errors.Is(err, webadmin.ErrBootstrapTokenInvalid) {
			s.logger.Error("webadmin setup check failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		}
		writePlainTextPage(w, http.StatusBadRequest, "This link is invalid or has expired.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(adminSetupPageHTML))
}

// handleAdminSetupBegin serves POST /admin/setup/begin. Body:
// {"token": "<raw>"}. Calls s.webadmin.BeginRegistration and writes the
// returned *protocol.CredentialCreation as the JSON response body —
// go-webauthn's own protocol package already defines its JSON shape, so
// this handler is a thin passthrough, not a custom wire format.
func (s *Server) handleAdminSetupBegin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeWebAdminError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	creation, err := s.webadmin.BeginRegistration(r.Context(), body.Token)
	if err != nil {
		s.writeWebAdminCeremonyError(w, r, "webadmin begin registration failed", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(creation)
}

// handleAdminSetupFinish serves POST /admin/setup/finish?token=<raw>.
// The request body IS the browser's raw navigator.credentials.create()
// response (go-webauthn's FinishRegistration reads r.Body itself) —
// token travels as a query parameter here, not the body, for exactly
// that reason. On success it responds 200 with a minimal JSON body:
// Phase 1 issues no cookie and starts no session, so there is nothing
// else to return; the page's own script shows a "you're registered"
// message and does not redirect anywhere requiring authentication.
func (s *Server) handleAdminSetupFinish(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if _, err := s.webadmin.FinishRegistration(r.Context(), token, r); err != nil {
		s.writeWebAdminCeremonyError(w, r, "webadmin finish registration failed", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// writeWebAdminCeremonyError maps every webadmin.Service ceremony error
// to a response: ErrBootstrapTokenInvalid, ErrSessionInvalid, and
// ErrCeremonyFailed are all cases a caller can hit through no fault of
// the server — an expired/replayed link or session, or a WebAuthn
// response that failed verification (a tampered/forged assertion is a
// client-presented-bad-proof condition, not a server error) — and all
// three get 401 with a generic body, mirroring writeAuthenticationFailed's
// precedent of never revealing *why* an auth-adjacent value was rejected
// (plan-136-phase2 §6.2). Everything else (an unexpected storage error)
// is logged with the request id and returned as a generic 500, never the
// raw error text.
func (s *Server) writeWebAdminCeremonyError(w http.ResponseWriter, r *http.Request, logMsg string, err error) {
	if errors.Is(err, webadmin.ErrBootstrapTokenInvalid) {
		writeWebAdminError(w, http.StatusUnauthorized, "bootstrap token is invalid, expired, or already used")
		return
	}
	if errors.Is(err, webadmin.ErrSessionInvalid) {
		writeWebAdminError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if errors.Is(err, webadmin.ErrCeremonyFailed) {
		writeWebAdminError(w, http.StatusUnauthorized, "WebAuthn verification failed")
		return
	}
	s.logger.Error(logMsg, "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
	writeWebAdminError(w, http.StatusInternalServerError, "internal error")
}

func writeWebAdminError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// adminSetupPageHTML is Issue #136 Phase 1's entire admin surface: a
// static, dependency-free page that runs the WebAuthn registration
// ceremony via two fetch() calls against the endpoints above. It embeds
// no server-supplied data of any kind (see handleAdminSetup's doc
// comment) — the token is read from window.location.search by the
// script itself, at the client, not written into this string by Go.
const adminSetupPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Register a passkey</title>
</head>
<body>
<p id="status">Preparing registration&hellip;</p>
<script>
(function () {
  "use strict";

  function base64urlToBuffer(value) {
    var padded = value.replace(/-/g, "+").replace(/_/g, "/");
    while (padded.length % 4) padded += "=";
    var binary = atob(padded);
    var bytes = new Uint8Array(binary.length);
    for (var i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    return bytes.buffer;
  }

  function setStatus(text) {
    document.getElementById("status").textContent = text;
  }

  async function run() {
    var token = new URLSearchParams(window.location.search).get("token");
    if (!token) {
      setStatus("This link is missing its token.");
      return;
    }
    if (!window.PublicKeyCredential) {
      setStatus("This browser does not support passkeys (WebAuthn).");
      return;
    }

    var beginResp = await fetch("/admin/setup/begin", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: token })
    });
    if (!beginResp.ok) {
      setStatus("This link is invalid or has expired.");
      return;
    }
    var creation = await beginResp.json();
    var publicKey = creation.publicKey;
    publicKey.challenge = base64urlToBuffer(publicKey.challenge);
    publicKey.user.id = base64urlToBuffer(publicKey.user.id);
    if (publicKey.excludeCredentials) {
      publicKey.excludeCredentials = publicKey.excludeCredentials.map(function (c) {
        return Object.assign({}, c, { id: base64urlToBuffer(c.id) });
      });
    }

    setStatus("Follow your browser's prompt to create a passkey.");
    var credential;
    try {
      credential = await navigator.credentials.create({ publicKey: publicKey });
    } catch (e) {
      setStatus("Passkey creation was cancelled or failed: " + e.message);
      return;
    }

    var finishResp = await fetch("/admin/setup/finish?token=" + encodeURIComponent(token), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(credential.toJSON())
    });
    if (!finishResp.ok) {
      setStatus("This link is invalid or has expired.");
      return;
    }
    setStatus("Passkey registered. You can close this tab.");
  }

  run().catch(function (e) {
    setStatus("Registration failed: " + e.message);
  });
})();
</script>
</body>
</html>
`

// handleAdminLogin serves GET /admin/login: a static, dependency-free
// page structurally identical to Phase 1's adminSetupPageHTML (zero
// server-side interpolation — see that page's own doc comment for the
// XSS-posture reasoning, unchanged here) whose inline script drives the
// two-call login ceremony below and, on success, sets document.location
// to /admin/.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(adminLoginPageHTML))
}

// handleAdminLoginBegin serves POST /admin/login/begin (no body needed
// — single-Owner deployment, no username to submit). Calls
// s.webadmin.BeginLogin, returns {"sessionId": "...", "publicKey": {...}}
// — the assertion options plus the pending session's own row ID, which
// the page's script must echo back on /admin/login/finish (mirrors
// Phase 1's token-as-query-param convention, not a new cookie).
// webadmin.ErrNoCredentialsRegistered maps to 400 with a message
// telling the operator to run `miauthctl web-login issue` first, not a
// generic auth failure — this is a deployment-state problem, not a
// forged/expired credential attempt, and hiding it would only confuse a
// legitimate first-time operator.
func (s *Server) handleAdminLoginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, sessionID, err := s.webadmin.BeginLogin(r.Context())
	if err != nil {
		if errors.Is(err, webadmin.ErrNoCredentialsRegistered) {
			writeWebAdminError(w, http.StatusBadRequest, "no passkey is registered yet; run miauthctl web-login issue first")
			return
		}
		s.writeWebAdminCeremonyError(w, r, "webadmin begin login failed", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sessionId": sessionID,
		"publicKey": assertion.Response,
	})
}

// handleAdminLoginFinish serves POST /admin/login/finish?sessionId=....
// Body is the browser's raw navigator.credentials.get() response. Calls
// s.webadmin.FinishLogin; on success sets the Decision-5 cookie (Secure
// in production/HttpOnly/SameSite=Strict/Path=/admin, value the raw
// session token, Expires matching the session's own ExpiresAt) and
// responds 200 with {"ok": true}. webadmin.ErrSessionInvalid -> 401
// generic body via writeWebAdminCeremonyError, matching
// handleAdminSetupFinish's existing error-mapping shape exactly.
func (s *Server) handleAdminLoginFinish(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	rawToken, session, err := s.webadmin.FinishLogin(r.Context(), sessionID, r)
	if err != nil {
		s.writeWebAdminCeremonyError(w, r, "webadmin finish login failed", err)
		return
	}
	http.SetCookie(w, adminSessionCookie(rawToken, session.ExpiresAt, s.adminCookieSecure()))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleAdminLogout serves POST /admin/logout (behind RequireAdminSession
// + RequireAdminCSRF). Revokes the session, clears the cookie (Set-Cookie
// with MaxAge=-1, matching Name/Path/Secure/HttpOnly/SameSite exactly —
// a clearing cookie whose attributes don't match the original is
// silently ignored by the browser), responds 200.
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	session := AdminSessionFromContext(r.Context())
	if err := s.webadmin.Logout(r.Context(), session.ID); err != nil {
		s.logger.Error("webadmin logout failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeWebAdminError(w, http.StatusInternalServerError, "internal error")
		return
	}
	cookie := adminSessionCookie("", time.Unix(0, 0), s.adminCookieSecure())
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// adminCookieSecure reports whether the admin session cookie should
// carry the Secure attribute: derived from s.localOrigin's scheme
// (mirrors LOCAL_ORIGIN's own production-https-enforcement logic —
// see plan-136-phase2 §6.3 — rather than introducing a second "are we
// in production" signal).
func (s *Server) adminCookieSecure() bool {
	return strings.HasPrefix(s.localOrigin, "https://")
}

func adminSessionCookie(raw string, expiresAt time.Time, secure bool) *http.Cookie {
	return &http.Cookie{
		Name: adminSessionCookieName, Value: raw, Path: "/admin",
		Expires: expiresAt, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	}
}

// adminLoginPageHTML is Issue #136 Phase 2's login surface: a static,
// dependency-free page that runs the WebAuthn login ceremony via two
// fetch() calls against the endpoints above, mirroring
// adminSetupPageHTML's own "no server-supplied data, ever" construction
// (see that constant's doc comment) — nothing here is interpolated by
// Go.
const adminLoginPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Admin login</title>
</head>
<body>
<p id="status">Preparing login&hellip;</p>
<script>
(function () {
  "use strict";

  function base64urlToBuffer(value) {
    var padded = value.replace(/-/g, "+").replace(/_/g, "/");
    while (padded.length % 4) padded += "=";
    var binary = atob(padded);
    var bytes = new Uint8Array(binary.length);
    for (var i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    return bytes.buffer;
  }

  function setStatus(text) {
    document.getElementById("status").textContent = text;
  }

  async function run() {
    if (!window.PublicKeyCredential) {
      setStatus("This browser does not support passkeys (WebAuthn).");
      return;
    }

    var beginResp = await fetch("/admin/login/begin", { method: "POST" });
    if (!beginResp.ok) {
      setStatus("Unable to start login. Is a passkey registered?");
      return;
    }
    var begin = await beginResp.json();
    var publicKey = begin.publicKey;
    publicKey.challenge = base64urlToBuffer(publicKey.challenge);
    if (publicKey.allowCredentials) {
      publicKey.allowCredentials = publicKey.allowCredentials.map(function (c) {
        return Object.assign({}, c, { id: base64urlToBuffer(c.id) });
      });
    }

    setStatus("Follow your browser's prompt to use your passkey.");
    var credential;
    try {
      credential = await navigator.credentials.get({ publicKey: publicKey });
    } catch (e) {
      setStatus("Login was cancelled or failed: " + e.message);
      return;
    }

    var finishResp = await fetch("/admin/login/finish?sessionId=" + encodeURIComponent(begin.sessionId), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(credential.toJSON())
    });
    if (!finishResp.ok) {
      setStatus("Login failed. Try again.");
      return;
    }
    setStatus("Logged in. Redirecting…");
    document.location = "/admin/";
  }

  run().catch(function (e) {
    setStatus("Login failed: " + e.message);
  });
})();
</script>
</body>
</html>
`
