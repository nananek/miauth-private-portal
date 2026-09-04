# Configuration

This document covers the configuration and HTTP-routing foundation added by
Issue #3, the SQLite persistence layer added by Issue #4, the bridged
MiAuth authentication flow added by Issue #5, and the Aria/Misskey-compatible
note API added by Issue #7. It does not cover LLM configuration; that is
added by a later issue and will extend this document rather than replace
it. The normative design for MiAuth is
[`docs/decisions/0001-auth-topology.md`](../decisions/0001-auth-topology.md)
(ADR-0001) and [`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md);
this document covers only the operational surface (config keys, routes,
the bootstrap tool), not the protocol design itself.

## Loading order

`internal/config.Load` resolves each setting in this order, later sources
overriding earlier ones:

1. built-in defaults;
2. an optional dotenv-style config file (`.env` in the working directory by
   default; override the path with `CONFIG_FILE`);
3. real process environment variables.

A missing config file is not an error. An unknown key, an invalid value, or
a missing required field fails startup immediately with a redacted error
that names the offending key and reason, never the value that was
supplied.

An environment variable that is *set but empty* (for example an unresolved
`${VAR}` in a `docker-compose.yml` or systemd `EnvironmentFile`) is also a
startup error rather than a silent override: `Load` cannot tell whether an
empty value is intentional, and treating it as "unset" would silently
discard a config-file value with no diagnostic at all. Unset the variable
entirely to fall back to the config file/default instead.

### Why environment-variable scanning is scoped to known keys

The config file is fully owned by this application, so `Load` can safely
reject any key in it that is not recognized. The process environment is
not: unrelated variables such as `PATH`, `HOME`, or `LANG` are normal and
must never fail startup. `Load` therefore only performs a *named* lookup
of the known keys listed below; it never scans `os.Environ()`. The
trade-off is that a typo'd environment variable name (for example
`HTTP_PROT` instead of `HTTP_PORT`) is silently ignored rather than
rejected — the config file's stricter unknown-key check is the place to
catch that class of mistake during local development.

## Known configuration keys

| Key | Required | Default | Notes |
| --- | --- | --- | --- |
| `APP_ENV` | yes | — | One of `development`, `staging`, `production`. |
| `HTTP_HOST` | no | `0.0.0.0` | Listen host. |
| `HTTP_PORT` | no | `8080` | 1-65535. |
| `HTTP_READ_TIMEOUT` | no | `5s` | `time.ParseDuration` format, must be positive. |
| `HTTP_READ_HEADER_TIMEOUT` | no | `5s` | Same format/rules. |
| `HTTP_WRITE_TIMEOUT` | no | `15s` | Same format/rules. Must be at least `1s` greater than `UPSTREAM_HTTP_TIMEOUT`, leaving time to return an upstream timeout response. |
| `HTTP_IDLE_TIMEOUT` | no | `60s` | Same format/rules. |
| `HTTP_MAX_BODY_BYTES` | no | `1048576` (1 MiB) | Enforced via `http.MaxBytesReader` on every request. |
| `HTTP_SHUTDOWN_GRACE_PERIOD` | no | `15s` | Bounds how long graceful shutdown waits before forcing connections closed. |
| `LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error`. Must not be `debug` in production. |
| `LOG_FORMAT` | no | `text` | `json` or `text`. Must be `json` in production. |
| `DB_PATH` | no | `./data/portal.db` | SQLite database file path; its parent directory is created if missing. Must not be empty. |
| `DB_BUSY_TIMEOUT_MS` | no | `5000` | Positive integer milliseconds passed to SQLite's `busy_timeout` pragma. |
| `DB_MAX_OPEN_CONNS` | no | `8` | 1-100. Bounds the SQLite connection pool. |
| `LOCAL_ORIGIN` | yes | — | This service's own public origin (ADR-0001 `LOCAL_ORIGIN`). Scheme+host only: no userinfo, path beyond `""`/`"/"`, query, or fragment. Must be `https` in production. |
| `IDENTITY_ORIGIN` | yes | — | The fixed upstream Misskey origin used for owner verification (ADR-0001 `IDENTITY_ORIGIN`). Same format/production rules as `LOCAL_ORIGIN`. Never supplied by a client request. |
| `ALLOWED_MISSKEY_USER_ID` | no | `""` (bootstrap-only) | The opaque upstream Misskey user ID allowed to bind as this deployment's single owner. Never logged or returned to a client; `Config.Redacted()` shows only whether it is set. |
| `ARIA_CLIENT_CALLBACKS` | no | `""` (reject any client callback) | Comma-separated exact-match allowlist of Aria's client return callbacks (for example `aria://aria/miauth`). Commas inside a callback path or query are retained; a separator is a comma followed by the next absolute URL scheme. A non-HTTPS scheme is explicitly allowed here, unlike the two origins above. |
| `UPSTREAM_HTTP_TIMEOUT` | no | `10s` | Bounds every HTTP call this service makes to `IDENTITY_ORIGIN`. `time.ParseDuration` format, must be positive and at least `1s` shorter than `HTTP_WRITE_TIMEOUT`. |
| `OWNER_USERNAME` | no | `owner` | ASCII letters, digits, and underscores only. Reported as the owner's `UserDetailedNotMe.username` until a later issue adds self-service profile editing. |
| `OWNER_DISPLAY_NAME` | no | `""` (null) | Reported as the owner's `UserDetailedNotMe.name` (nullable); empty means `null`. |

`internal/config.KnownKeys()` is the single source of truth this table is
generated from by hand; keep them in sync when a key is added or removed.

## Production hardening

When `APP_ENV=production`, `Config.Validate` additionally requires
`LOG_FORMAT=json` and rejects `LOG_LEVEL=debug`, so an operator cannot
accidentally ship a production deployment with development-oriented,
higher-verbosity logging.

## Redaction

`internal/logging` redacts a fixed set of structured-log attribute keys
(`authorization`, `cookie`, `set-cookie`, `i`, `token`, `access_token`,
`api_key`, `apikey`, `state`, `miauth_state`, `password`, `secret`,
`body`, `prompt`, `mail_body`), case-insensitively and regardless of
`slog.Group` nesting, matching AGENTS.md's rule against logging
authorization headers, cookies, MiAuth state, API keys, and message
bodies. This is a fixed key allowlist, not a value scanner: a secret
embedded in an unlisted free-text field is not caught by this mechanism,
so code must not concatenate secrets into arbitrary log fields.

The HTTP access-log middleware (`internal/logging.AccessLog`) logs the
**route pattern** a handler was registered under (for example
`/miauth/{session}`), never the raw request path, query string, headers,
or body. ADR-0001 fixes Aria's `{session}` route value, the upstream MiAuth
`state`, and local API tokens as secrets that must never reach a log line;
Issue #5's MiAuth handlers rely on this pattern-only logging to satisfy
that, and additionally never construct a log attribute containing any of
those values in the first place (see
[`TestMiAuthFlow_NeverLogsSensitiveValues`](../../internal/httpserver/miauth_handlers_test.go)).

`Config.Redacted()` is the one place that decides which config fields are
safe to print. Issue #5 adds the first field this genuinely applies to:
`ALLOWED_MISSKEY_USER_ID` is shown only as `<set>`/`<unset>`, never its
value, per its acceptance criteria (an unauthorized login attempt's
generic denial must never let the allowlisted ID leak into a log or
response).

## HTTP routing

Routing uses the standard library's `net/http.ServeMux` with Go 1.22+
method+path patterns (e.g. `GET /healthz`, and later `GET
/miauth/{session}`). This service's Misskey-compatible surface is a small,
mostly-static set of routes with at most one path parameter per route,
which `ServeMux` already expresses directly. AGENTS.md requires a concrete
reason for any new dependency, and none exists yet for a third-party
router; if a future issue's routing needs (regex constraints, richer
sub-router middleware composition, etc.) outgrow `ServeMux`, record that
decision in a new ADR at that time. `docs/decisions/0002-*` is already
reserved by the Open WebUI roadmap's OWUI-C track, so this decision is
intentionally not filed as an ADR here.

## MiAuth

Issue #5 adds the bridged MiAuth flow ADR-0001 designs. This section covers
only the operational surface; the protocol itself (state machines, owner
binding rules, threat model) is normative in ADR-0001, and the exact wire
shapes Aria expects are normative in
[`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md).

### Routes

| Route | Purpose |
| --- | --- |
| `GET /miauth/{session}` | Aria's entry point. Starts (or idempotently resumes) the local session and redirects the browser to `IDENTITY_ORIGIN` for owner verification. |
| `GET /miauth/callback` | The fixed internal callback `IDENTITY_ORIGIN` redirects back to, shared by the ordinary and bootstrap flows. Not part of Aria's contract; never call it directly. |
| `GET /miauth/bootstrap/{gate}` | Operator-only entry point reached with a gate value from `cmd/bootstrapctl`. Refuses once an owner is already bound. |
| `POST /api/miauth/{session}/check` | Aria polls this to complete the flow. Every non-success outcome (pending, denied, expired, replayed) responds identically with `200 {"ok":false}`. |

Every non-success outcome across these routes is deliberately generic: an
unauthorized login attempt, a wrong upstream user, a state mismatch, and an
unknown session all render the same response, so a probing request cannot
learn which case applies or exfiltrate the allowlisted user ID.

### Effective scopes

A successful login is always granted `read:notes`, plus `read:account`
and/or `write:notes` if Aria requested them. `read:notes` is granted
unconditionally rather than intersected with the request: Aria's real,
source-traced permission list never actually requests a bare `read:notes`,
yet the compat doc fixes it as part of the effective set a successful login
grants — see `internal/miauth/scope.go`'s `effectiveScopes` doc comment for
the full reasoning. Scope enforcement is exact-match only
(`internal/httpserver.RequireScope`); Aria requesting a scope this service
does not implement never grants a capability.

### Session and gate lifetimes

Local and upstream MiAuth sessions expire after 10 minutes; the operator
bootstrap gate expires after 15 minutes. These are ADR-0001's fixed,
accepted design, not operator-configurable settings — the same treatment
this document's SQLite section gives `foreign_keys`/`journal_mode` below.

### Binding the owner

With `ALLOWED_MISSKEY_USER_ID` set, the first successful MiAuth login from
that upstream user ID binds them as the owner; no further action is needed.

With `ALLOWED_MISSKEY_USER_ID` unset, the deployment stays unbound until an
operator runs `cmd/bootstrapctl` against the same `DB_PATH`:

```sh
go run ./cmd/bootstrapctl
```

It refuses if an owner is already bound, otherwise prints a single-use URL
under `LOCAL_ORIGIN` valid for 15 minutes. The operator opens it, as the
owner, from the upstream Misskey account to bind. `cmd/bootstrapctl`
exposes no HTTP endpoint of its own — the printed URL is reachable only by
whoever can already run the command, which is what satisfies ADR-0001's
"shown only through the operator channel" requirement instead of a public
first-login-wins path.

### Deliberately out of scope for Issue #5

- Persisting the upstream Misskey token: `internal/provider/misskey.Client`
  uses it only within a single check call to read the verified user ID,
  then discards it. Encryption-at-rest wiring
  (`domain.UpstreamTokenRepository`) is deferred to whichever future issue
  first needs to read upstream data.
- Browser session cookies: security relies on the crypto/rand upstream
  `state` plus TTL and atomic consume, not a same-browser confirmation
  cookie.
- `POST /api/meta` and `POST /api/i`: assigned to Issue #7's minimal
  Aria/Misskey surface.
- Self-service profile editing (`POST /api/i/update`): `OWNER_USERNAME`/
  `OWNER_DISPLAY_NAME` are config-only for Issue #5; a fast-follow issue is
  expected to add an editable, database-backed profile so an operator does
  not need to edit config to change them.

## Note API

Issue #7 adds the minimal Aria/Misskey-compatible note surface
[`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md) specifies. This
section covers only the operational surface (routes, scopes, wiring); the
wire contract itself (request/response shapes, error codes, pagination and
visibility decisions) is normative there, in its "Issue #7 implementation
notes" section.

### Routes

| Route | Auth | Scope |
| --- | --- | --- |
| `POST /api/meta` | Anonymous | — |
| `POST /api/endpoints` | Anonymous | — |
| `POST /api/i` | `i` token | `read:account` |
| `POST /api/notes/create` | `i` token | `write:notes` |
| `POST /api/notes/timeline` | `i` token | `read:notes` |
| `POST /api/notes/show` | `i` token | `read:notes` |
| `POST /api/notes/conversation` | `i` token | `read:notes` |
| `POST /api/notes/children` | `i` token | `read:notes` |

These routes register only when `httpserver.Options.TimelineService` is
also set alongside `MiAuthService` (see `internal/httpserver.NewServer`);
`cmd/server` always wires both. `POST /api/notes/update` and the
WebSocket `/streaming` timeline channel are deliberately not implemented
(docs/compat/aria-v1.5.11.md classifies both **不要** for this MVP), so
`POST /api/endpoints` never advertises `notes/update`.

### Wiring

`internal/timeline.Service` is the use-case layer these handlers call
into; `cmd/server` constructs it from the same `*sqlite.DB` as
`internal/miauth.Service` (one `db.Repos`, two independent services, no
shared mutable state beyond the database itself).

## SQLite

`internal/storage/sqlite.Open` is the only place in this service that opens
the database. It applies three PRAGMAs through the connection DSN (as
`_foreign_keys`, `_busy_timeout`, and `_journal_mode` query parameters)
rather than as a one-time `PRAGMA` statement run once after `Open` returns:
`database/sql` can open more than one physical connection over the
program's lifetime (concurrent HTTP handlers, and eventually Issue #8's
worker), and a PRAGMA executed only against the first connection would
never reach any connection opened later. Embedding them in the DSN makes
the driver re-apply them to every connection it opens.

- `foreign_keys` is always on. This is a correctness requirement, not an
  operator-configurable setting.
- `journal_mode` is always `WAL`. This is also fixed rather than
  configurable: WAL is what allows concurrent readers alongside a single
  writer, which this service depends on once more than one goroutine
  touches the database.
- `busy_timeout` is the one operator-configurable PRAGMA
  (`DB_BUSY_TIMEOUT_MS`); it bounds how long SQLite retries internally on
  `SQLITE_BUSY` before returning an error to the caller.

At startup, `cmd/server` logs `"sqlite pragmas applied"` with the resolved
`foreign_keys`, `journal_mode`, and `busy_timeout` values.

### Migrations

Migrations live in `internal/storage/sqlite/migrations/*.sql`, embedded at
build time with `embed.FS`, and are applied forward-only in ascending
numeric-prefix order (`0001_actors.sql`, `0002_owner_binding.sql`, ...) by
`internal/storage/sqlite.DB.Migrate`, which `cmd/server` calls once at
startup, after `Open` and before serving traffic. Each migration runs in
its own transaction, so a failing statement rolls back cleanly instead of
partially applying.

**An applied migration file must never be edited.** This is enforced
mechanically, not just by convention: `Migrate` records a SHA-256
checksum of each migration's contents in the `schema_migrations` table
when it is first applied, and on every later startup it recomputes that
checksum from the embedded file and fails startup immediately if it no
longer matches. Add a new, higher-numbered migration file instead of
changing one that has already shipped.

## Health and readiness

- `GET /healthz` (liveness) always returns `200`. It reports only that the
  process is running, so an orchestrator should use it to decide whether
  to restart the process, never whether to route traffic to it.
- `GET /readyz` (readiness) returns `503` until the server has finished
  starting (`health.Registry.MarkReady`) and every registered
  `health.Checker` currently succeeds; it returns `200` otherwise. Startup
  marks the registry not-ready by default and only marks it ready once
  serving has started; shutdown marks it not-ready again before beginning
  the graceful drain, so a load balancer can stop sending new traffic
  before in-flight requests finish.

Issue #3 registered no `Checker`s (there was no dependency to check yet).
Issue #4 registers `internal/storage/sqlite.DB.Checker()` with
`Registry.Register` in `cmd/server`; `internal/health` and
`internal/httpserver` did not need to change. The checker runs `SELECT 1`,
a real round trip through the query engine, rather than `PRAGMA
integrity_check`: that check is a full-database scan, too expensive to run
on every `/readyz` poll.

## Graceful shutdown

`httpserver.Run` listens for `SIGINT`/`SIGTERM` (or an externally
cancelled `context.Context`), marks the registry not-ready, and calls
`http.Server.Shutdown` bounded by `HTTP_SHUTDOWN_GRACE_PERIOD`. If
in-flight requests do not finish within that window, it force-closes
remaining connections via `http.Server.Close` rather than hanging
indefinitely.

## Running in a container

See the README's [Container](../../README.md#container) section for the
`docker build`/`docker run` commands and volume/uid requirements. Two
configuration points specific to a container deployment:

- `.env` is never baked into the image (`.dockerignore` excludes it):
  configure a container deployment entirely through environment
  variables (`docker run -e`, a Compose `environment:` block, or your
  orchestrator's equivalent), never a mounted `.env` file.
- The "set but empty" startup error above (an unresolved
  `${VAR}` in a Compose file or an orchestrator's env-from-secret
  wiring) is a common way a container-based deployment trips this check;
  the fix is the same — unset the variable rather than passing it empty.
