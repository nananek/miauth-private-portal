# Configuration

This document covers the configuration and HTTP-routing foundation added by
Issue #3. It does not cover MiAuth, SQLite, the post API, or LLM
configuration; those are added by later issues and will extend this
document rather than replace it.

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
| `HTTP_WRITE_TIMEOUT` | no | `10s` | Same format/rules. |
| `HTTP_IDLE_TIMEOUT` | no | `60s` | Same format/rules. |
| `HTTP_MAX_BODY_BYTES` | no | `1048576` (1 MiB) | Enforced via `http.MaxBytesReader` on every request. |
| `HTTP_SHUTDOWN_GRACE_PERIOD` | no | `15s` | Bounds how long graceful shutdown waits before forcing connections closed. |
| `LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error`. Must not be `debug` in production. |
| `LOG_FORMAT` | no | `text` | `json` or `text`. Must be `json` in production. |

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
or body. This matters beyond MiAuth being out of scope for Issue #3:
ADR-0001 fixes Aria's `{session}` route value as a bearer
capability/correlation secret, so once Issue #5/#7 add that route, its
value must never reach a log line — a rule this middleware already
enforces today.

`Config.Redacted()` is the one place that decides which config fields are
safe to print; no config field added by Issue #3 is itself secret, but the
method exists so a future secret-bearing field (upstream Misskey tokens,
etc.) only needs to be excluded once.

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

Issue #3 registers no `Checker`s (there is no dependency to check yet).
Issue #4 registers a SQLite `Checker` with `Registry.Register`; no change
to `internal/health` or `internal/httpserver` is required to do so.

## Graceful shutdown

`httpserver.Run` listens for `SIGINT`/`SIGTERM` (or an externally
cancelled `context.Context`), marks the registry not-ready, and calls
`http.Server.Shutdown` bounded by `HTTP_SHUTDOWN_GRACE_PERIOD`. If
in-flight requests do not finish within that window, it force-closes
remaining connections via `http.Server.Close` rather than hanging
indefinitely.
