# ADR-0010: Admin Web UI authentication — SSH-anchored bootstrap + WebAuthn

- Status: Proposed for Issue #136
- Date: 2026-09-10
- Scope: a new browser-based admin surface for MiAuth session/token management
  and RSS feed management (Issue #136's "第一弾"). Does not touch Aria's own
  MiAuth/API-token flow (ADR-0001/ADR-0002), which is unchanged.
- Supersedes: ADR-0002's "browser session cookies ... out of scope" exclusion
  only. ADR-0002's core decision — CLI/SSH-approved MiAuth sessions, no public
  first-login-wins path, authenticated host access as *a* trust point — remains
  in force and is in fact the root of trust this ADR anchors to.

## Context

Issue #136 asks for a Web UI covering (1) MiAuth session approve/reject and API
token list/revoke (today: `miauthctl` only) and (2) RSS feed add/remove/list
(today: `miauthctl config set/unset RSS_FEED_URLS`, or direct `.env` editing).
ADR-0002 explicitly excluded browser session cookies. This ADR decides how to
introduce a browser-authenticated admin surface without weakening ADR-0002's
"no public first-login-wins, SSH access is the trust point" guarantee, and
without inventing a second, disconnected owner concept.

## Decision

1. **A fifth, structurally distinct credential type.** ADR-0001's "keep four
   credential records distinct" rule gets a fifth entry: a **web admin session**.
   New table(s) (`web_admin_bootstrap_tokens`, `web_admin_credentials`,
   `web_admin_sessions`), never reusing `api_tokens`/`miauth_local_sessions`
   schema or Go types. A web admin session authenticates a *browser*, not Aria;
   it is never accepted anywhere `RequireScope` is, and vice versa.

2. **SSH/host access remains the sole root of trust — reachable only via a new
   `miauthctl web-login` bootstrap subcommand.** An operator with host access
   runs `miauthctl web-login issue`, which:
   - generates a single-use bootstrap token with `crypto/rand` (mirroring
     `internal/miauth`'s existing token generation);
   - stores only its hash (mirroring `api_tokens.token_hash`);
   - sets a 10-minute TTL (matching `internal/miauth/service.go`'s existing
     `localSessionTTL` convention — no reason for this credential to live
     longer, and precedent already exists for this exact number in this
     codebase);
   - prints a `https://<LOCAL_ORIGIN>/admin/setup?token=<raw>` URL to stdout —
     **never over HTTP, never logged** (same redaction rule as every other raw
     token in this codebase).
   There is still no public, unauthenticated path to create a web admin
   credential — exactly ADR-0002's "no first-login-wins" property, just for a
   second credential type.

3. **The bootstrap token's only capability is completing WebAuthn
   registration.** Opening the setup URL and presenting a valid, unexpired,
   not-yet-consumed token starts a narrowly-scoped "registration ceremony"
   session (its own short-lived, single-purpose cookie — cannot read/write any
   admin resource) that can do exactly one thing: call
   `navigator.credentials.create()` and register one WebAuthn public-key
   credential. The bootstrap token is atomically consumed (compare-and-set,
   single winner — the same pattern `LocalMiAuthSessionRepository.Authorize`/
   `Consume` already establishes) the moment registration succeeds, or on
   expiry, whichever first. It cannot be reused to register a second
   credential; run `web-login issue` again for that (operators are expected to
   register once per device, e.g. a phone and a laptop — see Decision 6).

4. **WebAuthn/passkey is the only day-to-day login mechanism after the first
   registration.** `POST /admin/login/webauthn` (challenge/assertion ceremony)
   verifies against the Owner's registered credential(s) and, on success,
   mints a normal admin session (Decision 5). No password exists anywhere in
   this design — nothing to phish, brute-force, or leak.

5. **Session cookie, meeting every AGENTS.md cookie rule explicitly:**
   - `Secure` (production; same `LOCAL_ORIGIN`-is-https convention as the rest
     of this service — allow plain HTTP only under the same non-production
     carve-out `LOCAL_ORIGIN` itself already has for local dev);
   - `HttpOnly` (never readable from JS — nothing in the admin UI's own JS
     needs to read the session cookie value);
   - `SameSite=Strict` (single-owner deployment, no legitimate cross-site
     navigation target ever needs to carry this cookie);
   - scoped to `Path=/admin` only — never sent on any Aria-facing `/api/*`
     request, keeping the two authentication surfaces structurally
     unreachable from each other, not just conceptually separate;
   - a fresh, `crypto/rand`, hash-at-rest session ID minted on every login
     (rotated after authentication, per AGENTS.md), the previous one (if any)
     left to expire normally rather than being explicitly revoked — logging
     in from a second device is a legitimate, expected action (Decision 6),
     not a takeover to punish;
   - explicit bounded lifetime: a fixed `ADMIN_SESSION_TTL` (recommend a
     bootstrap-only config default of 12h — a low-traffic admin surface, and
     "re-authenticate with your passkey once a day" costs an operator nothing
     and bounds a stolen-cookie's blast radius) — **fixed, not sliding**: an
     operator who wants to stay logged in re-authenticates with a fingerprint/
     face tap, no meaningful UX cost, versus a sliding window's larger
     stolen-cookie exposure window.

6. **Multiple registered credentials, one Owner, no new login-capable actor
   type.** A web admin credential is bound to *the* Owner actor (ADR-0002's
   singleton) — never a new "web user" record. An operator may register
   several credentials (phone, laptop, hardware key) for the same Owner, each
   independently listable and revocable (`miauthctl web-login list-credentials`
   / `revoke-credential <id>`, and equivalently from the Web UI once logged
   in — self-service revoke of *other* credentials, not the one currently in
   use, to avoid an accidental self-lockout mid-session). **Recovery always
   stays possible via SSH**: even if every passkey is lost/revoked, `miauthctl
   web-login issue` on the host re-bootstraps a new one. This is deliberate —
   it's what keeps "authenticated host access is the trust point" true for
   this credential type too, exactly mirroring how ADR-0002 keeps SSH as the
   recovery path for local API tokens (`miauthctl approve`/`revoke`).

7. **CSRF: `SameSite=Strict` plus a defense-in-depth synchronizer token.**
   `SameSite=Strict` alone is strong protection in evergreen browsers, but this
   codebase's security posture (AGENTS.md's whole "Security and privacy"
   section) favors layered defenses over a single control. Every
   state-changing `/admin/*` route additionally requires a per-session
   synchronizer token (embedded in the page, sent back as a custom header,
   e.g. `X-Admin-CSRF-Token`) — cheap to implement (one random value stored
   alongside the session row, compared on every mutating request) and closes
   the residual gap `SameSite=Strict` alone leaves against a same-site
   embedded-content edge case.

8. **Every admin action performed via the Web UI is audited**, mirroring
   Issue #133's `api_token_scope_audit` shape exactly: a `web_admin_action_audit`
   table, one row per approve/reject/revoke/RSS-feed-change, written in the
   same transaction as the underlying write, recording `{credential_id,
   action, target, before, after, changed_at}`. This closes a gap that only
   matters once more than one credential can act as "the operator": recording
   which one did what is worth the (small) cost, even though ADR-0002's
   CLI-only model never needed it. **This ADR does not propose retrofitting
   the same audit trail onto the CLI path** — `miauthctl`'s
   existing SSH-is-the-boundary model (ADR-0002) is unchanged and still
   self-consistent; only the *new* multi-credential Web UI path needs it.

9. **New dependency: a WebAuthn library, not a hand-rolled implementation.**
   WebAuthn's wire format (CBOR-encoded attestation objects, COSE public keys,
   the full ceremony state machine, replay/origin/RP-ID validation) is a
   large, security-critical surface where a subtle bug is a full
   authentication bypass — exactly the kind of code this repo's "prefer
   stdlib or small, maintained libraries" rule (AGENTS.md) is not asking
   engineers to reimplement from scratch. Recommend `github.com/go-webauthn/
   webauthn` — pure Go, no cgo (compatible with this repo's
   `CGO_ENABLED=0` static-binary build), the de facto standard Go WebAuthn
   server library as of this writing. **Implementer must verify the exact
   current module path, license, and latest tagged version at
   implementation time** (no network access during this planning pass to
   confirm); if that library has since been abandoned/renamed, the fallback
   is any other actively-maintained pure-Go WebAuthn server library meeting
   the same no-cgo/no-network-calls-of-its-own bar this repo already applies
   to `go.starlark.net` (Issue #135's ADR-0009 Consequences section is the
   template for that justification).

## Consequences

- New tables: `web_admin_bootstrap_tokens`, `web_admin_credentials`,
  `web_admin_sessions`, `web_admin_action_audit`. New migration(s) for all
  four, forward-only per this repo's migration rules.
- New package, e.g. `internal/webadmin`, holding the bootstrap-token,
  credential, and session domain logic — kept behind narrow repository
  interfaces exactly like every other domain package (AGENTS.md's
  architecture rules), so `internal/httpserver`'s new `/admin/*` handlers
  stay thin.
- New `cmd/miauthctl web-login` subcommand family (issue / list-credentials /
  revoke-credential), matching `config`/`tokens`'s existing dispatch shape.
- New dependency: a WebAuthn library (Decision 9).
- `docs/operations/security-regression.md`'s "Cookie attributes: Not
  applicable" row must be replaced with real test evidence once this ships
  (own follow-up, tracked per-phase in the implementation plan).
- No change to Aria's own MiAuth/API-token flow, `RequireScope`, or any
  `/api/*` route — the two authentication surfaces remain structurally
  separate (Decision 5's `Path=/admin` scoping).

## Revisit if

- A genuine second human operator ever needs independent admin access —
  this design is still fundamentally single-Owner (multiple *credentials*,
  one *Owner*); true multi-user admin is out of scope and would need its own
  ADR, consistent with AGENTS.md's "no multi-user behavior unless an issue
  explicitly promotes it."
- The admin surface grows beyond MiAuth admin + RSS feed CRUD into general
  config editing (including secrets) — AGENTS.md's secret-handling rules
  and this repo's CLI-only-for-secrets convention (`internal/config`'s
  `IsSecretKey`) would need explicit re-examination before any secret ever
  becomes browser-editable, and is out of scope for the phases planned under
  Issue #136.
