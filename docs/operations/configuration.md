# Configuration

This document covers the configuration and HTTP-routing foundation added by
Issue #3, the SQLite persistence layer added by Issue #4, the local MiAuth
authentication flow added by Issues #5 and #28, the Aria/Misskey-compatible
note API added by Issue #7, the durable job worker added by Issue #8, the
LLM reply/follow-up generation added by Issue #9, the LLM post
classification added by Issue #10, the RSS/Atom ingestion framework
added by Issue #11, and the read-only IMAP mail ingestion added by
Issue #12. The normative design for MiAuth is
[`docs/decisions/0002-ssh-cli-auth.md`](../decisions/0002-ssh-cli-auth.md)
(ADR-0002), for IMAP/mailfetch process isolation is
[`docs/decisions/0003-imap-mailfetch-isolation.md`](../decisions/0003-imap-mailfetch-isolation.md)
(ADR-0003), and [`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md)
documents Aria's wire contract; this document covers only the
operational surface (config keys, routes, the operator tool), not the
protocol design itself.

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
| `HTTP_WRITE_TIMEOUT` | no | `15s` | Same format/rules. |
| `HTTP_IDLE_TIMEOUT` | no | `60s` | Same format/rules. |
| `HTTP_MAX_BODY_BYTES` | no | `1048576` (1 MiB) | Enforced via `http.MaxBytesReader` on every request. |
| `HTTP_SHUTDOWN_GRACE_PERIOD` | no | `15s` | Bounds how long graceful shutdown waits before forcing connections closed. |
| `LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error`. Must not be `debug` in production. |
| `LOG_FORMAT` | no | `text` | `json` or `text`. Must be `json` in production. |
| `DB_PATH` | no | `./data/portal.db` | SQLite database file path; its parent directory is created if missing. Must not be empty. |
| `DB_BUSY_TIMEOUT_MS` | no | `5000` | Positive integer milliseconds passed to SQLite's `busy_timeout` pragma. |
| `DB_MAX_OPEN_CONNS` | no | `8` | 1-100. Bounds the SQLite connection pool. |
| `LOCAL_ORIGIN` | yes | — | This service's public origin. Scheme+host only: no userinfo, path beyond `""`/`"/"`, query, or fragment. Must be `https` in production. |
| `ARIA_CLIENT_CALLBACKS` | no | `""` (reject any client callback) | Comma-separated exact-match allowlist of Aria's client return callbacks (for example `aria://aria/miauth`). Commas inside a callback path or query are retained; a separator is a comma followed by the next absolute URL scheme. A non-HTTPS scheme is explicitly allowed. |
| `OWNER_USERNAME` | no | `owner` | ASCII letters, digits, and underscores only. Reported as the owner's `UserDetailedNotMe.username` until a later issue adds self-service profile editing. |
| `OWNER_DISPLAY_NAME` | no | `""` (null) | Reported as the owner's `UserDetailedNotMe.name` (nullable); empty means `null`. |
| `JOBS_WORKER_ID` | no | hostname + PID | Human-readable worker identity used in logs and as a lease-owner prefix. Each claim appends a random fencing value, so a reclaim never reuses the previous lease generation. Set a deployment-unique value when operational logs need one; an empty config-file value uses the generated default. |
| `JOBS_POLL_INTERVAL` | no | `1s` | How often an idle worker checks for due or lease-expired work. Positive duration. |
| `JOBS_CLAIM_BATCH_SIZE` | no | `10` | Maximum jobs claimed per poll, 1-100; available concurrency can reduce it further. |
| `JOBS_LEASE_DURATION` | no | `30s` | Initial and renewed lease duration. Positive and greater than `JOBS_LEASE_RENEW_MARGIN`. |
| `JOBS_LEASE_RENEW_MARGIN` | no | `10s` | Renewal margin; renewal runs after `lease duration - margin`. Must be positive and less than the lease duration. |
| `JOBS_MAX_ATTEMPTS` | no | `8` | Total handler executions before a retryable failure reaches `dead`, 1-100. |
| `JOBS_BACKOFF_BASE` | no | `1s` | Base retry delay. Positive and no greater than `JOBS_BACKOFF_MAX`. |
| `JOBS_BACKOFF_MAX` | no | `10m` | Upper bound for exponential retry delay. Positive. The worker applies fixed ±20% jitter within the base/max bounds. |
| `JOBS_MAX_CONCURRENT` | no | `4` | Maximum handlers running in this process, 1-64. |
| `JOBS_SHUTDOWN_GRACE_PERIOD` | no | `15s` | Time allowed for handlers to finish after shutdown starts. Remaining handlers are cancelled and immediately requeued. |
| `LLM_ENABLED` | no | `false` | Gates Issue #9's reply/follow-up generation entirely. While `false`, `notes/create` never evaluates the reply policy and no `llm_generation` job is ever enqueued or handler-registered, and no request ever reaches `LLM_BASE_URL`. |
| `LLM_BASE_URL` | required if `LLM_ENABLED=true` | `""` | OpenAI-compatible API base (for example `https://api.openai.com/v1`), trailing slash trimmed. A path is expected and allowed, unlike `LOCAL_ORIGIN`. Must be `https` in production. |
| `LLM_API_KEY` | no | `""` | Bearer credential sent to `LLM_BASE_URL`; omitted from the request entirely when empty (self-hosted providers that need no key). Never logged or returned to a client; `Config.Redacted()` shows only whether it is set. |
| `LLM_MODEL` | required if `LLM_ENABLED=true` | `""` | Model name passed to the provider and recorded as `LLMGeneration.Model`. |
| `LLM_TIMEOUT` | no | `30s` | Bounds every HTTP call this service makes to `LLM_BASE_URL`. |
| `LLM_MAX_OUTPUT_TOKENS` | no | `1024` | 1-32768. Upper bound on one generation's completion length. |
| `LLM_THREAD_CONTEXT_MAX_MESSAGES` | no | `20` | 1-500. Maximum prior thread entries included as generation context. |
| `LLM_THREAD_CONTEXT_MAX_CHARS` | no | `8000` | 1-200000. Maximum combined character length of included prior thread context. |
| `LLM_CLASSIFICATION_ENABLED` | no | `false` | Gates Issue #10's post classification entirely, independent of `LLM_ENABLED`: an operator can run one without the other. While `false`, `notes/create` never enqueues an `llm_classification` job. |
| `LLM_CLASSIFICATION_MODEL` | no | `""` (falls back to `LLM_MODEL`) | Model name passed to the provider for classification; empty reuses `LLM_MODEL`. Required (directly or via `LLM_MODEL`) when `LLM_CLASSIFICATION_ENABLED=true`. |
| `LLM_CLASSIFICATION_MAX_OUTPUT_TOKENS` | no | `1024` | 1-32768. Upper bound on one classification completion's length. |
| `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_MESSAGES` | no | `20` | 1-500. Maximum same-thread candidate entries offered as related-post candidates. Independent budget from `LLM_THREAD_CONTEXT_MAX_MESSAGES`. |
| `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_CHARS` | no | `8000` | 1-200000. Maximum combined character length of included candidate entries. Independent budget from `LLM_THREAD_CONTEXT_MAX_CHARS`. |
| `RSS_ENABLED` | no | `false` | Gates Issue #11's RSS/Atom ingestion entirely. While `false`, no `domain.ExternalSource` row is ever seeded from `RSS_FEED_URLS`, no adapter/scheduler is constructed, and no request ever reaches a configured feed URL. |
| `RSS_FEED_URLS` | required if `RSS_ENABLED=true` | `""` | Comma-separated list of RSS/Atom feed URLs, seeded as `external_sources` rows (`kind="rss"`) at startup. Commas inside a URL's own path or query are retained (same splitting rule as `ARIA_CLIENT_CALLBACKS`); a separator is a comma followed by the next absolute URL. Each entry must be an absolute `http(s)` URL. |
| `RSS_POLL_INTERVAL` | no | `15m` | How often each configured feed is re-fetched. Positive duration, must exceed `RSS_FETCH_TIMEOUT`. |
| `RSS_FETCH_TIMEOUT` | no | `15s` | Bounds a single feed fetch's HTTP round trip. Positive duration, must be less than `RSS_POLL_INTERVAL`. |
| `RSS_MAX_RESPONSE_BYTES` | no | `2097152` (2 MiB) | Integer of at least 1. Bounds how much of a feed response is read into memory; a larger response fails the fetch. |
| `RSS_MAX_REDIRECTS` | no | `3` | 0-20. Maximum redirect hops a feed fetch follows; 0 disallows any redirect. |
| `RSS_SUMMARY_MAX_CHARS` | no | `4000` | 1-100000. Bounds each ingested item's normalized title/body length after HTML tags are stripped. |
| `RSS_ALLOW_INSECURE_HTTP` | no | `false` | When `false` (the default), any `http://` entry in `RSS_FEED_URLS` fails config validation — mirroring `LOCAL_ORIGIN`'s production `https` enforcement. Set `true` only for a trusted internal/test feed. |
| `IMAP_ENABLED` | no | `false` | Gates Issue #12's IMAP mail ingestion entirely. While `false`, no `domain.ExternalSource` row is ever seeded, no adapter/scheduler is constructed, and `cmd/mailfetch`'s socket is never dialed. |
| `IMAP_HOST` | required if `IMAP_ENABLED=true` | `""` | The IMAP server's hostname. |
| `IMAP_PORT` | no | `993` | 1-65535. `993` is the conventional implicit-TLS port; `143` is conventional for `IMAP_TLS_MODE=starttls`. |
| `IMAP_TLS_MODE` | no (validated only if `IMAP_ENABLED=true`) | `implicit` | `implicit` (TLS from the first byte) or `starttls` (plaintext `CAPABILITY`/`STARTTLS` negotiation before `LOGIN`). There is deliberately no plaintext option: IMAP credentials must never cross the network unencrypted. |
| `IMAP_USERNAME` | required if `IMAP_ENABLED=true` | `""` | IMAP login username. Never logged; `Config.Redacted()` shows only whether it is set (it can be a personal email address). |
| `IMAP_PASSWORD` | required if `IMAP_ENABLED=true` | `""` | IMAP login password/app-password. Never logged; sent only in the per-request payload to `cmd/mailfetch` over `IMAP_MAILFETCH_SOCKET`, never as a command-line argument or `cmd/mailfetch`'s own environment variable (see ADR-0003). |
| `IMAP_MAILBOX` | no | `INBOX` | The mailbox `cmd/mailfetch` `EXAMINE`s (never `SELECT`s: this service never marks, moves, or deletes mail). |
| `IMAP_POLL_INTERVAL` | no | `5m` | How often the mailbox is re-fetched. Positive duration, must exceed `IMAP_FETCH_TIMEOUT`. |
| `IMAP_FETCH_TIMEOUT` | no | `30s` | Bounds a single fetch's IMAP round trip (connect through `LOGOUT`). Positive duration, must be less than `IMAP_POLL_INTERVAL`. |
| `IMAP_MAX_MESSAGE_BYTES` | no | `1048576` (1 MiB) | Integer of at least 1. Bounds how many octets of a single message's text body `cmd/mailfetch` reads (the `BODY.PEEK<0,N>` upper bound); a larger body is truncated at this limit, not rejected. |
| `IMAP_SNIPPET_MAX_CHARS` | no | `2000` | 1-100000. Bounds the plain-text snippet stored per message after HTML sanitization, unless `IMAP_STORE_FULL_BODY=true` raises the bound to `IMAP_FULL_BODY_MAX_CHARS`. |
| `IMAP_STORE_FULL_BODY` | no | `false` | When `false` (the default), only a bounded snippet (`IMAP_SNIPPET_MAX_CHARS`) is stored per message, not the full body. |
| `IMAP_FULL_BODY_MAX_CHARS` | no | `20000` | 1-1000000. Upper bound on a stored body's length when `IMAP_STORE_FULL_BODY=true`; ignored otherwise. |
| `IMAP_MAILFETCH_SOCKET` | no | `/run/mailfetch/mailfetch.sock` | Unix domain socket path `internal/ingest/imap` dials and `cmd/mailfetch` listens on (`MAILFETCH_SOCKET_PATH`, `cmd/mailfetch`'s own, separate environment variable — see below). Both default to the same path so a deployment that overrides neither still lines up. |

`LLM_BASE_URL`, `LLM_API_KEY`, and `LLM_TIMEOUT` are shared connection
settings: required (and bound-checked) whenever *either* `LLM_ENABLED` or
`LLM_CLASSIFICATION_ENABLED` is `true`, not duplicated per feature.

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
or body. ADR-0002 fixes Aria's `{session}` route value and local API tokens
as secrets that must never reach a log line;
Issue #5's MiAuth handlers rely on this pattern-only logging to satisfy
that, and additionally never construct a log attribute containing any of
those values in the first place (see
[`TestMiAuthFlow_NeverLogsSensitiveValues`](../../internal/httpserver/miauth_handlers_test.go)).

`Config.Redacted()` is the one place that decides which config fields are
safe to print. Authentication secrets are not configuration fields in the
SSH+CLI design.

## HTTP routing

Routing uses the standard library's `net/http.ServeMux` with Go 1.22+
method+path patterns (e.g. `GET /healthz`, and later `GET
/miauth/{session}`). This service's Misskey-compatible surface is a small,
mostly-static set of routes with at most one path parameter per route,
which `ServeMux` already expresses directly. AGENTS.md requires a concrete
reason for any new dependency, and none exists yet for a third-party
router; if a future issue's routing needs (regex constraints, richer
sub-router middleware composition, etc.) outgrow `ServeMux`, record that
decision in a new ADR at that time.

## MiAuth

Issue #28 replaces the upstream bridge with the local flow ADR-0002 designs. This section covers
only the operational surface; the protocol itself (state machines, owner
state transitions, and threat model) is normative in ADR-0002, and the exact wire
shapes Aria expects are normative in
[`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md).

### Routes

| Route | Purpose |
| --- | --- |
| `GET /miauth/{session}` | Aria's entry point. Starts (or idempotently resumes) a pending local session. Redirects immediately to an allowlisted client callback when supplied; otherwise shows a waiting page. |
| `POST /api/miauth/{session}/check` | Aria polls this to complete the flow. Every non-success outcome (pending, denied, expired, replayed) responds identically with `200 {"ok":false}`. |

Every non-success check outcome is deliberately generic. HTTP never approves
a session; approval requires host access and `miauthctl`.

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

### Session lifetime

Local MiAuth sessions expire after 10 minutes. This is ADR-0002's fixed,
accepted design, not an operator-configurable setting — the same treatment
this document's SQLite section gives `foreign_keys`/`journal_mode` below.

### Approving sessions and managing tokens

Connect to the server host through SSH and run `miauthctl` against the same
configuration and `DB_PATH` as the server:

```sh
go run ./cmd/miauthctl list
go run ./cmd/miauthctl approve <session-id>
```

`approve` shows the session ID, creation time and age, requested permissions,
and callback, then requires typing `yes`. Use `--yes` only in trusted
automation. `reject <session-id>` denies a pending request. `tokens` lists
issued local API tokens by ID without exposing their hashes or raw values;
`revoke <token-id>` revokes one. The first approved session creates the sole
Owner actor and later approvals reuse it.

### Deliberately out of scope

- Managing SSH access, host accounts, or operating-system audit policy.
- Browser session cookies; authorization occurs through the host-local CLI.
- `POST /api/meta` and `POST /api/i`: assigned to Issue #7's minimal
  Aria/Misskey surface.
- Self-service profile editing (`POST /api/i/update`): `OWNER_USERNAME`/
`OWNER_DISPLAY_NAME` remain config-only; a fast-follow issue is
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

### Contract testing

`contract/aria_client` (run via `make contract-test`) verifies this note API surface
against `misskey_dart`, the client library Aria itself uses — see
[README.md](../../README.md#contract-tests) and
`docs/compat/aria-v1.5.11.md`'s "Issue #7 implementation notes" for what
it covers. The script creates a pending local session and approves it with
`miauthctl approve --yes` before running the Dart suite.

## SQLite

`internal/storage/sqlite.Open` is the only place in this service that opens
the database. It applies three PRAGMAs through the connection DSN (as
`_foreign_keys`, `_busy_timeout`, and `_journal_mode` query parameters)
rather than as a one-time `PRAGMA` statement run once after `Open` returns:
`database/sql` can open more than one physical connection over the
program's lifetime (concurrent HTTP handlers and Issue #8's worker), and a
PRAGMA executed only against the first connection would
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

## Durable jobs

`cmd/server` starts the HTTP server and durable worker against the same
SQLite connection pool and cancellation context. Jobs use the existing
`jobs` table and migration `0006_jobs.sql`; Issue #8 adds no schema change.
The worker atomically claims due `pending` rows and expired `running` leases,
renews long-running leases, and limits claims to its available concurrency.
Each claim stores a fresh fencing value in `lease_owner`; completion and retry
transitions compare that exact value so an older handler cannot finalize a job
after a later claim, including a reclaim by the same logical worker.
This provides at-least-once execution and restart recovery. Handlers must make
their result writes idempotent because a process can stop after an external
side effect but before recording `succeeded`.

The terminal states deliberately distinguish two failure modes:

- `failed` means a handler classified the error as permanent and retrying
  would not help;
- `dead` means retryable failures exhausted `JOBS_MAX_ATTEMPTS`.

Automatic retries use exponential backoff with fixed ±20% jitter bounded by
`JOBS_BACKOFF_BASE` and `JOBS_BACKOFF_MAX`. An unregistered job type is treated
as retryable so rolling deployments do not permanently discard work created by
a newer producer. Payloads and raw errors are never logged; transition logs use
job IDs, types, attempt numbers, claim-time `queue_latency_ms`, and coarse error
categories. The detailed error remains in SQLite for host-local inspection.

Operators can inspect and recover jobs without adding an administrative HTTP
surface:

```sh
go run ./cmd/jobsctl list --state=dead --limit=50
go run ./cmd/jobsctl show <job-id>
go run ./cmd/jobsctl retry <job-id>
```

Only `dead` and `failed` rows can be manually requeued. Requeue preserves the
attempt count for auditability and clears the old lease and error. On forced
worker shutdown, the existing `Retry` transition requeues the cancelled job
immediately and increments its attempt. This intentionally reuses the one
durable retry path rather than introducing a shutdown-only state transition.
Worker liveness is not registered as a separate health checker: readiness
already verifies the shared SQLite dependency, while queue inactivity by
itself is not a reliable failure signal.

## LLM reply generation

Issue #9 generates versioned LLM replies and follow-up questions to a
post, asynchronously through the durable job worker above, so an LLM
outage, timeout, or malformed response never affects `notes/create`
itself: `POST /api/notes/create` always creates the post and returns
`200` first, and any generation failure is visible only in the
`llm_generations` table (`internal/domain.LLMGenerationRepository`), not
as an error on the create request.

### Enqueue decision

`handleNotesCreate` (`internal/httpserver`) decides synchronously, from
the new post's body alone, whether to enqueue an `llm_generation` job:
`internal/llmreply.DecideReply` applies a versioned v1 heuristic (see its
package comment) that returns at most one of `reply` (an explicit
request — a question mark, or a fixed table of Japanese/English trigger
phrases) or `follow_up_question` (a fixed table of "still working
through this" markers), never both. The heuristic's trigger lists are
hardcoded Go constants for v1, not configurable; a future issue would add
that if an operator actually needs to tune them. When `LLM_ENABLED` is
`false`, this decision is never evaluated and no job is ever enqueued.

The job intent is passed into `timeline.Service.CreateRoot`/`CreateReply`
so it commits in the same transaction as the entry it targets
(`Job.SourceEntryID` is set atomically to the new entry's ID), matching
this service's existing "commit the post and durable job intent
atomically" rule for every other job producer.

### Generation job

`internal/llmreply.Service.Handle`, registered under job type
`llm_generation` only while `LLM_ENABLED=true`, does the actual work once
the job worker claims it:

1. Derives a deterministic generation ID (`"llmgen:" + job.ID`) and
   inserts a `pending` `LLMGeneration` row. A conflict here means this
   exact job was already delivered before: a `complete`/`failed` row
   means the delivery is a duplicate to skip; a still-`pending` row means
   an earlier attempt crashed before finishing, and this attempt resumes
   it.
2. Builds the prompt: `internal/llmreply.BuildThreadContext` takes the
   target entry's thread, drops hidden/archived entries (a user's
   visibility choice extends to not feeding it back into a new reply),
   and bounds what remains by `LLM_THREAD_CONTEXT_MAX_MESSAGES` and
   `LLM_THREAD_CONTEXT_MAX_CHARS`. `BuildMessages` assembles the system
   prompt (a persona, an always-on qualified-language instruction, a
   kind-specific instruction, and — only when
   `internal/llmreply.isHighRisk` detects a legal/medical/financial topic
   in the target post — an additional disclaimer instruction) followed by
   the bounded context and the target post itself.
3. Calls `internal/provider/openai.Client.Complete` (an OpenAI-compatible
   `POST {LLM_BASE_URL}/chat/completions`), bounded by `LLM_TIMEOUT` and
   `LLM_MAX_OUTPUT_TOKENS`.
4. On success, `timeline.Service.CreateGeneratedReply` atomically creates
   the `llm_reply`/`llm_follow_up` entry and marks the generation
   `complete`, linked to it, in one transaction.
5. On failure, the error is classified into one of
   `internal/llmreply.Category`'s values (`auth`, `client_error`,
   `malformed_response`, and `content_refusal` are permanent; `timeout`,
   `rate_limit`, `server_error`, and `transport` are retryable). A
   permanent failure marks the generation `failed` immediately. A
   retryable failure marks it `failed` only once this is the job's last
   configured attempt (`JOBS_MAX_ATTEMPTS`); otherwise the generation
   stays `pending` and an ordinary job retry follows.

Every provider-classified error is logged only by its `Category`
constant; the request body, response body, and any upstream error text
(which can echo request content back) never reach a log line, matching
this service's existing "never log LLM prompts or response bodies" rule.

### Replying to a follow-up question

A generated `llm_follow_up` entry is an ordinary timeline entry: when the
user answers it with `replyId` set to that entry's ID, `notes/create`'s
existing `CreateReply` path handles it exactly like any other reply,
landing in the same thread. No separate answer-routing endpoint exists.

## Post classification

Issue #10 classifies and organizes each `user_post` (subject, field,
keywords, tags, summary, open questions, priority, notebook/review
candidacy, and same-thread related-post guesses) as a separate, versioned
result, asynchronously through the durable job worker, so an LLM outage,
timeout, or malformed response never affects `notes/create` itself or the
post's own body. Results live in `llm_classifications`
(`internal/domain.LLMClassificationRepository`) and are never merged into
an entry's user-authored `Body`; only `internal/llmreply`'s generated
`llm_reply`/`llm_follow_up` entries are excluded from classification —
every `user_post` is classified unconditionally, with no body-dependent
policy like `internal/llmreply.DecideReply`.

### Enqueue

`handleNotesCreate` (`internal/httpserver`) enqueues an
`llm_classification` job for every created `user_post` whenever
`LLM_CLASSIFICATION_ENABLED=true`, alongside (and independently of) any
`llm_generation` job the reply policy decides to enqueue —
`timeline.Service.CreateRoot`/`CreateReply` accept any number of jobs and
commit them all in the same transaction as the entry they target. Editing
a post (a future `notes/update` endpoint; not implemented by Issue #10)
would enqueue another `llm_classification` job for the same entry the
same way; the version scheme below already supports this without any
different code path.

### Classification job

`internal/llmclassify.Service.Handle`, registered under job type
`llm_classification` only while `LLM_CLASSIFICATION_ENABLED=true`:

1. Resolves the classification version this exact job owns
   (`ensureClassification`): it searches the target entry's existing
   classification versions for one already carrying this job's ID. Found
   and still `pending` → resume it (an earlier attempt crashed before
   finishing). Found and already `complete`/`failed` → this delivery is a
   duplicate, skip it entirely. Not found → insert the next version
   number as a new `pending` row. Unlike `internal/llmreply`'s
   deterministic generation ID, `llm_classifications.id` is
   auto-incrementing, so this job-ID lookup — not a database conflict —
   is what makes a duplicate delivery detectable no matter how long after
   the original attempt it arrives (for example, if the job's own
   "succeeded" state transition failed to persist after the classification
   itself had already committed).
2. Builds the prompt: `internal/llmclassify.BuildCandidates` takes the
   target entry's thread (both earlier and later entries, unlike reply
   generation's context), drops hidden/archived entries, and bounds what
   remains by `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_MESSAGES` and
   `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_CHARS`. `BuildMessages` uses a
   fixed system prompt that never contains post content — every candidate
   and the target's own body are carried only in `user`-role messages, so
   text embedded in a post can never be promoted to a system instruction.
3. Calls `internal/provider/openai.Client.CompleteForClassification` (the
   same underlying wire handling `internal/provider/openai.Client.Complete`
   uses for reply generation), bounded by `LLM_TIMEOUT` and
   `LLM_CLASSIFICATION_MAX_OUTPUT_TOKENS`, against the model resolved by
   `LLM_CLASSIFICATION_MODEL` (or `LLM_MODEL` if unset).
4. `internal/llmclassify.ParseAndNormalize` validates the structured JSON
   output: an unrecognized `priority` enum value is normalized to `null`;
   oversize `keywords`/`tags`/`openQuestions`/`learningTargets`/
   `relatedEntryIds` are truncated; oversize `subject`/`field`/`summary`
   strings are truncated; `confidence` is clamped to `[0, 1]`. Only a
   response that cannot be parsed as a JSON object at all fails the
   classification — every other defect above is repaired, not rejected.
   `validateRelatedIDs` separately drops self-references, duplicates, and
   any ID outside the actual candidate set (a hallucinated or cross-thread
   ID) from `relatedEntryIds`; an empty result after that filtering does
   not fail the classification.
5. On success, one transaction calls `Complete` (recording `summary`,
   the remaining fields as opaque `structured_output` JSON, and the
   materialized `priority`/`notebook_candidate`/`review_candidate`/
   `unresolved` columns), `AddTag`/`AddRelatedEntry` for each validated
   tag/related ID, and `Activate` (deactivating every other version for
   the entry — the schema's partial unique index guarantees at most one
   active version per entry).
6. On failure, the error is classified into one of
   `internal/llmclassify.Category`'s values (`auth`, `client_error`,
   `malformed_response`, and `content_refusal` are permanent; `timeout`,
   `rate_limit`, `server_error`, and `transport` are retryable — identical
   meaning to `internal/llmreply.Category`). A permanent failure marks the
   classification version `failed` immediately. A retryable failure marks
   it `failed` only once this is the job's last configured attempt
   (`JOBS_MAX_ATTEMPTS`); otherwise the version stays `pending` and an
   ordinary job retry follows.

Every provider-classified error is logged only by its `Category`
constant, matching `internal/llmreply`'s and this service's existing
"never log LLM prompts or response bodies" rule.

### Review/notebook/unresolved queries

`internal/domain.LLMClassificationRepository.ListReviewCandidates`,
`ListNotebookCandidates`, and `ListUnresolved` return the active
classifications flagged accordingly, most recently generated first. These
are Go API (use-case layer) only — no new Aria/Misskey-compatible HTTP
endpoint exists for them, since Aria has no equivalent concept; a future
UI or interface would need its own issue.

## RSS/Atom ingestion

Issue #11 adds `internal/ingest`, an extensible external-source ingestion
framework, and its first adapter, `internal/ingest/rss` (RSS 2.0 and Atom).
An adapter turns one external source's items into `EntryNews`/`EntryMail`
timeline root entries, with the same "a source outage never affects a user
post" isolation this service already gives LLM generation and
classification: a broken or unreachable feed only ever affects its own
durable jobs, never `notes/create`, another source, or the durable job
worker itself.

### Framework

- `internal/ingest.Adapter` is the narrow interface a source kind
  implements: `Kind()` names the `domain.ExternalSource.Kind` value it
  handles (`"rss"`), and `Fetch(ctx, source, cursor)` retrieves and
  normalizes that source's new items since `cursor` (an adapter-opaque
  string). Issue #12's IMAP adapter is expected to implement the same
  interface without this framework changing.
- `internal/ingest.Service` implements the single job type both today's
  RSS adapter and any future adapter share:
  `external_source_poll` (`internal/ingest.JobType`), registered only while
  `RSS_ENABLED=true`. `Service.Handle` looks up the claimed job's
  `domain.ExternalSource.Kind` in its adapter registry
  (`Service.RegisterAdapter`), so adding Issue #12 only needs one more
  `RegisterAdapter` call in `cmd/server`, never a second
  `jobsManager.Register`.
- `internal/ingest/safehttp.Client` is the shared SSRF-protected outbound
  HTTP client every future HTTP-based adapter is expected to use: it
  resolves each hop's host, rejects any address that is not a public
  unicast IP (loopback, private, link-local, unspecified, and multicast
  ranges are all refused, at the initial request and at every redirect
  hop — dialing the already-validated IP directly, not re-resolving the
  hostname, to close the DNS-rebinding TOCTOU window), enforces a fixed
  redirect-count bound, and rejects an in-flight `https`-to-`http` scheme
  downgrade unconditionally, even when `RSS_ALLOW_INSECURE_HTTP=true`. Its
  test-only IP-policy override (used so a test can target an
  `httptest.Server` on `127.0.0.1`) is a Go-level constructor argument,
  never an `internal/config` key: a production deployment can never
  configure its way into disabling this protection.

### Job

`internal/ingest.Service.Handle`, registered under job type
`external_source_poll` only while `RSS_ENABLED=true`:

1. Decodes the job payload (`{"sourceId": "..."}`) and looks up the
   `domain.ExternalSource` row. A malformed payload, a missing source, or
   a source whose `Kind` has no registered adapter all fail the job
   permanently — retrying cannot fix any of them.
2. Calls the registered adapter's `Fetch` with the source's stored
   `cursor` (nil on a source's first-ever fetch). A conditional-fetch
   "not modified" result (RSS/Atom: an HTTP 304) records fetch success
   with no cursor change and returns without touching any item.
3. For each fetched item, calls
   `internal/timeline.Service.CreateExternalEntry`, which atomically
   dedupes the item (by `external_sources`+`external_id`, and by a
   content-hash `dedupe_key` fallback) and, only when it is new, creates
   its `EntryNews`/`EntryMail` root entry and promotes the item to it in
   one transaction. A duplicate delivery (a retried job, or the same item
   reappearing in a later fetch) returns the existing, already-promoted
   entry instead of creating a second one.
4. The source's `cursor` only advances — via
   `domain.ExternalSourceRepository.RecordFetchSuccess` — after **every**
   item in the batch has been durably processed. If any single item's
   `CreateExternalEntry` call fails partway through a batch, the whole job
   fails with a plain (retryable) error and the cursor is left unchanged:
   a retry re-processes the same batch, and `CreateExternalEntry`'s own
   dedupe makes re-processing the items that already succeeded a no-op.
   This is the mechanical guarantee behind "a fetch failure never silently
   drops unprocessed items."
5. `RecordFetchFailure` records every fetch-level failure (last error
   category, incremented `consecutive_failures`) on the source row for
   operator observability, independent of and in addition to
   `internal/jobs`' own retry/backoff/dead-job bookkeeping, which this
   never influences.

Fetch failures are classified into one of `internal/ingest.Category`'s
values (`transport`, `timeout`, and `server_error` are retryable;
`too_large`, `client_error`, `malformed`, and `policy` — an SSRF/scheme/
redirect policy violation — are permanent), the same
retryable-vs-permanent shape `internal/llmreply.Category` and
`internal/llmclassify.Category` already use.

### Scheduler

`internal/ingest.Scheduler` is a small periodic ticker
`internal/jobs.Manager` itself has no equivalent of, since every other job
producer enqueues in reaction to a user action rather than on a fixed
interval. It re-lists configured sources and enqueues one
`external_source_poll` job per source every `RSS_POLL_INTERVAL`
(immediately on start, then on each tick), using an idempotency key
derived from the source ID and the poll interval truncated to a fixed
window, so a restart or a duplicate tick within the same window collides
on `Jobs.Enqueue` rather than double-enqueueing. `cmd/server` runs it
alongside the HTTP server and job worker under the same shutdown context
only while `RSS_ENABLED=true`.

### Startup seeding

While `RSS_ENABLED=true`, `cmd/server` seeds one `external_sources` row
(`kind="rss"`) per `RSS_FEED_URLS` entry at startup via
`ExternalSourceRepository.EnsureFromConfig`, which is idempotent: a
`(kind, uri)` pair already present is left untouched (including its
`display_name`, `cursor`, and failure-tracking fields), never modified or
re-seeded. **To add a source**, add its URL to `RSS_FEED_URLS` and restart
the server; the next startup seeds it and the scheduler begins polling it
on its next tick. There is no HTTP endpoint or CLI command for managing
sources — like `LLM_MODEL` and the other single-owner settings in this
document, source configuration is env/config-file only.

### Untrusted external content

Ingested item bodies are HTML-stripped to plain text before ever reaching
`Entry.Body` (`internal/textsanitize.StripHTML`, shared with Issue #12's
IMAP adapter below: tags and `<script>`/`<style>` element content are
discarded, entities are decoded via the standard library's `html`
package, matching AGENTS.md's "treat feeds...as untrusted data" and
"never execute...embedded instructions"). An ingested entry is never fed
into an LLM prompt by this framework: `EntryNews`/`EntryMail` entries are
a distinct generation source from `notes/create`'s `user_post` entries,
and neither `llm_generation` nor `llm_classification` jobs are ever
enqueued for them — the same "explicit configuration required" rule
AGENTS.md requires before external content reaches a prompt.

## IMAP mail ingestion

Issue #12 adds `internal/ingest/imap`, the second `internal/ingest.Adapter`
(kind `"imap"`), turning read-only IMAP mailbox polling into `EntryMail`
timeline root entries through the same framework RSS uses (see "RSS/Atom
ingestion" above): the same job type, the same
`internal/timeline.Service.CreateExternalEntry` dedupe/promotion, and the
same "a source outage only affects its own durable jobs, never
`notes/create`" isolation.

### Process isolation

Unlike RSS (plain XML over `encoding/xml`), parsing IMAP's `ENVELOPE`/
`BODYSTRUCTURE` responses and MIME bodies from an untrusted mail server is
a larger, more attacker-influenced surface. Per
[`docs/decisions/0003-imap-mailfetch-isolation.md`](../decisions/0003-imap-mailfetch-isolation.md)
(ADR-0003), that entire surface is isolated in a separate process,
`cmd/mailfetch`, never in `cmd/server`:

- `internal/ingest/imap.Adapter` (running inside `cmd/server`) is a thin
  RPC client: it builds a request from `IMAPConfig` plus the polled
  source's ID and cursor, sends it to `cmd/mailfetch` over
  `IMAP_MAILFETCH_SOCKET` (a Unix domain socket, one newline-delimited
  JSON request/response pair per fetch — see `internal/mailfetch/rpc`),
  and turns the response into `ingest.FetchedItem`s. It imports neither an
  IMAP nor a MIME library.
- `cmd/mailfetch` (built from `internal/mailfetch`) owns the actual IMAP
  connection and MIME parsing, using `github.com/emersion/go-imap` and
  `github.com/emersion/go-message`. It is a single, stateless process:
  IMAP credentials travel only in each request's payload over the socket,
  never as `cmd/mailfetch`'s own command-line argument or environment
  variable, so a process listing or `/proc` inspection on the mailfetch
  side never reveals them. Its only configuration is
  `MAILFETCH_SOCKET_PATH` (default `/run/mailfetch/mailfetch.sock`,
  matching `IMAP_MAILFETCH_SOCKET`'s own default) and, for logging,
  `MAILFETCH_LOG_LEVEL`/`MAILFETCH_LOG_FORMAT`.
- `cmd/mailfetch` unreachable (not started, socket missing, connection
  refused) classifies as a retryable transport failure on the
  `cmd/server` side, handled by the same job retry/backoff as a
  transient IMAP server outage; it never affects `notes/create`, RSS
  ingestion, or the durable job worker.

### Read-only guarantees

`cmd/mailfetch` `EXAMINE`s `IMAP_MAILBOX` (never `SELECT`s it), fetches
message bodies with `BODY.PEEK[...]<0,N>` (never `BODY[...]`, which would
implicitly mark a message `\Seen`), and never issues `STORE`, `UID
STORE`, `COPY`, `MOVE`, `EXPUNGE`, or `APPEND` — AGENTS.md's "IMAP is
read-only by default and must not mark, move, or delete mail" is
mechanical here, not just a convention: those command paths simply do not
exist in `internal/mailfetch`'s code. `IMAP_TLS_MODE` never accepts a
plaintext option either, for the same "credentials never cross the
network unencrypted" reason.

### Cursor and dedupe

The stored cursor (`domain.ExternalSource.Cursor`, opaque outside this
adapter) carries `{uidValidity, lastUid}`. A UID greater than `lastUid`
is fetched; if the mailbox's current `UIDVALIDITY` no longer matches the
cursor's (the mailbox was recreated or renumbered), fetching restarts
from UID 1. This does not create duplicate timeline entries: each
message's `ExternalID`/dedupe key is derived from its `Message-ID` header
(RFC 5322), which is unaffected by a `UIDVALIDITY` reset, so
`CreateExternalEntry`'s existing dedupe recognizes a re-fetched message as
the same item. A message with no `Message-ID` (rare, but permitted) falls
back to a hash of `UIDVALIDITY`+UID, which is only stable until the next
`UIDVALIDITY` change — a narrow, documented limitation for that case. A
single fetch call processes at most 200 messages, oldest first (a fixed
internal bound, not configurable): a mailbox with a large pre-existing
backlog drains gradually across successive polls rather than in one
unbounded batch.

### Body content and privacy

Each stored `Entry.Body` begins with a plain-text `From`/`Subject`/`Date`
header block, followed by a sanitized snippet of the message's text
body (`text/plain` preferred; `text/html` sanitized the same way RSS
bodies are, via `internal/textsanitize.StripHTML`, when no `text/plain`
part exists). This header-in-body placement is deliberate:
`internal/ingest.FetchedItem.Title` is written by the RSS adapter but
never actually read by `internal/ingest.Service.Handle` (only `Body`
reaches `CreateExternalEntry`), so folding sender/subject/date into
`Body` itself is the only way they are actually preserved.

- `IMAP_STORE_FULL_BODY=false` (the default) stores only a bounded
  snippet (`IMAP_SNIPPET_MAX_CHARS`); `true` raises the bound to
  `IMAP_FULL_BODY_MAX_CHARS`, still a bounded length, never truly
  unlimited.
- Attachments and non-text MIME parts are never fetched at all: BODYSTRUCTURE
  identifies the message's text part before any body octet is
  requested, and only that part is ever fetched.
- Message text (subject, body, any instruction-like text a sender wrote)
  is stored and displayed as inert data, matching AGENTS.md's "treat
  ...mail...as untrusted data" — it is never executed, and (like RSS's
  `EntryNews` entries) never automatically reaches an LLM prompt.
- There is currently no automatic retention/expiry for ingested mail
  entries (the same "no automatic deletion mechanism exists yet" state
  RSS's `EntryNews` entries are already in): retention is indefinite by
  default. An operator who needs to remove specific entries today does so
  directly against `DB_PATH`'s SQLite database; a dedicated retention
  policy would be its own future issue.
- `IMAP_PASSWORD` (and, effectively, `IMAP_USERNAME`, which can be a
  personal email address) are configuration-only secrets: `Config.Redacted()`
  shows only whether each is set, matching `LLM_API_KEY`'s treatment.
  Rotate them the same way as any other config-file/environment secret;
  Issue #13's release-gate secret-rotation runbook is expected to note
  this alongside the LLM/other credentials it already covers.

### Deploying `cmd/mailfetch`

- **Container**: `docker-compose.yml` (this repository's first) defines a
  `mailfetch` service, gated behind the `imap` Compose profile:

  ```sh
  docker compose --profile imap up -d --build
  ```

  A plain `docker compose up -d` (no profile) starts only `server`,
  matching `IMAP_ENABLED=false`'s default of never needing `cmd/mailfetch`
  running at all. `mailfetch` publishes no port, runs with a read-only
  root filesystem, and drops every Linux capability — it is reachable only
  through the `mailfetch-socket` volume it shares with `server`.
- **Bare host**: `make build` also produces `bin/mailfetch`. Run it as a
  second, independent process/systemd unit alongside `bin/server`,
  ideally under its own low-privilege OS user, with
  `MAILFETCH_SOCKET_PATH` pointing at a directory both it and `bin/server`
  (via `IMAP_MAILFETCH_SOCKET`) can reach.

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

`httpserver.Run` listens for `SIGINT`/`SIGTERM` (or an externally cancelled
`context.Context`), marks the registry not-ready, and calls
`http.Server.Shutdown` bounded by `HTTP_SHUTDOWN_GRACE_PERIOD`. In parallel,
the job worker stops claiming and waits up to `JOBS_SHUTDOWN_GRACE_PERIOD` for
handlers. It then cancels remaining handlers and durably requeues their jobs
before the shared database closes. If HTTP requests do not finish within their
window, the server force-closes remaining connections via `http.Server.Close`
rather than hanging indefinitely.

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
