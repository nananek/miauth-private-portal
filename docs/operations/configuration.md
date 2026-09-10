# Configuration

This document covers the configuration and HTTP-routing foundation added by
Issue #3, the SQLite persistence layer added by Issue #4, the local MiAuth
authentication flow added by Issues #5 and #28, the Aria/Misskey-compatible
note API added by Issue #7, the durable job worker added by Issue #8, the
LLM reply/follow-up generation added by Issue #9, the LLM post
classification added by Issue #10, the RSS/Atom ingestion framework
added by Issue #11, the read-only IMAP mail ingestion added by
Issue #12, and the DB-backed runtime configuration overlay and
`miauthctl config` added by Issue #76. The normative design for MiAuth is
[`docs/decisions/0002-ssh-cli-auth.md`](../decisions/0002-ssh-cli-auth.md)
(ADR-0002), for IMAP/mailfetch process isolation is
[`docs/decisions/0003-imap-mailfetch-isolation.md`](../decisions/0003-imap-mailfetch-isolation.md)
(ADR-0003), for the runtime configuration overlay is
[`docs/decisions/0006-runtime-config-store.md`](../decisions/0006-runtime-config-store.md)
(ADR-0006), and [`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md)
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

This three-step resolution is what ADR-0006 calls a key's *bootstrap*
value. For a fixed subset of keys (the "db-eligible"/Tier A keys — see
[Runtime configuration overlay](#runtime-configuration-overlay-miauthctl-config)
below), a fourth, higher-priority source exists on top of these three: a
row in the `app_config` table, written by `miauthctl config set` or by
the automatic startup seed. Effective precedence for those keys is
**DB (when a row exists) > environment variable > config file >
default**; every other key is resolved exactly as above, with no DB
layer at all.

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
| `HTTP_MAX_BODY_BYTES` | no | `1048576` (1 MiB) | Enforced via `http.MaxBytesReader` on every request. Issue #77 PR3: the effective ceiling `cmd/server` actually wires (`httpserver.Options.MaxRequestBodyBytes`) is `max(HTTP_MAX_BODY_BYTES, DRIVE_MAX_FILE_BYTES + 64 KiB)`, since this one global wrap covers `POST /api/drive/files/create`'s multipart upload too — raising `DRIVE_MAX_FILE_BYTES` past 1 MiB - 64 KiB raises this effective ceiling automatically, without needing `HTTP_MAX_BODY_BYTES` itself changed. |
| `HTTP_SHUTDOWN_GRACE_PERIOD` | no | `15s` | Bounds how long graceful shutdown waits before forcing connections closed. |
| `LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error`. Must not be `debug` in production. |
| `LOG_FORMAT` | no | `text` | `json` or `text`. Must be `json` in production. |
| `DB_PATH` | no | `./data/portal.db` | SQLite database file path; its parent directory is created if missing. Must not be empty. |
| `DB_BUSY_TIMEOUT_MS` | no | `5000` | Positive integer milliseconds passed to SQLite's `busy_timeout` pragma. |
| `DB_MAX_OPEN_CONNS` | no | `8` | 1-100. Bounds the SQLite connection pool. |
| `LOCAL_ORIGIN` | yes | — | This service's public origin. Scheme+host only: no userinfo, path beyond `""`/`"/"`, query, or fragment. Must be `https` in production. |
| `ARIA_CLIENT_CALLBACKS` | no | `""` (reject any client callback) | Comma-separated exact-match allowlist of Aria's client return callbacks (for example `aria://aria/miauth`). Commas inside a callback path or query are retained; a separator is a comma followed by the next absolute URL scheme. A non-HTTPS scheme is explicitly allowed. |
| `OWNER_USERNAME` | no | `owner` | ASCII letters, digits, and underscores only. Reported as the owner's `UserDetailedNotMe.username`. Permanently config-only: neither Aria nor the pinned `misskey_dart` client has any way to send a username field to `POST /api/i/update` (see `docs/compat/aria-v1.5.11.md`'s "POST /api/i/update" section), so there is no self-service way to change this — edit the config and restart. |
| `OWNER_DISPLAY_NAME` | no | `""` (null) | Only the *initial* value copied into the database the first time the owner actor is created (approving the first MiAuth session). From then on, `POST /api/i/update` (Issue #23 PR1) is the source of truth for the owner's `UserDetailedNotMe.name` (nullable; empty means `null`), and this config value is no longer consulted — changing it after the owner already exists has no effect. |
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
| `RSS_FEED_URLS` | required if `RSS_ENABLED=true` | `""` | Comma-separated list of RSS/Atom feed URLs, seeded as `external_sources` rows (`kind="rss"`) at startup. Commas inside a URL's own path or query are retained (same splitting rule as `ARIA_CLIENT_CALLBACKS`); a separator is a comma followed by the next absolute URL. Each entry must be an absolute `http(s)` URL. Issue #77 PR4 (ADR-0008): each entry may carry an optional `\|<username>` suffix (for example `https://note.com/rss\|myuser`) naming that feed's own `ActorExternalSource` username — `\|` is never a valid unencoded URI character, so it unambiguously separates the URL from the suffix. The username must match Misskey's own character set (ASCII letters/digits/underscore); when omitted, one is derived from the feed's own host and disambiguated against every other username already registered for that host. Set only when a source is *first* registered — editing this suffix (or the feed's host) after the fact never changes an already-registered source's projected username/host; register under a new URL instead. |
| `RSS_POLL_INTERVAL` | no | `15m` | How often each configured feed is re-fetched. Positive duration, must exceed `RSS_FETCH_TIMEOUT`. |
| `RSS_FETCH_TIMEOUT` | no | `15s` | Bounds a single feed fetch's HTTP round trip. Positive duration, must be less than `RSS_POLL_INTERVAL`. |
| `RSS_MAX_RESPONSE_BYTES` | no | `2097152` (2 MiB) | Integer of at least 1. Bounds how much of a feed response is read into memory; a larger response fails the fetch. |
| `RSS_MAX_REDIRECTS` | no | `3` | 0-20. Maximum redirect hops a feed fetch follows; 0 disallows any redirect. |
| `RSS_SUMMARY_MAX_CHARS` | no | `4000` | 1-100000. Bounds each ingested item's normalized title/body length after HTML tags are stripped. |
| `RSS_ALLOW_INSECURE_HTTP` | no | `false` | When `false` (the default), any `http://` entry in `RSS_FEED_URLS` fails config validation — mirroring `LOCAL_ORIGIN`'s production `https` enforcement. Set `true` only for a trusted internal/test feed. |
| `RSS_FILTER_SCRIPT_PATH` | no | `""` | Issue #135: path to a Starlark (`.star`) script defining a top-level `matches(title, body, source_host, source_uri, provenance_url)` function; `True` excludes that item from the timeline. Empty (the default) means no filtering — every fetched item is kept, exactly pre-Issue #135 behavior. Read and compiled once at `cmd/server` startup (`internal/ingest/rss.LoadFilter`); a syntax error or a missing/mis-shaped `matches()` fails startup. Not stored in the database (see [ADR-0009](../decisions/0009-rss-item-filtering.md)) — edit the file and restart to change it. See "[RSS item filtering](#rss-item-filtering)" below. |
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
| `OPENWEBUI_ENABLED` | no | `false` | Gates Issue #52's Open WebUI registry and identity projection entirely. While `false`, no `openwebui_workspaces`/`openwebui_models` row is ever seeded, no actor projects as a remote user, and no other `OPENWEBUI_*` value is validated. |
| `OPENWEBUI_BASE_URL` | required if `OPENWEBUI_ENABLED=true` | `""` | The Open WebUI instance's origin: `https` only, in every environment (not only production), scheme+host only (no userinfo, path, query, or fragment — the same shape `LOCAL_ORIGIN` enforces, minus the `http` option). Must appear verbatim in `OPENWEBUI_ALLOWED_ORIGINS`. |
| `OPENWEBUI_ALLOWED_ORIGINS` | required if `OPENWEBUI_ENABLED=true` | `""` | Comma-separated fixed HTTPS origin allowlist (ADR-0005 D11); same splitting rule as `ARIA_CLIENT_CALLBACKS`/`RSS_FEED_URLS`. `OPENWEBUI_BASE_URL` must be an exact-match member. A Tailnet origin belongs here only if listed explicitly — nothing is inferred. |
| `OPENWEBUI_API_KEY` | required if `OPENWEBUI_ENABLED=true` | `""` | Bearer credential for `OPENWEBUI_BASE_URL` (ADR-0005 D10). Never logged or returned to a client; `Config.Redacted()` shows only whether it is set. The registry stores only this key's *name* (`secret_ref`), never its value. |
| `OPENWEBUI_WORKSPACE_NAME` | no | `Open WebUI` | The seeded workspace's display name. |
| `OPENWEBUI_DEFAULT_MODEL_ID` | required if `OPENWEBUI_ENABLED=true` | `""` | The provider's own opaque model id (ADR-0005 D9: a `GET /api/models` `data[].id`, or a workspace custom-model id from `GET /api/v1/models`). Trimmed of surrounding whitespace only — never case-folded, split, or otherwise reshaped; it is never regenerated from a display name. `Registry.Seed`'s fallback model row for this id; catalog sync (Issue #75) also expects to see it, and logs a warning when a sync round does not report it. |
| `OPENWEBUI_PRESENTATION_HOST` | required if `OPENWEBUI_ENABLED=true` | `""` | A bare lowercase DNS hostname (no scheme, port, path, or trailing dot) — a fixed, deployment-provisioned presentation value, never inferred from `OPENWEBUI_BASE_URL`. Must differ from `LOCAL_ORIGIN`'s host: a `UserLite` with a null host means "local to this service", so reusing the local host here would make a VirtualActor indistinguishable from a local actor. |
| `OPENWEBUI_CATALOG_SYNC_INTERVAL` | no | `10m` | Issue #75: how often `Registry.SyncCatalog` re-lists `GET /api/models` and reconciles `openwebui_models` (`CatalogScheduler`). Also attempted once, bounded by `OPENWEBUI_TIMEOUT`, at startup — a failure there is logged and does not block the server from starting; it just means this run keeps whatever the last successful sync (or `Registry.Seed`'s own fallback model, on a deployment that has never synced) already produced until the next scheduled round. Independent of `OPENWEBUI_GENERATION_ENABLED`: catalog sync keeps the VirtualActor projection and search results current even when outbound generation is off. Must be a positive duration. |
| `OPENWEBUI_GENERATION_ENABLED` | no | `false` | Issue #53's outbound-generation gate; only meaningful when `OPENWEBUI_ENABLED=true`, ignored otherwise (parsed and defaulted regardless, the same "sub-flag" shape `LLM_CLASSIFICATION_ENABLED` has relative to `LLM_ENABLED`, but never validated or required while the parent flag is off). `Registry.Seed` reconciles it onto the enabled workspace's `generation_enabled` column on every run (both directions — turning it back off clears a previous `true`), but as of this issue nothing reads that column: there is no bridge yet to gate. |
| `OPENWEBUI_TIMEOUT` | no | `120s` | Issue #53's per-HTTP-call bound for the outbound adapter. Larger than `LLM_TIMEOUT`'s default because a buffered (non-streaming) chat completion can run considerably longer than an ordinary reply generation. Not yet consumed by anything. |
| `OPENWEBUI_MAX_RESPONSE_BYTES` | no | `4194304` (4 MiB) | Issue #53's response-size bound. Larger than `RSS_MAX_RESPONSE_BYTES`/`IMAP_MAX_MESSAGE_BYTES` because `GET /api/v1/chats/{id}` returns the whole chat, not one message. Minimum `65536`. Not yet consumed by anything. |
| `OPENWEBUI_MAX_REQUEST_BYTES` | no | `1048576` (1 MiB) | Issue #53's outbound request-size bound; exceeding it is meant to fail a turn closed rather than silently truncate the conversation context sent to the model. Not yet consumed by anything. |
| `OPENWEBUI_MAX_CONTEXT_MESSAGES` | no | `100` | Issue #53's bound on how many prior-turn messages (including the new one) a single request may carry, independent of `OPENWEBUI_MAX_REQUEST_BYTES` — a byte bound alone would let a thread of many short messages slip through uncapped. 1-1000. Not yet consumed by anything. |
| `OPENWEBUI_WEB_SEARCH_ENABLED` | no | unset | Issue #72's opt-in, made tri-state by Issue #75 AC#11 (ADR-0005 D21): unset (the default) resolves `features.web_search` per model, from that model's own most recently synced `GET /api/models` `info.meta.defaultFeatureIds`; `true`/`false` overrides every model uniformly regardless of its own default. Independent of, and never inferred from, any per-model web-search setting configured in the Open WebUI instance's own admin/web UI — Open WebUI does not apply a model's web-UI tool/web-search configuration to API-key-authenticated callers (only requests carrying a UI session id get that auto-injection; an API caller must ask explicitly); `defaultFeatureIds` is a separate value the same `GET /api/models` response already returns to any caller. The target Open WebUI instance must also have its own `web.search.enable` admin setting and an actual search backend configured — this key alone does not make web search work end to end. |
| `OPENWEBUI_VIEWER_BASE_URL` | no | unset | Issues #81+#84's one new key (ADR-0005 D23): a browser-reachable HTTPS origin for the *same* instance `OPENWEBUI_BASE_URL` names (they may differ — a tailnet hostname this server dials vs. one a browser resolves). When set, a generated reply's text gains an owner-only "view in Open WebUI" link (`<value>/c/<remote_chat_id>`) and this deployment starts requesting `background_tasks.title_generation` on each new chat's first turn, so a generated title can be shown too (subject to a **要実機確認** synchronous/asynchronous timing gap — see `docs/compat/openwebui-0.11.3.md`'s point (i) and ADR-0005 D23: a title that has not appeared yet by the time this adapter checks is simply not shown, never wrong). Unlike `OPENWEBUI_BASE_URL`, it is **not** required to appear in `OPENWEBUI_ALLOWED_ORIGINS` — this server never makes a request to it, so D11's SSRF policy does not apply; validation only checks its shape (HTTPS origin, no userinfo/path/query/fragment). Leaving it unset reproduces pre-#84 behavior exactly: no link, no title-generation request. |
| `OPENWEBUI_TOOL_TURN_TIMEOUT` | no | `10m` | Bounds a tool/web-search-carrying turn's own long-running budget in two places (ADR-0005 D27, D27 addendum/Issue #126): the initiating `POST /api/chat/completions` call itself (`Client.runTurn`'s native branch, since a real-instance capture found that call blocks until Open WebUI's native tool-call loop fully finishes, not "returns immediately") and, independently, the completion-polling loop after it (`Client.awaitTurnDone`) — each gets its own full `OPENWEBUI_TOOL_TURN_TIMEOUT` budget rather than sharing one, so a turn's worst-case total wall time is up to 2x this value, not this value. Kept separate from `OPENWEBUI_TIMEOUT` (which still bounds every other call, including a plain turn's completions POST) because Open WebUI's native tool-call loop can take several rounds, not one buffered call. Originally Issue #93's per-HTTP-call bound for the now-retired `Client.StreamTurn` (ADR-0005 D24); D27 (Issue #123) repointed the same config key at the polling loop that replaced it, keeping its default and its "how long a native tool-call loop may run" meaning. Must be a positive duration. `Client.runTurn` selects this path for any turn (first or continuation) whose own resolved `tool_ids`/`web_search` is non-empty — see "Native multi-round tool execution" below for the full mechanism. |
| `DRIVE_BACKEND` | no | `localdisk` | Issue #77 PR1 (ADR-0007): selects the `internal/drive.Storage` implementation, `localdisk` or `s3compat`. Unlike `RSS_ENABLED`/`OPENWEBUI_ENABLED` there is no separate feature-flag key — Drive has no "off" state, only a choice of backend — so every field below is validated on every startup, not gated behind an enable flag. A deployment picks exactly one backend for its whole lifetime; there is no per-file/per-request switch and no migration tooling between backends. Nothing reads through this configuration yet (no HTTP endpoint, job, or repository exists until PR3/PR4/PR5/PR6 build one). |
| `DRIVE_DATA_DIR` | no | `./data/drive` | The `localdisk` backend's root directory (`internal/drive.Local`). Must not be empty when `DRIVE_BACKEND=localdisk`; this package does not check it exists on disk — `internal/drive.Local` fails closed at first use (`Put`/`Get`/`Delete`) if it does not, the same "config validates shape, the consumer validates reachability" split `DB_PATH` already has. |
| `DRIVE_S3_ENDPOINT` | required if `DRIVE_BACKEND=s3compat` | `""` | The S3-compatible API's `host[:port]`, no scheme (`minio-go`'s own convention) — for example `minio.internal:9000`, or an AWS S3 regional endpoint. |
| `DRIVE_S3_BUCKET` | required if `DRIVE_BACKEND=s3compat` | `""` | The single bucket every Drive file is stored under. `internal/drive.S3` never creates or configures it; it must already exist. |
| `DRIVE_S3_ACCESS_KEY_ID` / `DRIVE_S3_SECRET_ACCESS_KEY` | required if `DRIVE_BACKEND=s3compat` | `""` | S3 credentials. Never logged or returned to a client; `Config.Redacted()` shows only whether each is set. Unlike `OPENWEBUI_API_KEY`, these are not stored via the `secret_ref` indirection (`internal/openwebui/registry.go`'s pattern of persisting only a configuration key's *name* to a database row) — this PR persists no Drive configuration to any database row for a `secret_ref` to name. A future PR that does add one should reuse that same indirection rather than storing a raw credential a second time. |
| `DRIVE_S3_USE_SSL` | no | `true` | Selects `https` (default) or `http` against `DRIVE_S3_ENDPOINT`. |
| `DRIVE_S3_REGION` | no | `""` | Passed to the S3 client when non-empty; most S3-compatible servers (MinIO included) do not require it. |
| `DRIVE_MAX_FILE_BYTES` | no | `10485760` (10 MiB) | Bounds any single uploaded file, image or not. Minimum `1`. Issue #77 PR5: also bounds an RSS source's favicon fetch (`internal/ingest/favicon.Fetch`'s `maxBytes`) — no separate favicon-specific size configuration key exists. |
| `DRIVE_MAX_IMAGE_WIDTH` / `DRIVE_MAX_IMAGE_HEIGHT` | no | `8000` | Bounds a raster image's decoded pixel dimensions (`internal/drive.ValidateImage`), independent of `DRIVE_MAX_FILE_BYTES` — a small but pathologically large-dimension image ("decompression bomb") is rejected by this check even when it fits comfortably under the byte-size bound. 1-100000. |
| `DRIVE_ORPHAN_GC_INTERVAL` | no | `24h` | Issue #77 PR7: how often `internal/drive.GCScheduler` enqueues an orphan-file GC sweep (`RunOrphanGC`), which deletes any object the configured `Storage` backend holds that no `files` row references — the safety net for a rare best-effort-cleanup failure in the upload/delete paths, not a correctness-critical process, hence the deliberately infrequent default. Positive duration. |
| `ADMIN_SESSION_TTL` | no | `12h` | Issue #136 Phase 2 (ADR-0010): how long a browser session issued by `POST /admin/login/finish` stays valid before `RequireAdminSession` rejects it and the Owner must log in again with a passkey. Positive duration. |

`LLM_BASE_URL`, `LLM_API_KEY`, and `LLM_TIMEOUT` are shared connection
settings: required (and bound-checked) whenever *either* `LLM_ENABLED` or
`LLM_CLASSIFICATION_ENABLED` is `true`, not duplicated per feature.

`internal/config.KnownKeys()` is the single source of truth this table is
generated from by hand; keep them in sync when a key is added or removed.

30 of the keys above (Issue #76's Tier A) can additionally be read and
changed at runtime, without a restart, via `miauthctl config` — see
[Runtime configuration overlay](#runtime-configuration-overlay-miauthctl-config)
below for the full list and how. Every other key remains env/config-file
only, either because it is a secret (never stored in the database —
ADR-0005 D10), a network destination or credential (ADR-0006's own
carve-out), or a value only ever read once before any live-reloadable
consumer exists.

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

## Runtime configuration overlay (`miauthctl config`)

Issue #76 (ADR-0006) adds a DB-backed overlay on top of the file/env/
default resolution above, for a fixed subset of keys, and a `miauthctl
config` subcommand to read and change it. The goal is narrow: change a
handful of frequently-tuned values (an RSS feed list, a poll interval, an
LLM model) without a restart, with an audit trail — not to move every
setting into the database.

### The three-way key classification

Every known key (`internal/config.ClassOf`, `internal/config/keyclass.go`
is the single source of truth) falls into exactly one class:

- **secret** — `LLM_API_KEY`, `OPENWEBUI_API_KEY`, `IMAP_USERNAME`,
  `IMAP_PASSWORD`. Never written to `app_config` at all (ADR-0005 D10):
  `miauthctl config set/import` reject these keys outright, so "cannot be
  stored" is a property of the schema, not a check every caller must
  remember.
- **db-eligible** (Tier A, 30 keys) — may have an `app_config` row that
  overrides the bootstrap value, reloaded live by the component that
  consumes it. Listed by component below.
- **bootstrap-only** — every other key: process topology, every network
  destination and credential, the five `*_ENABLED` subsystem-construction
  flags, and anything read only once before a live-reloadable consumer
  exists. Stays env/config-file-only exactly as before this issue.

The 30 db-eligible keys, grouped by the component that reloads them:

| Component | Keys |
| --- | --- |
| `internal/jobs.Manager` | `JOBS_POLL_INTERVAL`, `JOBS_CLAIM_BATCH_SIZE`, `JOBS_LEASE_DURATION`, `JOBS_LEASE_RENEW_MARGIN`, `JOBS_MAX_ATTEMPTS`, `JOBS_BACKOFF_BASE`, `JOBS_BACKOFF_MAX`, `JOBS_MAX_CONCURRENT`, `JOBS_SHUTDOWN_GRACE_PERIOD` |
| `internal/ingest.Scheduler` (RSS and IMAP share this type) | `RSS_POLL_INTERVAL`, `RSS_FEED_URLS`, `RSS_SUMMARY_MAX_CHARS`, `IMAP_POLL_INTERVAL` |
| IMAP fetch job handler (`internal/ingest/imap.Adapter`) | `IMAP_FETCH_TIMEOUT`, `IMAP_MAX_MESSAGE_BYTES`, `IMAP_SNIPPET_MAX_CHARS`, `IMAP_STORE_FULL_BODY`, `IMAP_FULL_BODY_MAX_CHARS` |
| `internal/llmreply.Service` / `internal/llmclassify.Service` | `LLM_MODEL`, `LLM_TIMEOUT`, `LLM_MAX_OUTPUT_TOKENS`, `LLM_THREAD_CONTEXT_MAX_MESSAGES`, `LLM_THREAD_CONTEXT_MAX_CHARS`, `LLM_CLASSIFICATION_MODEL`, `LLM_CLASSIFICATION_MAX_OUTPUT_TOKENS`, `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_MESSAGES`, `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_CHARS` |
| Open WebUI catalog scheduler / turn job | `OPENWEBUI_CATALOG_SYNC_INTERVAL`, `OPENWEBUI_WEB_SEARCH_ENABLED` |
| `internal/webadmin.Service` (`FinishLogin`'s session-cookie lifetime) | `ADMIN_SESSION_TTL` |

Notably absent, on purpose: `JOBS_WORKER_ID` (identifies this process's
own in-flight lease ownership — changing it mid-run would make an
existing lease's owner ambiguous), every network-destination/credential
key (`OPENWEBUI_BASE_URL`, `IMAP_HOST`, `LLM_BASE_URL`, ...: switching a
connection target at runtime without repeating the SSRF-allowlist
scrutiny `Validate` applies once at startup is out of scope), the five
`*_ENABLED` flags (toggling one would require dynamically starting and
stopping goroutines `cmd/server` only ever builds once — a materially
larger design problem than reloading a tunable, left for a future issue
if ever needed), and `OPENWEBUI_GENERATION_ENABLED` (already
independently live via the `openwebui_workspaces` row —
`Registry.Seed`/`SetGenerationEnabled` — outside this mechanism
entirely).

### `miauthctl config` subcommands

```sh
go run ./cmd/miauthctl config list [--json]
go run ./cmd/miauthctl config get [--json] <key>
go run ./cmd/miauthctl config set [--dry-run] <key> <value>
go run ./cmd/miauthctl config unset <key>
go run ./cmd/miauthctl config validate <key> <value>
go run ./cmd/miauthctl config validate --file <path>
go run ./cmd/miauthctl config export [--json]
go run ./cmd/miauthctl config import [--from-env] [--file <path>]
go run ./cmd/miauthctl config history [--json] <key>
go run ./cmd/miauthctl config rollback --to-version <N> <key>
```

`list`/`get`/`export` never print a secret's real value: a secret key can
never have an `app_config` row (see classification above), so its
displayed value is always `Config.Redacted()`'s existing `<set>`/`<unset>`
marker, the same one every other `miauthctl`/log output already uses.
`list`/`get` additionally show each key's *source* — `db`, `env`,
`file`, or `default` — so an operator can tell at a glance whether a
value came from the database overlay or the bootstrap resolution.

Every `set`/`unset`/`rollback` is attributed to the bound owner actor
(ADR-0002: whoever can SSH to the host and run the CLI) and recorded to
an append-only audit table (`app_config_audit`) in the same transaction
as the `app_config` write, so a change and its audit trail can never
disagree. `set` is optimistically locked on the row's current version: a
concurrent `set` racing another one fails with exit code 4 rather than
silently clobbering it (see [Exit codes](#exit-codes) below).

`config import`'s three mutually exclusive sources: no flag imports the
currently resolved bootstrap config (file+env+default merged — the same
values the automatic startup seed below would use); `--from-env` imports
only what is explicitly set in the process environment right now,
ignoring the config file and defaults; `--file <path>` imports a given
dotenv-style file directly, for carrying another host's settings over
when moving to a new one. In every mode, a key with an existing
`app_config` row is left untouched — import only fills in what is
missing.

#### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success. |
| `1` | Usage error, or another general failure (including: no owner actor is bound yet). |
| `2` | Validation error — an invalid value, or a key that is a secret or otherwise not db-eligible. |
| `3` | An unrecognized key, or (for `unset`/`rollback`) a key/version with nothing to act on. |
| `4` | A concurrent update: the row's version changed since it was last read. Re-read with `config get` and retry. |

### Migration from `.env`/environment to the database

Every server startup idempotently seeds every db-eligible key that has no
`app_config` row yet, from that startup's own resolved bootstrap value —
an existing row is never touched. `miauthctl config import` (above) is
the same operation, run manually.

**This has one consequence worth calling out explicitly, because it is
easy to be surprised by:** because a key gets an `app_config` row from
its very first startup — even when that value is empty (an unset/
disabled feature's own default) — and the DB overlay's precedence is
*DB-when-a-row-exists* over environment, **a key becomes "sticky" the
moment it is first seeded.** From then on, editing `.env` or the
process environment for that key has **no effect** until an operator
also runs `miauthctl config set` (to push the new value into the
database) or `miauthctl config unset` (to remove the DB row and fall
back to the bootstrap resolution again). This is the intended,
approved behavior of the automatic seed — not a bug — but it is the
single most likely point of operator confusion this feature introduces:
*"I changed `LLM_MODEL` in `.env` and restarted, and nothing changed"*
almost always means that key already has a stale `app_config` row.
`miauthctl config get <key>` shows `source: db` in exactly that
situation; `miauthctl config unset <key>` resolves it.

### RSS feed additions and removals take effect without a restart

`RSS_FEED_URLS` is db-eligible specifically because it is this issue's
own motivating example. `external_sources` gained an `active` column
(migration 0023) so this could be done safely: on every
`internal/ingest.Scheduler` tick, `ExternalSourceRepository.
ReconcileFromConfig` reconciles the *current* (DB-overlaid) feed list
against existing rows — a new URL is created (or reactivated if it was
previously removed), and a URL no longer listed is deactivated. An
inactive source **is never deleted**: this service's append-only-history
convention means everything it already fetched, and every timeline
`Entry` promoted from it, survives untouched — removing a feed only
stops future polling of it. IMAP's single mailbox source is reconciled
through the same method for consistency, but since `IMAP_HOST`/`PORT`/
`MAILBOX` stay bootstrap-only, its own reconciliation only ever runs once,
at startup.

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

#### Reflecting newly-added scopes onto existing tokens (`tokens reflect-scopes`)

A local API token's `scopes` are computed once, at issuance (`Check`).
Whenever a new scope is added to `internal/miauth.grantableScopes`, a token
issued before that change does not carry it, even though Aria's MiAuth
`permission` query already requested it — re-approving through `miauthctl
approve` is one way to pick it up (it issues a fresh token), and
`miauthctl tokens reflect-scopes` (Issue #133) is the other: it updates an
**already-issued** token in place, without a fresh Aria login.

```sh
go run ./cmd/miauthctl tokens reflect-scopes --token-id <token-id>
go run ./cmd/miauthctl tokens reflect-scopes --all
go run ./cmd/miauthctl tokens reflect-scopes --token-id <token-id> --dry-run
```

`--token-id <id>` reflects one token; `--all` reflects every currently
non-revoked token (exactly one of the two is required). `--dry-run` reports
what would change without writing anything. Each reflect recomputes the
token's effective scopes from its originating MiAuth session's stored
requested permissions against the *current* `grantableScopes`, and only ever
**adds** scopes the stored value is missing — it never removes a scope a
token already has, even a hypothetical one a future `grantableScopes` change
might no longer grant, so running it is always safe. Every actual change is
recorded in an audit trail (`api_token_scope_audit`, mirroring
`app_config_audit`) with the token id, before/after scopes, timestamp, and
operator (the bound Owner actor, per ADR-0002); a reflect that finds nothing
to add writes no audit row.

A token fails to reflect — with a clear per-token error, never a silent
no-op — if it is revoked, or if its originating MiAuth session can no longer
be found (nothing in this codebase ever deletes a `miauth_local_sessions`
row, so this only happens if one was removed manually, outside the app); the
remediation in both cases is to issue a fresh token via `miauthctl approve`
instead. `--all` reflects every non-revoked token independently — one
token's failure does not block the others — and exits non-zero if any
token failed, listing which ones.

When adding a new scope to `grantableScopes`, existing tokens do not gain it
automatically: run `miauthctl tokens reflect-scopes --all` after deploying
(or document why not).

### Admin Web UI: bootstrap, registration, login, and session/token administration (Phase 3)

Issue #136 (ADR-0010) adds a browser-based admin surface, anchored to the
same SSH/host-access trust point as everything else in this section. Phase
1 covers bootstrapping a passkey for the Owner (no session cookie, no
admin screen). Phase 2 adds logging in with that passkey, the resulting
session, logout, and CSRF protection. Phase 3 (this document's current
state) replaces the Phase 2 placeholder `GET /admin/` with a real
dashboard listing pending MiAuth sessions and API tokens, each with its
own approve/reject/revoke/reflect-scopes action — see ADR-0010 for the
full four-phase design and why a web admin session is a structurally
distinct, fifth credential type rather than a repurposed
`api_tokens`/`miauth_local_sessions` row.

```sh
go run ./cmd/miauthctl web-login issue
```

This prints a single-use setup URL
(`<LOCAL_ORIGIN>/admin/setup?token=<raw>`), valid for 10 minutes. Opening it
in a browser and completing the WebAuthn prompt registers one passkey for
the Owner; the token is consumed atomically with that registration, so a
second open of the same URL fails. The raw token is printed to stdout
exactly once and never logged, the same redaction rule this document's
other raw-secret CLI outputs (`tokens` output excluded, `config`'s
`Redacted()`) already follow.

Once a passkey is registered, `<LOCAL_ORIGIN>/admin/login` runs the
WebAuthn login ceremony (`POST /admin/login/begin` then
`POST /admin/login/finish`) and, on success, sets the `admin_session`
cookie (`Secure` when `LOCAL_ORIGIN` is `https://`, `HttpOnly`,
`SameSite=Strict`, `Path=/admin`) that `RequireAdminSession` checks on
every other `/admin/*` route, including `GET /admin/`. The session lasts
`ADMIN_SESSION_TTL` (default `12h`) and
ends early via the page's own logout button (`POST /admin/logout`,
guarded by a CSRF synchronizer token alongside `SameSite=Strict`) or an
operator revoking the credential that established it:

```sh
go run ./cmd/miauthctl web-login list-credentials
go run ./cmd/miauthctl web-login revoke-credential <id>
```

`revoke-credential` deletes that passkey registration and revokes every
currently-active session it established — recovery for a lost or stolen
device, not just blocking its future logins.

Once logged in, `GET /admin/` renders every pending MiAuth session and
every API token, read fresh from `internal/miauth.Service` on each
request (`ListPendingSessions`/`ListAPITokens` — the same methods
`miauthctl` already uses; this phase adds no new capability, only a
second, browser-based caller). Each row exposes the action(s)
`miauthctl` already has a CLI equivalent for, as a `POST` guarded by both
`RequireAdminSession` and the CSRF synchronizer token:

| Route | Effect |
| --- | --- |
| `POST /admin/sessions/approve` | `internal/miauth.Service.ApproveSession` |
| `POST /admin/sessions/reject` | `internal/miauth.Service.RejectSession` |
| `POST /admin/tokens/revoke` | `internal/miauth.Service.RevokeAPIToken` |
| `POST /admin/tokens/reflect-scopes` | `internal/miauth.Service.ReflectScopes` |

Every mutating action, once it succeeds, writes one row to
`web_admin_action_audit` (owner actor, which registered credential
performed it, the action, its target, and a before/after value where
meaningful) — a best-effort write in its own transaction immediately
after the underlying action commits, not the same transaction as that
action: `internal/miauth.Service`'s own methods are untouched by this
phase and own their own transaction boundaries, so a rare audit-write
failure never rolls back, and never fails the HTTP response for, an
action that already succeeded. `RequestedPermissions`/`ClientCallback`
(both Aria-supplied, untrusted) render on the dashboard through
`html/template`'s ordinary auto-escaping, the same as the CSRF token
above — never a `template.HTML`-style escape hatch.

### Deliberately out of scope

- Managing SSH access, host accounts, or operating-system audit policy.
- Browser session cookies for Aria's own MiAuth flow above; authorization
  there occurs through the host-local CLI (ADR-0002). Issue #136 Phase 2
  narrows this exclusion for the admin Web UI specifically (see
  ADR-0010's structurally distinct fifth credential type) — the two
  surfaces never share a session or cookie.
- The RSS feed admin screen: a separate, later phase (Phase 4) of Issue
  #136; `GET /admin/` as of Phase 3 covers MiAuth sessions and API tokens
  only.
- An audit-history viewer for `web_admin_action_audit`: the table and its
  index are built to support one cheaply later, but no UI or endpoint to
  browse it exists yet.
- `POST /api/meta`, `POST /api/i`, and `POST /api/i/update`: assigned to
  Issue #7's minimal Aria/Misskey surface and Issue #23 PR1's
  self-service display-name editing, respectively; see the Note API
  section below.
- Self-service **username** editing: permanently out of scope. Issue
  #23 PR1's source trace found no wire path for it in Aria or the
  pinned `misskey_dart` client at all (see `docs/compat/aria-v1.5.11.md`),
  so `OWNER_USERNAME` remains the only way to set it, and changing it
  requires a config edit and restart.

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
| `POST /api/i/update` | `i` token | `write:account` |
| `POST /api/notes/create` | `i` token | `write:notes` |
| `POST /api/notes/timeline` | `i` token | `read:notes` |
| `POST /api/notes/show` | `i` token | `read:notes` |
| `POST /api/notes/conversation` | `i` token | `read:notes` |
| `POST /api/notes/children` | `i` token | `read:notes` |
| `POST /api/users/show` | `i` token | `read:account` |
| `POST /api/users/notes` | `i` token | `read:notes` |

These routes register only when `httpserver.Options.TimelineService` is
also set alongside `MiAuthService` (see `internal/httpserver.NewServer`);
`cmd/server` always wires both. `POST /api/notes/update` is deliberately
not implemented (docs/compat/aria-v1.5.11.md classifies it **不要** for
this MVP), so `POST /api/endpoints` never advertises `notes/update`. The
WebSocket `/streaming` timeline channel is a separate case: it registers
whenever `MiAuthService` alone is set (it needs no timeline), completes
the handshake since Issue #41, and pushes a live `homeTimeline` note-create
event since Issue #95 — see docs/compat/aria-v1.5.11.md's "Streaming
decision" for why this remains a UX enhancement, not the correctness
source of truth (`httpserver.Options.StreamHub`, wired from
`cmd/server/main.go`, controls whether the push half is active; nil
disables it and leaves the handshake-only stub).

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

#### Table rebuilds (`-- migrate:rebuild`)

SQLite cannot change a table's `CHECK` or table-level `UNIQUE`
constraints, or drop a column covered by one, with `ALTER TABLE`. The
only way is SQLite's documented twelve-step rebuild: create the
replacement table, copy the rows across, drop the original, and rename.
That sequence cannot run on the ordinary path, because dropping a table
while foreign keys are enabled behaves like deleting every one of its
rows and so fails against any child table, and `PRAGMA foreign_keys` is
a no-op inside a transaction, so the migration file cannot turn them off
itself.

A migration that needs the rebuild declares it with the line

```sql
-- migrate:rebuild
```

in its leading comment block (only that block is scanned, so the marker
cannot be buried among the statements). `Migrate` then applies that one
migration on a dedicated connection: foreign keys off, the migration and
its `schema_migrations` row inside a single transaction, `PRAGMA
foreign_key_check` before committing, foreign keys back on afterwards.
The `foreign_key_check` step is what keeps "foreign keys are off" from
meaning "foreign keys are unenforced" — a rebuild that orphaned a child
row rolls back whole and is not recorded, so the next startup retries it
rather than proceeding on a corrupt schema.

Use the directive only when a rebuild is genuinely required; adding a
table or a nullable column never needs it. `0016_actors_virtual_model.sql`
is the first migration that does: it widened `actors.actor_type`'s
`CHECK` and replaced `UNIQUE(actor_type)` with a partial unique index
over the singleton types, so an Open WebUI model's VirtualActor row
(Issue #52) can exist alongside the owner, assistant, and system actors.
`0027_actors_external_source_type.sql` (Issue #77 PR4) widens the same
`CHECK` list again, for RSS-kind `external_sources` actors.
`0032_openwebui_links_stateless.sql` (Issue #93 PR2, ADR-0005 D24/D25)
is the first to rebuild a *different* table: it widened
`openwebui_conversation_links.state`'s `CHECK` list to admit
`'stateless'`, the terminal state a branch reached when its turn was
served through Issue #93's native multi-round tool-execution path
instead of a chat-managed one. ADR-0005 D27 (Issue #123) retired that
path — no code writes `'stateless'` any longer, and this migration is
not reverted (AGENTS.md: never edit an applied migration), so the value
stays permitted but unused, the same state `0016`'s and `0027`'s own
now-superseded `CHECK` values are already in.

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

Every `JOBS_*` key except `JOBS_WORKER_ID` is Issue #76 db-eligible:
`miauthctl config set` takes effect on the Manager's next poll
(`JOBS_POLL_INTERVAL` resets the poll ticker itself), with no restart —
see [Runtime configuration overlay](#runtime-configuration-overlay-miauthctl-config).
`JOBS_MAX_CONCURRENT` required replacing the worker's fixed-capacity
semaphore channel with an atomic counter, since a channel's capacity
cannot change after creation.

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

`LLM_MODEL`, `LLM_TIMEOUT`, `LLM_MAX_OUTPUT_TOKENS`, and
`LLM_THREAD_CONTEXT_MAX_MESSAGES`/`_MAX_CHARS` are Issue #76 db-eligible:
`Handle` resolves the live value once per job, and the same resolved
model is what both the outbound request and the recorded
`LLMGeneration.Model` use, so the two can never disagree — see
[Runtime configuration overlay](#runtime-configuration-overlay-miauthctl-config).
The equivalent `LLM_CLASSIFICATION_*` keys work the same way for post
classification below.

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

### Startup seeding and live reconciliation

While `RSS_ENABLED=true`, `cmd/server` seeds one `external_sources` row
(`kind="rss"`) per `RSS_FEED_URLS` entry in two steps, run in order —
**both at startup and again on every scheduler tick** (Issue #134;
before that fix, step 2 below ran once, at startup, only):

1. `ExternalSourceRepository.ReconcileFromConfig` (Issue #76 PR4a,
   replacing the old create-only `EnsureFromConfig` for RSS) creates a
   bare row for any URL not yet registered at all, reactivates a
   previously-deactivated source whose URL is back in `RSS_FEED_URLS`,
   and deactivates (`active=0`, never deleted) one no longer listed.
   This step knows nothing about actors, Drive, or HTTP — it is pure SQL
   reconciliation (`AGENTS.md`'s persistence-boundary rule) — so a row
   it just created has no `actor_id`/`username`/`host` yet.
2. `ensureRSSSourceActors` (Issue #77 PR4, rewritten as a self-healing
   pass by Issue #134) scans every currently active `kind="rss"` row and
   provisions a dedicated `ActorExternalSource` actor (ADR-0008) —
   `actor_id`/`username`/`host`, set together via
   `ExternalSourceRepository.SetActorIdentity` — for any row that does
   not have one yet, whether step 1 just created it or it was left
   actor-less by an older deployment (see "Backfill" below). A row that
   already has an actor is left completely untouched: `SetActorIdentity`
   only ever succeeds against a row whose `actor_id` is still `NULL`, so
   an already-registered source's identity is never recomputed.
   Immediately after each row's actor commits — outside that
   transaction, and never able to fail it — Issue #77 PR5's
   `fetchAndSetSourceFavicon` best-effort fetches that source's own
   `https://<host>/favicon.ico`, extracts its largest embedded PNG image
   (`internal/ingest/favicon.Fetch`/`ExtractPNGFromICO`; a legacy
   BMP-in-ICO favicon is not supported and is skipped like any other
   failure), stores it via `drive.Service.CreateSystemFile`
   (`source_favicon` purpose, no owner), and sets the actor's
   `avatar_file_id` to it. This fetch always uses `https`, bounded by
   `DRIVE_MAX_FILE_BYTES`, regardless of `RSS_ALLOW_INSECURE_HTTP`
   (which governs only the feed fetch itself); a missing, oversized, or
   undecodable favicon is logged and otherwise silently skipped — the
   source keeps its actor either way, which simply keeps no avatar.

Since `RSS_FEED_URLS` is one of Issue #76's db-eligible keys, and both
steps above now run on every scheduler tick (not just at startup):
**adding or removing a feed URL via `miauthctl config set/unset
RSS_FEED_URLS` takes effect on the scheduler's next tick, no restart
required** — see
[Runtime configuration overlay](#runtime-configuration-overlay-miauthctl-config)
above — and a **genuinely new** URL's `ActorExternalSource`/`username`/
`host`/favicon are provisioned within that very same tick, immediately,
with no restart needed either. Editing `.env`/the environment directly
and restarting always works too, exactly as before this issue.

**Backfill.** Because step 2 is a self-healing pass over every
actor-less active row rather than a diff against configured URLs, it
also fixes up any row a pre-Issue-#134 deployment left without an actor
(for example, a source that was created by a live `RSS_FEED_URLS`
change under the old code, which only ever ran step 2's predecessor
once, at startup). No separate migration, flag, or `miauthctl`/SQL step
is needed by the operator — this fix's first startup, or its first
scheduler tick if that comes first, provisions every such row
automatically. Already-ingested entries/items from such a source are
**not** retroactively re-attributed: `entries.author_actor_id` is set
once, at entry-creation time, and never re-resolved, so historical items
already showing the shared `system` actor stay that way permanently —
only entries ingested *after* the backfill runs pick up the source's
real actor.

### RSS item filtering

Issue #135, [ADR-0009](../decisions/0009-rss-item-filtering.md): setting
`RSS_FILTER_SCRIPT_PATH` to a `.star` file lets an operator exclude
individual articles from ever reaching the timeline — by title/body
keyword, by feed, or any combination — without touching
`RSS_FEED_URLS`' whole-feed granularity. The script must define exactly
one top-level function:

```python
def matches(title, body, source_host, source_uri, provenance_url):
    if "sponsored" in title.lower():
        return True
    if source_host == "note.com" and "spam" in body.lower():
        return True
    return False
```

`matches()` runs once per fetched item, right after `internal/ingest/
rss.parseFeed` normalizes it (so `title`/`body` are already
HTML-stripped and bounded by `RSS_SUMMARY_MAX_CHARS`) and before it ever
reaches `internal/timeline.Service.CreateExternalEntry` — an excluded
item gets no `external_items` row, no `EntryNews` entry, and is never
re-offered once the feed's ETag/cursor moves past it, the same
"silently skip, never replay" shape `RSS_ENABLED=false` already has.
`source_host`/`source_uri` let one global script express per-feed rules
as ordinary conditionals; `provenance_url` is `""` when the item carries
none.

Two distinct failure moments, two different defaults:

- **The script fails to load** (missing file, syntax error, no
  top-level `matches` function, wrong parameter count) — `cmd/server`
  fails to start, the same fail-closed posture every other startup-time
  config problem already gets.
- **The script fails while evaluating one item** (an execution-step
  bound exceeded, a type error triggered by unanticipated feed content,
  a non-bool return, ...) — that one item is **kept**, a warning is
  logged, and the rest of the batch proceeds normally. A filter
  *infrastructure* failure must never itself make an otherwise-legitimate
  article disappear; only a script's own deliberate `True` does.

`RSS_FILTER_SCRIPT_PATH` is bootstrap-only (not one of the [db-eligible
keys](#runtime-configuration-overlay-miauthctl-config) above): the
script is read and compiled exactly once, at startup, so editing it
requires a restart.

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
`internal/mailfetch` builds the block itself and never sets
`internal/ingest.FetchedItem.Title`, so `internal/ingest.Service.Handle`'s
`composeExternalBody` — which prepends a
`[<entry kind>[: <source display name>]] <title>` provenance header (and
the item's `ProvenanceURL` on its own line) only to an item whose `Title`
is non-empty, the RSS/Atom case — is a no-op for mail and never
double-prefixes it. Sender/subject/date are preserved because they are
already part of `Body` by the time it reaches `CreateExternalEntry`. See
"Adding a source adapter" below for the boundary this draws for a new
adapter, and `docs/compat/aria-v1.5.11.md` for the per-kind `Body` shapes.

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
- `IMAP_FETCH_TIMEOUT`, `IMAP_MAX_MESSAGE_BYTES`, `IMAP_SNIPPET_MAX_CHARS`,
  `IMAP_STORE_FULL_BODY`, and `IMAP_FULL_BODY_MAX_CHARS` are Issue #76
  db-eligible and reloaded fresh on every fetch by
  `internal/ingest/imap.Adapter`; `IMAP_HOST`/`PORT`/`TLS_MODE`/
  `USERNAME`/`PASSWORD`/`MAILBOX` stay bootstrap-only (they name a
  network destination or a credential) — see
  [Runtime configuration overlay](#runtime-configuration-overlay-miauthctl-config).

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

## Open WebUI bridge (registry and identity projection)

Issue #52 (OWUI-P) and Issue #53 (OWUI-B), `docs/roadmap/openwebui.md`.
Unlike RSS/IMAP, Open WebUI is an **outbound** provider this service
talks to, not an inbound source it ingests from — it has no `##
<Kind> ingestion` adapter-contract row in the table above, and it is not
wired through `cmd/server`'s ingestion scheduler at all. #52 built the
registry (one configured Open WebUI instance and model) and the identity
projection (that model presented to Aria as a VirtualActor); #53 built
the outbound chat bridge on top of it — sending an Aria message to Open
WebUI and turning its reply into a local entry — described in "Outbound
turn bridge (Issue #53)" below. Owner recovery tooling for a link that
gets stuck `ambiguous` — `internal/openwebui/recovery.go`'s owner-only
methods and the `cmd/openwebuictl` CLI built on them — is that section's
own closing note; see `docs/operations/runbook.md` for the operational
procedure.

### Feature flag and startup seeding

`OPENWEBUI_ENABLED` gates the feature entirely, the same safe-default
shape as `LLM_ENABLED`/`RSS_ENABLED`/`IMAP_ENABLED`. While `false`, the
`openwebui_workspaces`/`openwebui_models` tables (added by migration
`0017`) stay empty and no actor ever projects as a VirtualActor.

When `true`, `cmd/server` constructs an `internal/openwebui.Registry` and
calls `Seed` once at startup, right after `EnsureReservedActors`. `Seed`
reconciles the configured workspace and model into the registry in one
transaction, idempotently:

1. Find or create the workspace by `OPENWEBUI_BASE_URL` (its unique key);
   update its name/presentation host/secret ref if it already exists.
2. Find or create the model by `OPENWEBUI_DEFAULT_MODEL_ID` within that
   workspace, minting its `actors` row (`actor_type='openwebui_model'`,
   migration `0016`) and a generated handle slug the first time it is
   seen (see below); reactivate it if a catalog sync had deactivated it.
3. Point the workspace's `default_model_id` at that model.
4. Disable every *other* workspace (and deactivate its models): this
   deployment supports exactly one enabled workspace, so changing
   `OPENWEBUI_BASE_URL` and restarting disables the instance left behind
   rather than leaving it enabled alongside the new one.
5. Enable the workspace.

As of Issue #75, `Seed` no longer deactivates any other model in the
workspace, and it no longer takes a display name or handle slug from
configuration — both are catalog-sync-owned presentation values now
(`internal/openwebui.Registry.SyncCatalog`, run at startup and on an
interval, reconciles every model `GET /api/models` reports for the
configured account). `Seed`'s own model row exists only as the
fallback `OPENWEBUI_DEFAULT_MODEL_ID` guarantees even before any catalog
sync has ever succeeded: a brand new such row gets a provisional display
name equal to its own opaque id, and a slug generated from that same id
(`internal/openwebui.GenerateActorSlug`) — which, per that function's own
stable-handle contract, is never recomputed again even once a later sync
learns the model's real name. Re-running `Seed` therefore never disturbs
the workspace id, a model id, or a model's `actor_id` — the roadmap's "a
model actor's stable local ID must survive display-name or handle
changes" requirement — but it also never re-derives a model's display
name or slug once assigned; only a sync round (a new `Name` from the
provider) or an explicit owner rename does that.

Startup fails closed (the server does not start) if `Seed` returns an
error, which includes every `OPENWEBUI_*` validation `Config.Validate`
already performs before `Seed` ever runs, plus
`internal/openwebui.ErrInvalidSecretRef` if the stored `secret_ref` is
ever anything other than the one credential key this service knows how
to resolve (`OPENWEBUI_API_KEY`).

### Owner-only

There is no HTTP endpoint or CLI for changing a workspace or a model.
Configuration is the only interface, in exactly the sense ADR-0002 means
"owner-only": only someone with host access to edit the config file and
restart the process can change it — the same principle
`OWNER_USERNAME`/`OWNER_DISPLAY_NAME` and RSS's `RSS_FEED_URLS` startup
seeding already rely on. `internal/openwebui.Registry`'s few change
methods used by later issues (`SetGenerationEnabled`,
`SetCapabilityStatus`, `RenameModel`) additionally re-check their caller
against the database (`actor.IsLoginable()`), returning `ErrNotOwner`
otherwise — a second, structural lock behind the "no endpoint exists"
lock, for whenever a future issue does add one.

### VirtualActor

The seeded model is presented to Aria as `@<slug>@<presentation
host>` — a `userLite` with a non-null `host`, the one exception to "host
is always null" now noted on that type's own doc comment
(`internal/httpserver/noteapi_wire.go`). This is a fixed presentation
value, not federation: the host is never discovered, resolved, or
delivered to (AGENTS.md's "no federation" is unchanged). Structurally, a
VirtualActor:

- is **never login-capable**: `domain.Actor.IsLoginable()`/`CanMiAuth()`
  are `false` for `actor_type='openwebui_model'`, and `internal/miauth`
  only ever binds a session or issues a token to the owner actor
  (`docs/operations/security-regression.md`'s VirtualActor-exclusion
  tests are the mechanical evidence).
- **cannot own a credential**: `domain.Actor.CanOwnSecret()` is `false`
  for every actor type — `OPENWEBUI_API_KEY` lives in configuration, and
  the database stores only its key name (ADR-0005 D10).
- **can author a reply**: `internal/timeline.Service.
  CreateGeneratedReplyBy` accepts either the assistant actor or an
  active Open WebUI model actor whose workspace is enabled as an entry's
  author. `internal/openwebui.TurnJob` (Issue #53) is its caller: it
  completes a successful turn the same atomic way `CreateGeneratedReply`
  completes an `LLMGeneration`, and — unlike that method's own default —
  its `complete` hook does record a "reply" notification (never a
  self-mention, since a generated reply is never the owner's own post);
  see "Outbound turn bridge (Issue #53)" below for exactly when.
- **falls back cleanly when not resolvable**: if the model is
  deactivated or its workspace disabled after an entry already exists,
  `resolveUserLite` falls back to the same actor-ID-as-username
  projection any other unresolvable author gets, rather than presenting
  a stale or partially-filled remote identity.
- **its avatar is set only through the CLI, never a wire endpoint**
  (Issue #77 PR7): a model is never a Drive API caller (it cannot
  authenticate at all — see the login-capability bullet above), so it
  has no `POST /api/i/update`-shaped path to its own `avatar_file_id`.
  `go run ./cmd/openwebuictl avatar-set <model-slug> <local-image-path>`
  validates and stores the image exactly like a Drive upload
  (`internal/drive.ValidateImage`, `DRIVE_MAX_FILE_BYTES`/
  `DRIVE_MAX_IMAGE_WIDTH`/`DRIVE_MAX_IMAGE_HEIGHT`, whichever
  `DRIVE_BACKEND` this deployment already uses) and points the model's
  actor at it; `avatar-clear <model-slug>` removes it. `<model-slug>` is
  the same handle Aria's own `@<slug>@<presentation host>` projection
  above already shows the owner.

### Conversation-link state machine (for Issue #53)

Migration `0018` and `internal/domain`'s `LinkState`/`LinkTransition`
already exist so #53 has a schema and a tested state machine to build
its bridge on; #52 stores no row that reaches any state but `unlinked`
(no row at all — nothing ever calls `Claim` yet). The machine itself,
reproduced from `docs/roadmap/openwebui.md`:

```text
unlinked --claim--> creation_pending --confirmed--> ready
                         |                         |
                         |                         +--uncertain continuation--> ambiguous
                         +--definitive failure--> failed
                         +--uncertain/lost response or lease expiry--> ambiguous

ambiguous --owner confirms the same chat--> ready
ambiguous --owner abandons branch-------> dead
```

A `creation_pending` link's single claimed `StartChat` is the only
create call it may ever make (`OpenWebUIConversationLink.
AllowsInitialStartChat` checks the claiming job id); `ambiguous`/
`failed`/`dead` permit no automatic retry at all
(`AllowsAutoRetry() == false` unconditionally). See ADR-0005 for the
full provider-contract reasoning this machine encodes.

### Turn outcome columns and client bounds (for Issue #53)

Migration `0019` adds `openwebui_turn_links.failure_category`,
`prompt_tokens`, `completion_tokens`, `finish_reason`, `last_attempt_at`,
and `completed_at`, plus an `idx_openwebui_links_state` index, so #53 has
somewhere to record a turn's outcome and owner-facing recovery tooling can
list links by state without a table scan. `failure_category` is a local
classification (`internal/domain`'s `FailureCategory*` constants — for
example `auth_failed`, `contract_failed`, `request_too_large`), never
provider error text (ADR-0005 D6); there is deliberately no `CHECK`
pinning the set of values, the same choice already made for
`provider_status` and `jobs.job_type`. `OpenWebUITurnLinkRepository`
gained `BeginAttempt` (records an attempt starting, before the provider is
ever called, so a lease expiry or crash afterward is distinguishable from
a turn that never attempted anything) and `RecordOutcome` (writes a
terminal or non-terminal status together with the category/usage/finish-reason that
explains it); `OpenWebUIConversationLinkRepository` gained `List`
(filterable by state/thread, for the CLI) and `MarkReady` now refuses to
move a link already carrying a `remote_chat_id` onto a *different* one.

`OPENWEBUI_GENERATION_ENABLED`/`OPENWEBUI_TIMEOUT`/
`OPENWEBUI_MAX_RESPONSE_BYTES`/`OPENWEBUI_MAX_REQUEST_BYTES`/
`OPENWEBUI_MAX_CONTEXT_MESSAGES` (table above) are Issue #53's
generation gate and the outbound adapter's client-side bounds. Their
defaults are this service's own conservative starting points, not values
observed from a real Open WebUI deployment — the compat contract
(`docs/compat/openwebui-0.11.3.md`) leaves "rate limits, sizes, and
timeouts" as to-be-determined by Issue #50's live-instance testing.
`Registry.Seed` reconciles `OPENWEBUI_GENERATION_ENABLED` onto the
enabled workspace on every run; `cmd/server` reads all five to build the
outbound adapter, the bridge, and the durable job (below) — but only when
`OPENWEBUI_GENERATION_ENABLED` is `true`. While it is `false` (the safe
default, independent of `OPENWEBUI_ENABLED`), no
`internal/provider/openwebui.Client` is ever constructed, no
`notes/create` enqueue hook is wired, and the `"openwebui_turn"` job type
is never registered — the same shape `LLM_ENABLED` already has.

### Outbound turn bridge (Issue #53)

With `OPENWEBUI_ENABLED=true` and `OPENWEBUI_GENERATION_ENABLED=true`,
`cmd/server` additionally builds `internal/provider/openwebui.Client`
(the HTTP adapter against `OPENWEBUI_BASE_URL`), `internal/openwebui.
Bridge`, and `internal/openwebui.TurnJob`, wires the bridge's
`EnqueueTurn` into `httpserver.Options.OpenWebUIBridge`, and registers
the job under job type `"openwebui_turn"`.

**Trigger.** Every `user_post` the owner creates while an enabled,
generation-enabled workspace exists is a candidate — there is no
content-based policy like Issue #9's `DecideReply` — independent of
`LLM_ENABLED`: an operator running both features gets both jobs for the
same post; nothing here special-cases that combination. `EnqueueTurn`
runs as an `internal/timeline.EntryHook`, inside the same transaction
`POST /api/notes/create` already commits the post in, so a claimed
conversation link, a turn row, and the `"openwebui_turn"` job intent are
all durable before the response is sent, exactly like Issue #9/#10's
existing job-intent hooks — and, symmetrically, a request whose author,
workspace, model, or reply-tree path is not eligible enqueues nothing at
all, leaving the post itself unaffected either way (a local post's
success never depends on the provider being reachable).

**Model selection (ADR-0005 D19, Issue #75).** Which model a post routes
to is resolved from its body's `@mention`s before the branch rule below
ever runs:

| `@mention`s resolved to active models | routes to |
| --- | --- |
| none | the workspace's `OPENWEBUI_DEFAULT_MODEL_ID` model |
| exactly one | that model |
| an unknown slug, or an inactive model's slug | folds into "none" above — never an error |
| two or more distinct models | `ambiguous_model_selection`: no job is enqueued; a failed link and turn are recorded for owner-facing visibility only (`go run ./cmd/openwebuictl links --state=failed`) |

A bare `@slug` or a fully-qualified `@slug@<presentation host>` both match;
a mention naming a *different* presentation host is never a candidate.

**Branch rule (ADR-0005 D4, generalized by D19).** A reply continues the
thread's existing link only when its parent is exactly that link's
current head (the `assistant_entry_id` of its latest non-superseded
succeeded turn), no turn already replies to that same parent, *and* the
link is bound to the model selection above resolved. A thread root, a
reply to an earlier node, a second reply to the same head, a re-ask after
a failed turn, and — since Issue #75 — a reply whose resolved model
differs from its parent link's own model, all start a **new** link (and
therefore a new remote chat) instead — Open WebUI's own per-chat
sibling/fork features are never used, so one local branch always maps to
at most one remote chat, and one remote chat is always talking to exactly
one model.

**Path construction.** Before either enqueueing or actually sending a
turn, the reply chain from the new message back to the thread root is
walked and validated fail-closed: a hidden or archived node, an entry
kind this bridge does not project (`news`/`mail`/`system`), an
unresolvable author, a walk that cannot reach the root, or a path longer
than `OPENWEBUI_MAX_CONTEXT_MESSAGES` (including the new message) all
refuse the turn rather than silently skipping or truncating a node. At
enqueue time this means "no job is created"; at job-execution time
(state can have changed since enqueue) it means the turn fails closed —
see the table below. The sequence actually sent carries only each node's
role (`user` for the owner, `assistant` for a prior reply) and body text
— never a local entry id, Misskey metadata, or a system prompt.

**Single-flight (ADR-0005 D5).** `TurnJob` holds an in-process, per-
thread lock (this deployment runs one worker process) so at most one
remote turn per thread is ever in flight; a second turn for the same
thread waits for the first to finish rather than racing it. A future
multi-worker deployment would need to replace this with a database-backed
lease — a separate issue, not built here.

**Web search / tool use (Issues #72, #74, #75).** `OPENWEBUI_WEB_SEARCH_ENABLED`
sets `features.web_search` on outbound completions calls, tri-state since
Issue #75 AC#11 (ADR-0005 D21): an explicit `true`/`false` overrides every
model uniformly, while leaving the key unset defers per model to that
model's own most recently synced `GET /api/models`
`info.meta.defaultFeatureIds` (on only when it contains `"web_search"`,
resolved by `Registry.SyncCatalog` into `internal/openwebui.
FeatureDefaultCache` on the same catalog sync round that already reads the
response for the registry — no second provider call). Each model's own
`tool_ids` are resolved per model too — there is no config key for them as
of Issue #75: `Registry.SyncCatalog` reads each
active model's own `GET /api/models` `info.meta.toolIds` on every catalog
sync round (`OPENWEBUI_CATALOG_SYNC_INTERVAL` above), filters it fail-closed
against `GET /api/v1/tools/` (an id this credential cannot actually invoke
is dropped and logged, never sent as-is — the same rule Issue #74
originally applied to one model, now applied to every one), and caches the
result in memory (`internal/openwebui.ToolConfigCache`) for `TurnJob` to
read per turn by the turn's own model. A model discovered since the last
sync round, or resolved before any round has ever succeeded, sends no
`tool_ids` at all — the same safe default a stale or inaccessible id
already fell back to. Whichever flags end up set, any resulting tool call
(web search or otherwise, MCP-backed or built in) runs entirely inside the
target Open WebUI instance's own request-handling loop, the same buffered,
single-HTTP-call shape this bridge already relies on (ADR-0005 D3) — this
codebase never executes a tool, runs an MCP server, or interprets a tool
call itself. A tool's output (a web search result, an MCP response) reaches
the model's final answer the same way any other upstream text does, so it
is untrusted data by the same AGENTS.md rule that already applies to Open
WebUI's own reply content, feeds, and mail.

Making that buffered call actually complete additionally requires
`"params": {"function_calling": "legacy"}` on the same request (sent
automatically whenever `features` or `tool_ids` is set on that call, never
otherwise): Open WebUI's non-streaming response handler never processes a
native `tool_calls` response, so without this flag a turn whose model
decides to call a tool wedges the assistant message at `done:false`
forever, and `features.web_search` alone silently does nothing at all
(Issue #74; see ADR-0005 D17 for the mechanism and
`docs/compat/openwebui-0.11.3.md`'s Phase 0 record for the real-instance
evidence).

**Citation footnotes and chat title (Issues #81, #84).** When a tool call
or web search actually ran (the `function_calling=legacy` path above),
the completions response carries a top-level `sources[]` array;
`internal/provider/openwebui.normalizeSources` turns each entry into a
short `{kind, display_name, url, arguments}` record (never the raw,
potentially large `document[]` text a tool or web page returned — ADR-0005
D22) and `internal/httpserver`'s wire projection appends them to a
generated reply as `[1] ...`/`[2] ...` footnotes, in the same array order
the model's own `[n]` citation markers are assumed to follow — an
explicit, **要実機確認** assumption for more than one source; see
`docs/compat/openwebui-0.11.3.md`'s point (i) and ADR-0005 D22 for what a
wrong assumption would look like (a cosmetic footnote mismatch, never a
`document[]` leak). `OPENWEBUI_VIEWER_BASE_URL` (above) additionally gates
a generated chat title and an owner-facing viewer link on the same reply;
both are opt-in and off by default.

**Outcome and retry.** Every provider call this bridge makes is
classified into a `failure_category` (never provider error text — the
observed Open WebUI instance echoes an upstream credential verbatim into
its own error text, so `internal/provider/openwebui` discards it at the
source; see `docs/decisions/0005-openwebui-boundary.md` D6) and a
terminal or retryable outcome for the turn and its link, summarized here
(the full table, phase by phase, is `internal/openwebui/turnjob.go`'s own
doc comments):

| Situation | Turn | Link | Retried? |
| --- | --- | --- | --- |
| Chat creation: credential/request rejected | `failed` | `failed` | No — creation is never replayed |
| Chat creation: response lost or undecodable | `ambiguous` (`creation_lost`) | `ambiguous` | No — needs owner recovery |
| Continuation: credential/request rejected, or a schema-drift response | `failed` | unchanged (stays `ready`) | No |
| Continuation: the chat itself reports an error for this turn | `failed` on the last attempt | unchanged | Yes, same ids (compat: re-sending overwrites rather than duplicates), until the last attempt |
| Continuation: rate limit / server error / transport / timeout | `ambiguous` on the last attempt | `ambiguous` on the last attempt | Yes, via a fresh lookup first, until the last attempt |
| Success | `succeeded` | `ready`, current pointer advanced | — |

An `ambiguous` link accepts no further automatic attempt at all —
`internal/openwebui/recovery.go`'s five owner-only methods (`ListLinks`,
`DescribeLink`, `ConfirmLink`, `AbandonLink`, `FreezeLink`) and the
`cmd/openwebuictl` CLI built on them are the only way out; see
`docs/operations/runbook.md` for the operational procedure.

**Notification (ADR-0005 D6).** A succeeded turn's reply records a Note-
style "reply" notification, the same owner-facing shape Issue #9's
assistant replies get — but never a self-mention (a generated reply is
never the owner's own post). Every other outcome (`auth_failed`,
`ambiguous`, `contract_failed`, `failed`) records neither a notification
nor a mention, and creates no assistant entry at all: this MVP is
buffered-only, so there is no partial/placeholder reply to show either.

**Generation later disabled.** Turning `OPENWEBUI_GENERATION_ENABLED`
back off and restarting deregisters the `"openwebui_turn"` job handler;
any job already enqueued for a turn already in flight is left pending
rather than dropped, the same unregistered-job-type recovery path
`LLM_ENABLED` already relies on — `jobsctl` can list and requeue it once
generation is turned back on.

### Native multi-round tool execution (Issue #93, redesigned by Issue #123)

Issue #93 originally gave the adapter a second, independent turn method
(`Client.StreamTurn`, ADR-0005 D24) that sent a chat-less, `stream:true`
request and read Open WebUI's SSE passthrough directly, dispatched to
(ADR-0005 D26) instead of `StartChat` whenever a branch's first turn's
own resolved `tool_ids`/`web_search` was non-empty. **Issue #120 found
this could never actually run a tool**: Open WebUI's native tool-calling
loop only runs for a request that carries `chat_id` (`event_emitter`'s
own condition), which a chat-less request by definition never sends —
see `docs/compat/openwebui-0.11.3.md`'s "(h.1)" section for the real
captures behind this finding. **ADR-0005 D27 (Issue #123) retired
`StreamTurn` and this whole separate dispatch** rather than repairing it.

Tool/web-search-using turns now go through the *same* `StartChat`/
`ContinueTurn` path the "Outbound turn bridge" section above describes,
for both a branch's first turn and every continuation on it — the
former "first turn only" restriction is gone, since there is no longer a
separate mode a continuation could fail to switch onto. Inside
`Client.runTurn`, a turn whose resolved `tool_ids`/`web_search` is
non-empty sends `stream: true` and no `params` key at all (instead of
`stream: false` plus D17's `params.function_calling="legacy"`), so Open
WebUI's native tool-calling loop runs — potentially over several rounds
— against the real chat this path already creates/continues.

**The initiating `POST /api/chat/completions` call itself blocks until
that loop finishes (ADR-0005 D27 addendum, Issue #126).** D27's original
text left this UNVERIFIED, reasoning by analogy from a capture that used
neither `tool_ids` nor `features`; a real-instance capture of the exact
native combination found the call does not return early with a `null`
body the way that other capture did — it returns only once generation is
already done. `runTurn`'s native branch therefore bounds this one POST by
`OPENWEBUI_TOOL_TURN_TIMEOUT` instead of the ordinary, shorter
`OPENWEBUI_TIMEOUT` a plain turn's completions call still uses
(`internal/provider/openwebui.Client.post`'s `timeout` parameter).
Regardless, the response body itself is still always `null`
(`docs/compat/openwebui-0.11.3.md`'s "(h)"/"(k)" sections) — this adapter
never trusts the completions response for the result — so confirmation
is still `LookupTurnOutcome` (`GET /api/v1/chats/{id}`, the same read a
plain turn's single confirming call already performs), polled every
`nativeTurnPollInterval` (2s, a package constant, not configurable)
until the assistant message reports done or an error, bounded by its
own, independent `OPENWEBUI_TOOL_TURN_TIMEOUT` budget (`Client.
awaitTurnDone`) — not a budget shared with the initiating POST's. In
practice the assistant message is typically already done by the time
the initiating POST returns (the observed capture behind Issue #126), so
this poll loop usually resolves on its first tick; the two independent
budgets mean a turn's worst-case total wall time is up to 2x
`OPENWEBUI_TOOL_TURN_TIMEOUT`, accepted as a rare edge rather than
threading one shared deadline through both calls.

A poll that never resolves within that budget lands in the same
`CategoryAmbiguous`/`ambiguous` outcome any other chat-managed turn's
unconfirmed completion already does (ADR-0005 D6) — a real chat exists
for a later `GET` to resolve, so there is no longer a distinct
"cannot ever be recovered" state for this case the way D25 (now
withdrawn) required. `domain.LinkStateless` (migration `0032`) is removed from the Go layer
along with it: no turn is dispatched statelessly any longer, so no link
ever reaches that state going forward. The migration itself is not
reverted (never edit an applied migration), so `'stateless'` remains a
technically-permitted `CHECK` value nothing in Go names anymore —
`go run ./cmd/openwebuictl links --state=stateless` now rejects it as an
unrecognized filter, same as any other unknown string; a deployment that
somehow still has a row in that state from before this decision needs an
unfiltered `links` listing (or a direct query) to find it, not this
flag.

### Table rebuild note

Migration `0016` (widening `actors.actor_type` to admit
`openwebui_model`) is this repository's first migration to use the
`-- migrate:rebuild` directive documented under "Migrations" above; read
that subsection for what the directive does and why it was necessary
here specifically.

## Drive storage foundation (Issue #77 PR1)

`internal/drive` is the storage foundation for Issue #77's Misskey-compatible
Drive API, profile avatars, external-source favicons, and post attachments
(PR3-PR6). This PR (PR1) adds only the foundation — no HTTP endpoint, job, or
repository reads or writes through it yet.

- `Storage` (`internal/drive/storage.go`) is a narrow `Put`/`Get`/`Delete`
  interface over opaque byte blobs, keyed by a caller-assigned string. It
  knows nothing about HTTP, the Misskey wire format, or the `files` table's
  metadata — persistence and provider boundaries stay behind this narrow
  interface (AGENTS.md), matching how `internal/ingest/safehttp` stays
  ignorant of any specific feed adapter.
- Two implementations exist from this PR, selected once for a deployment's
  whole lifetime by `DRIVE_BACKEND` (see "Known configuration keys" above):
  `Local` (`internal/drive/localdisk.go`), storing each key as a file under
  `DRIVE_DATA_DIR` with intermediate directories created on demand and
  every key checked against `filepath.IsLocal` before touching the
  filesystem (rejecting a traversal attempt rather than silently
  renormalizing it into a different, still-safe path); and `S3`
  (`internal/drive/s3.go`), reaching an S3-compatible store (AWS S3 or a
  self-hosted MinIO) through `minio-go` — chosen over the full AWS SDK for
  Go v2 because this deployment targets "some S3-compatible object store,"
  not AWS-specific features (see ADR-0007).
- `ValidateImage` (`internal/drive/validate.go`) decodes an upload's
  claimed image structure with `image.DecodeConfig` against an allowlist
  (PNG, JPEG, WebP) — never trusting a client-declared `Content-Type` or
  file extension. SVG is rejected by this same fails-to-decode path: it is
  XML, not any of these formats' binary header, so no dedicated
  SVG-detection code exists or is needed (ADR-0007).
- The `files` table (migration `0024_files.sql`) holds one row per stored
  object's metadata (`purpose`, `mime`, `byte_size`, `sha256`,
  `storage_key`, optional `width`/`height`, optional `owner_actor_id`). No
  repository reads or writes it yet; PR3/PR4/PR5/PR6 add the use case that
  populates it, each through its own repository added when that use case
  exists, rather than this PR guessing at an interface nothing calls yet.
- See [ADR-0007](../decisions/0007-drive-storage-boundary.md) for the
  backend-selection, credential, and image-validation design decisions,
  and [`docs/roadmap/media-drive.md`](../roadmap/media-drive.md) for the
  full PR0-PR7 breakdown this foundation is the second step of (after
  PR0's Aria/misskey_dart contract investigation).

## Adding a source adapter

Issue #17's contract for every ingestion adapter after the first two. RSS
(above) and IMAP (above) are the two worked examples this section
generalizes from; read one of them alongside this checklist rather than
instead of it.

### Approval gate

**Do not add a provider-specific adapter until the tracker issue (#1) has
approved that specific adapter's value.** "The framework supports it" is
not a reason to add one: every adapter is a new outbound network
dependency, a new untrusted parser surface, and (usually) a new
credential to hold for a single-owner deployment. Once an adapter is
approved, add it as **one adapter per pull request**, so its safety
boundaries can be reviewed on their own.

### Adapter contract

Fill the last column in for the new adapter, in its own
`## <Kind> ingestion` section of this document (step 5 of "Wiring a new
kind into `cmd/server`" below). Every row is a decision the operator has
to be able to look up later.

| Property | `rss` (`internal/ingest/rss`) | `imap` (`internal/ingest/imap` + `cmd/mailfetch`) | New adapter |
| --- | --- | --- | --- |
| Source owner | Public feeds the operator lists in `RSS_FEED_URLS`. No account, no per-source identity. | The single owner's own mailbox (`IMAP_HOST`/`IMAP_USERNAME`); exactly one mailbox. | |
| API / format | HTTP `GET`; RSS 2.0 / Atom XML parsed with `encoding/xml`. | IMAP4rev1 `EXAMINE` + `UID FETCH` + `BODY.PEEK`; RFC 5322 / MIME, parsed in the separate `cmd/mailfetch` process (ADR-0003). | |
| Auth | None. Authenticated feeds are not supported; if one is ever added, its credential goes in a request header, never in a query string. | `LOGIN` with `IMAP_USERNAME`/`IMAP_PASSWORD` over TLS (`implicit` or `starttls`; no plaintext mode exists). The credential never enters a job payload — it lives in the adapter's config and travels only in the per-request socket payload. | |
| Rate limit | One fetch per source per `RSS_POLL_INTERVAL` (default `15m`), bounded by `RSS_FETCH_TIMEOUT` (default `15s`). No additional per-host limiter; each kind has its own `Scheduler`. | One fetch per `IMAP_POLL_INTERVAL` (default `5m`), bounded by `IMAP_FETCH_TIMEOUT` (default `30s`), at most 200 messages per fetch (a fixed internal bound). A rejected `LOGIN` is classified permanent, so a bad password costs one login attempt per poll rather than a `JOBS_MAX_ATTEMPTS`-deep retry burst each time; nothing disables the source, so the scheduler keeps polling every `IMAP_POLL_INTERVAL` until the credential is fixed. | |
| Retention | Indefinite; no automatic purge (a dedicated retention policy is its own future issue). | Same. | |
| Dedupe key | `external_id` from the item's `guid`/Atom `id`, falling back to its link, and to the item's own `dedupe_key` when it has neither (`external_id` is `UNIQUE` per source, so it can never be left empty). `dedupe_key` hashes the source ID with that identifier, or with title+body+date for an item that has no identifier at all. | `external_id` from the message's `Message-ID`, stable across a `UIDVALIDITY` reset; when absent, a hash of source+`UIDVALIDITY`+UID, stable only until the next reset. | |
| Sanitization | `internal/textsanitize.StripHTML`, bounded by `RSS_SUMMARY_MAX_CHARS`. | `text/plain` preferred, `text/html` run through the same `StripHTML`; bounded by `IMAP_SNIPPET_MAX_CHARS` / `IMAP_FULL_BODY_MAX_CHARS`. | |
| Failure classification | `classifyDoError` / `categorizeStatus` in `internal/ingest/rss/adapter.go`. | `classifyConnError` / `classifyFetchError` in `internal/mailfetch/fetch.go`. | |

### Safety boundaries

A new adapter's pull request has to satisfy all of these. They are the
properties the framework itself cannot enforce for you.

- **Outbound HTTP goes through `internal/ingest/safehttp.Client`.** Never
  construct a bare `http.Client`: the SSRF address policy, the redirect
  bound, and the `https`→`http` downgrade refusal all live there.
  `safehttp.Config.AllowIPForTesting` stays a Go-level constructor
  argument — never expose it as an `internal/config` key, or a production
  deployment could configure its way past the protection.
- **Bound every fetch in both time and size.** A `context.WithTimeout`
  around the round trip, and `safehttp.ReadLimited` (or an equivalent
  explicit octet bound, as `IMAP_MAX_MESSAGE_BYTES` is) on anything read
  into memory. Return failures as `ingest.NewFetchError(category, err)`
  so `Service` can tell retryable from permanent.
- **Never put credentials or raw server response text in an error
  message.** `internal/logging` redacts by attribute *key* against a
  fixed allowlist (see "Redaction" above) — it does not scan values, so a
  secret concatenated into a free-text message is not caught anywhere
  downstream. Classify the failure and return a *fixed* string, the way
  `classifyConnError` does for a rejected `LOGIN`
  ([`internal/mailfetch/fetch.go`](../../internal/mailfetch/fetch.go));
  [`TestFetch_WrongPasswordFails`](../../internal/mailfetch/fetch_test.go)
  pins it. When an adapter is split across processes, the fixed message
  is built on the side that holds the credential — the RPC client half
  passes through whatever it receives and does no redaction of its own.
- **Sanitize in the adapter, not in `Service`.** Reuse
  `internal/textsanitize.StripHTML` to reduce a fetched body to plain
  text before putting it in `ingest.FetchedItem.Body`.
  `internal/ingest.Service` never parses, sanitizes, or executes `Body`
  (pinned by
  [`TestHandle_TitlelessItem_BodyIsStoredVerbatimAsInertText`](../../internal/ingest/service_test.go)).
  Its one edit to `Body` is `composeExternalBody`, which prepends a
  `[<entry kind>[: <source display name>]] <title>` provenance header —
  and the item's `ProvenanceURL` on its own line, when it has one — to an
  item whose `Title` is non-empty, and is a no-op for an item that sets
  no `Title`. That header is composed from `Service`'s own fields, never
  from anything `Body` says, so apart from it whatever the adapter
  returns is what a reader sees.
- **Keep ingested content out of LLM prompts.** `Service` enqueues no
  `llm_generation`/`llm_classification` job for an ingested entry, and
  the new adapter must not add one. Should a future feature deliberately
  include such an entry as thread context, `internal/llmreply`'s
  `roleForKind` still confines every non-LLM entry kind to the `user`
  role (`internal/llmreply/promptbuilder_test.go`,
  `TestBuildMessages_NonUserPostContextBecomesUserRole`) — instruction-like
  text in a feed or a mail body is data, never a system instruction.
- **Isolate a large untrusted parser in its own process.** A non-HTTP
  protocol with a substantial attacker-influenced parsing surface (MIME
  was the first) follows ADR-0003: the parsing library lives in a
  separate binary reached over a Unix socket, never linked into
  `cmd/server`.

### Required tests

The framework-level guarantees are already pinned once, for every
adapter, in `internal/ingest`. **Do not re-test them per adapter**; do
read them, because they define what your adapter is allowed to assume.

| Guarantee | Where it is already pinned |
| --- | --- |
| Duplicate delivery creates no second entry | `TestHandle_DuplicateDeliveryDoesNotDuplicateEntries` (`internal/ingest/service_test.go`) |
| A mid-batch failure leaves the cursor unadvanced | `TestHandle_PartialBatchFailureKeepsCursorAndCommittedItems` (same file) |
| One source's failure does not affect another source | `TestHandle_FailingSourceDoesNotAffectAnotherSource` (same file) |
| An ingested body is stored inert and enqueues no job | `TestHandle_TitlelessItem_BodyIsStoredVerbatimAsInertText`, `TestHandle_ImapItem_NeverEnqueuesLLMJobs` (same file) |
| A crashed worker's in-flight job is recovered | `TestManagerRecoversExpiredLeaseAfterCrash` (`internal/jobs/manager_test.go`) |
| A restart or duplicate tick does not double-enqueue a poll | `TestScheduler_TickWithinSameWindowDoesNotDoubleEnqueue` (`internal/ingest/scheduler_test.go`) |
| The SSRF/redirect/downgrade address policy holds | `internal/ingest/safehttp/client_test.go` |

What the new adapter's own test file has to cover — model it on
[`internal/ingest/rss/adapter_test.go`](../../internal/ingest/rss/adapter_test.go)
or [`internal/ingest/imap/adapter_test.go`](../../internal/ingest/imap/adapter_test.go):

- A successful fetch returns the expected items **and** a cursor, and
  passing that cursor back changes what the next fetch requests.
- A "nothing new" response (HTTP 304 or the protocol's equivalent) sets
  `FetchResult.NotModified` without returning items.
- A malformed or unparseable stored cursor is treated as absent, not as
  an error — a cursor written by an older version must never wedge a
  source permanently.
- A malformed response returns `ingest.CategoryMalformed`; an oversized
  one, `CategoryTooLarge`; a timeout, `CategoryTimeout`; and, for an
  HTTP-based adapter, a blocked address returns `CategoryPolicy`.
- A rejected authentication returns a permanent category and a message
  containing neither the credential nor the server's own response text.

### Wiring a new kind into `cmd/server`

1. Add an `XxxConfig` to `internal/config` (an `XXX_ENABLED` gate
   defaulting to `false`, plus the adapter's own keys), list every one of
   its `Key…` constants in `knownKeyOrder` — the single list both
   `isKnownKey` and `KnownKeys()` read, so a key left out of it fails
   startup as an unknown key the moment it appears in a config file — and
   reflect every secret in `Config.Redacted()` as set/not-set, never as
   its value.
2. In `cmd/server/main.go`, add the new gate to the
   `if cfg.RSS.Enabled || cfg.IMAP.Enabled` condition that constructs
   `ingestSvc` — it is nil when every ingestion feature is off, so a
   deployment that enables only the new kind would otherwise panic on the
   first `RegisterAdapter`. Then, behind the new kind's own gate:
   construct the adapter, `ingestSvc.RegisterAdapter(adapter)`, build one
   `ingest.NewScheduler` for the kind, and run it in the goroutine block
   alongside `rssScheduler`/`imapScheduler`. There is deliberately no
   second `jobsManager.Register`: `ingest.JobType` is shared by every
   kind, `Service.Handle` dispatches on `source.Kind` itself, and a
   second registration would silently overwrite the first.
3. Seed the source rows with
   `db.ExternalSources.EnsureFromConfig(ctx, sources)`, which leaves an
   existing `(kind, uri)` pair — and its cursor and failure counters —
   untouched.
4. If the new kind's entries are not `EntryNews`, extend
   `entryKindForSourceKind` in `internal/ingest/service.go` (today only
   `"imap"` maps to `EntryMail`).
5. Add this document's `## <Kind> ingestion` section and the new keys'
   rows to "Known configuration keys" above.

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
