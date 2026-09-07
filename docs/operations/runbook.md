# Operations runbook

Issue #13's release-gate acceptance criteria call for a documented,
single-owner operator runbook: starting and stopping the service,
incident response, secret rotation, token revocation, DB/file
permissions, reverse proxy/TLS, request rate/concurrency limits, and log
retention. This document is the primary source for those procedures; it
links to [configuration.md](configuration.md) and the
[README](../../README.md) for the underlying config keys and commands
rather than repeating them.

Everything here assumes the single-owner, permission-gated deployment
model this service is built for (see `AGENTS.md`'s "Product boundary"):
one operator with host/SSH access to the server, no multi-user
administration.

## Starting and stopping

**Bare host**: `make build` (or `make run`) produces `bin/server`; see the
README's [Getting started](../../README.md#getting-started) section for
the full startup sequence and the `/healthz`/`/readyz` checks. Stop it
with `SIGINT`/`SIGTERM` (or Ctrl-C in a foreground shell); the server
drains in-flight requests and durable-job handlers before exiting — see
[configuration.md](configuration.md#graceful-shutdown). There is no
separate "restart" command: stop, then start again the same way.

**Container**: see the README's [Container](../../README.md#container)
section for `docker build`/`docker run`, and
[configuration.md](configuration.md#deploying-cmdmailfetch)'s "Deploying
`cmd/mailfetch`" section if `IMAP_ENABLED=true`. `docker-compose.yml`
already sets `restart: unless-stopped` on both services, so a host
reboot or an unexpected process exit restarts them without operator
action; a deliberate stop is `docker compose down` (or `docker stop` for
a bare `docker run` deployment).

Before restarting after a config change, confirm the change with
`go run ./cmd/miauthctl list` (or any read command) rather than assuming
it took effect — a rejected config value fails startup immediately (see
[configuration.md](configuration.md#loading-order)), so a restart that
does not come back up almost always means the new config is invalid, not
that something else broke.

## Changing a runtime setting without a restart

29 configuration keys (RSS feed list and poll interval, job worker
tuning, LLM model/timeout/context bounds, Open WebUI catalog sync
interval and web search) can be changed live, without stopping the
process, via `miauthctl config` — see
[configuration.md](configuration.md#runtime-configuration-overlay-miauthctl-config)
for the full key list and how each one is reloaded. The everyday
sequence:

```sh
go run ./cmd/miauthctl config get RSS_POLL_INTERVAL          # check the current value and its source (db/env/file/default)
go run ./cmd/miauthctl config set RSS_POLL_INTERVAL 5m       # change it — no restart needed
go run ./cmd/miauthctl config history RSS_POLL_INTERVAL      # see who changed what, and when
go run ./cmd/miauthctl config unset RSS_POLL_INTERVAL        # revert to whatever .env/the environment/the default would give
go run ./cmd/miauthctl config rollback --to-version 2 RSS_POLL_INTERVAL  # or roll back to a specific prior value
```

**A setting `.env`/environment-variable change that "does not seem to
take effect" after a restart is the single most common surprise here.**
Every db-eligible key gets an `app_config` database row automatically on
this deployment's very first startup (even if that value is empty), and
the database, once a row exists, always wins over the environment. So
after that first startup, editing `.env` for one of these 29 keys and
restarting has **no effect** by itself — first check
`miauthctl config get <key>`: if `source: db`, either
`miauthctl config set <key> <new value>` (push the new value into the
database) or `miauthctl config unset <key>` (remove the DB row so
`.env`/the environment take over again) before restarting.

`miauthctl config set` refuses a secret key (`LLM_API_KEY`,
`OPENWEBUI_API_KEY`, `IMAP_USERNAME`, `IMAP_PASSWORD`) and a network-
destination/credential key (`OPENWEBUI_BASE_URL`, `IMAP_HOST`,
`LLM_BASE_URL`, ...) outright — those remain `.env`/environment-only and
still require a restart, exactly as before this feature; see "Secret
rotation" below.

## Incident response / troubleshooting

- **`/readyz` returns 503**: the process has either not finished startup
  yet, or `internal/storage/sqlite.DB.Checker()` (a `SELECT 1` round
  trip) is failing — almost always a `DB_PATH` the process cannot open or
  write to (permissions, missing/unwritable parent directory, or disk
  full). `/healthz` staying `200` while `/readyz` stays `503` past normal
  startup time confirms this is a readiness (dependency) problem, not a
  crashed process. See [configuration.md](configuration.md#health-and-readiness).
- **LLM provider outage or misconfiguration**: `POST /api/notes/create`
  is unaffected — replies/follow-ups and classification run
  asynchronously through the durable job worker, so a post is always
  created and returned first (see configuration.md's "LLM reply
  generation" and "Post classification" sections). Affected jobs
  accumulate as `failed`/`dead`, inspectable without any HTTP surface:

  ```sh
  go run ./cmd/jobsctl list --state=dead --limit=50
  go run ./cmd/jobsctl show <job-id>
  ```

  Once the provider is reachable again, jobs that are still `pending` (a
  retryable failure that had not yet exhausted `JOBS_MAX_ATTEMPTS`)
  resume automatically on their next scheduled retry — no operator action
  needed. Only `dead`/`failed` jobs need a manual
  `go run ./cmd/jobsctl retry <job-id>`.
- **Open WebUI outbound turn bridge (`OPENWEBUI_GENERATION_ENABLED=true`)
  left an `ambiguous` or stuck `creation_pending` conversation link**: the
  bridge never retries an uncertain outcome automatically (Issue #53's
  "an ambiguous outcome is resolved only by an explicit owner recovery
  action, never another automatic attempt") — an owner action is required.
  `go run ./cmd/jobsctl list --type=openwebui_turn --state=dead` (or
  `--state=failed`) finds the durable job side of this; the conversation
  link side, which is what actually needs resolving, is
  `cmd/openwebuictl`'s own concern and has no `jobsctl` equivalent:

  ```sh
  go run ./cmd/openwebuictl links --state=ambiguous
  go run ./cmd/openwebuictl show <link-id>
  ```

  For each ambiguous link, check the corresponding chat in the Open WebUI
  instance's own UI (the link's `has_remote_chat_id`/turn's
  `has_remote_chat_id` fields in `show`'s output say whether a chat id is
  even known to look for) to see whether it actually completed:
  - If it did complete with a usable reply, run
    `go run ./cmd/openwebuictl confirm <link-id>` (add
    `--remote-chat-id=<id>` only if the link does not already know one —
    `show`'s `has_remote_chat_id: false` — since this tool never lists the
    provider's chats to find one itself). This looks up the one turn's
    outcome and, on success, creates the recovered reply exactly as if
    the turn had completed live.
  - If it did not complete, or the chat cannot be identified at all, run
    `go run ./cmd/openwebuictl abandon <link-id>` instead. The owner's
    post is unaffected either way; only whether a reply eventually
    appears under it changes.

  A link stuck `creation_pending` (its claim job crashed or was killed
  before recording any outcome — `jobsctl show <job-id>` on the job named
  in `openwebuictl show`'s `state`/claim fields confirms this) cannot be
  confirmed or abandoned directly; freeze it first, then resolve it the
  same way:

  ```sh
  go run ./cmd/openwebuictl freeze <link-id>
  ```

  `auth_failed` turns/links are never retried automatically, by design
  (ADR-0005: a rejected credential is never blindly retried) — after
  rotating `OPENWEBUI_API_KEY`, the fix takes effect only for the
  **owner's next new post**; an already-`auth_failed` turn stays failed
  and its link, if `ambiguous`, still needs `confirm`/`abandon` above.
- **RSS feed or IMAP mailbox outage**: the same isolation applies — a
  broken feed or unreachable mail server only affects its own
  `external_source_poll` jobs, never `notes/create` or other sources (see
  configuration.md's "RSS/Atom ingestion" and "IMAP mail ingestion"
  sections). Diagnose with the same `jobsctl list --state=dead`/`show`
  commands above.
- **`cmd/mailfetch` unreachable** (`IMAP_ENABLED=true`): classifies as a
  retryable transport failure identical to a transient IMAP server
  outage (configuration.md's "Process isolation" section) — confirm the
  `mailfetch` process/container is actually running and that
  `IMAP_MAILFETCH_SOCKET` (server side) and `MAILFETCH_SOCKET_PATH`
  (`cmd/mailfetch` side) resolve to the same socket path/volume.
- **Suspected database corruption, or before relying on any backup**: see
  `cmd/backupctl`'s `verify` subcommand (see "Backup and restore" below)
  for a read-only schema/row-count check, and its `backup` subcommand for
  taking an online snapshot without stopping the server. If the live
  database itself is damaged, stop the server and restore from the most
  recent verified backup.

## Open WebUI outbound bridge: enabling and disabling

Issue #53's outbound turn bridge — the part of the Open WebUI integration
that sends an owner post to the configured model and turns its reply into
a local entry — is gated by two flags: `OPENWEBUI_ENABLED` (Issue #52's
registry and identity projection) and `OPENWEBUI_GENERATION_ENABLED` (the
bridge itself, a sub-flag only meaningful while the former is `true`). See
[configuration.md](configuration.md#outbound-turn-bridge-issue-53) for the
full mechanism this summarizes into operator steps.

### Enabling

Required config (`Config.Validate` fails startup closed if any of these is
missing or malformed — see
[configuration.md](configuration.md#known-configuration-keys) for the full
key table):

- `OPENWEBUI_ENABLED=true`
- `OPENWEBUI_BASE_URL` — the instance's `https` origin (scheme+host only)
- `OPENWEBUI_ALLOWED_ORIGINS` — comma-separated allowlist that must include
  `OPENWEBUI_BASE_URL` verbatim (ADR-0005 D11)
- `OPENWEBUI_API_KEY` — the dedicated adapter account's key (see "Secret
  rotation" below for how this is held and rotated)
- `OPENWEBUI_DEFAULT_MODEL_ID` — the provider's own model id
- `OPENWEBUI_PRESENTATION_HOST` — a bare hostname distinct from
  `LOCAL_ORIGIN`'s host

`OPENWEBUI_WORKSPACE_NAME` is cosmetic (it has a default). A model's
display name and handle slug (the `@<slug>@<presentation host>` local
half) are no longer configuration keys as of Issue #75: they are derived
automatically — from the provider's own model name once a catalog sync
succeeds, generated (never colliding, case-insensitively, with
`OWNER_USERNAME` or the reserved `assistant`/`system` names) the first
time each model is registered, and never recomputed afterward.

Steps:

1. Set the keys above in the config source (`.env` on a bare host; the
   container's environment-variable source otherwise) and restart (see
   "Starting and stopping" above). This alone only turns on identity
   projection — the model becomes visible as a VirtualActor — it does not
   make it reply yet.
2. To turn on replies, additionally set `OPENWEBUI_GENERATION_ENABLED=true`
   and restart again. Review `OPENWEBUI_TIMEOUT`/`OPENWEBUI_MAX_RESPONSE_BYTES`/
   `OPENWEBUI_MAX_REQUEST_BYTES`/`OPENWEBUI_MAX_CONTEXT_MESSAGES` first if
   the defaults look wrong for your instance — Issue #50's target-instance
   testing has not yet produced better values than these conservative
   defaults.
3. A restart that does not come back up almost always means one of the
   keys above is invalid, not that something else broke (see "Starting and
   stopping" above) — check the startup log for the failing `OPENWEBUI_*`
   key before assuming otherwise.
4. Optional (Issues #81/#84): citation footnotes render automatically
   whenever a reply's turn actually ran a tool call or web search — no
   config needed. To additionally show a generated chat title and an
   owner-only "view in Open WebUI" link on replies, set
   `OPENWEBUI_VIEWER_BASE_URL` to a browser-reachable HTTPS origin for the
   same instance (see configuration.md) and restart. This is separate
   from `OPENWEBUI_BASE_URL` and does **not** need to appear in
   `OPENWEBUI_ALLOWED_ORIGINS` — the server never dials it. A title may
   not appear on every reply even once this is set: whether the target
   instance generates it synchronously or asynchronously is unverified
   (docs/compat/openwebui-0.11.3.md point (i)), and a title that has not
   appeared yet by the time this adapter checks is simply left off, not
   shown incorrectly.

Verification:

1. `/readyz` returns `200` — confirms `Registry.Seed` completed without
   error (a `Seed` failure prevents the process from starting at all).
2. `POST /api/users/search` (as the owner) with a query matching the
   model's display name or generated handle slug — read
   `openwebui_models.display_name`/`actor_slug` directly if nothing has
   mentioned the model yet to have surfaced its slug some other way —
   returns the model as a user whose `host` is
   `OPENWEBUI_PRESENTATION_HOST`. Confirms the VirtualActor projects.
3. With generation also enabled: post a note as the owner and confirm a
   reply from that VirtualActor appears within `OPENWEBUI_TIMEOUT`. If it
   doesn't, `go run ./cmd/jobsctl list --type=openwebui_turn` and
   `go run ./cmd/openwebuictl links` (see "Incident response" above) show
   whether the turn is still pending, failed, or landed `ambiguous`.

### Model catalog: adding, removing, or losing access to a model

Since Issue #75, every model the configured account can see through
`GET /api/models` is synced into the registry automatically — there is no
separate enable step per model, and no config key names any model but the
one `OPENWEBUI_DEFAULT_MODEL_ID` fallback. `Registry.SyncCatalog` runs once
at startup (bounded, logged and non-fatal on failure) and then every
`OPENWEBUI_CATALOG_SYNC_INTERVAL` (default `10m`) as an ordinary
`openwebui_catalog_sync` durable job.

- **A new model appears in Open WebUI (a new connection, or a newly
  granted workspace custom model).** It becomes a VirtualActor on this
  service's next sync round — up to `OPENWEBUI_CATALOG_SYNC_INTERVAL` after
  it becomes visible to the account, never immediately. There is no manual
  "sync now" trigger as of this issue; to force it sooner, restart the
  process (the startup sync runs immediately) or shorten
  `OPENWEBUI_CATALOG_SYNC_INTERVAL` and restart. `go run ./cmd/jobsctl list
  --type=openwebui_catalog_sync` shows whether a round has run recently and
  whether it succeeded.
- **A model's access is revoked, or it is removed from Open WebUI.** The
  next successful sync round deactivates it: its VirtualActor stops
  resolving (`POST /api/users/search` no longer returns it, and a new
  `@mention` of its slug falls back to the default model per the decision
  table in [configuration.md](configuration.md#outbound-turn-bridge-issue-53)),
  but its row, actor, and every entry it already authored are left alone —
  nothing is deleted. If it reappears later, it is reactivated under its
  original local id, actor, and slug rather than being re-created.
- **The sync round itself fails (the target unreachable, a malformed
  response).** The registry is left exactly as the last successful round
  produced it — a transient outage never deactivates every model. Check
  `go run ./cmd/jobsctl list --type=openwebui_catalog_sync` for the job's
  own failure/retry state, and the service log for `openwebui: catalog
  sync job` errors.
- **A model's display name changed in Open WebUI.** The next sync round
  updates `openwebui_models.display_name`, but its handle slug never
  changes once assigned (the roadmap's stable-actor-ID requirement applied
  to the handle, not only the row id) — catalog sync never renames a
  handle to match. `internal/openwebui.Registry.RenameModel` exists at the
  use-case layer for this, but as of this issue no CLI or HTTP path calls
  it yet; a handle mismatch after a display-name change is cosmetic only
  (the model still resolves and still routes `@mention`s correctly under
  its existing slug).

### Disabling

- **Stop new replies only** (keep the VirtualActor and its history
  visible): set `OPENWEBUI_GENERATION_ENABLED=false` and restart. This
  deregisters the `"openwebui_turn"` job handler. A turn's job already
  claimed and running when the restart begins finishes first — the server
  drains in-flight durable-job handlers before exiting, the same graceful
  stop described in "Starting and stopping" above — but a turn's job still
  `pending` (not yet claimed) at restart keeps being claimed and retried
  afterward anyway: an unregistered job type is treated as an ordinary
  retryable failure, not a permanent one
  ([configuration.md](configuration.md#durable-jobs)), so it burns through
  `JOBS_MAX_ATTEMPTS` at exponential backoff — well under the default
  10-minute `JOBS_BACKOFF_MAX` ceiling, so typically minutes, not hours —
  and lands `dead` if generation stays off longer than that. Either way no
  turn silently disappears: `go run ./cmd/jobsctl list --type=openwebui_turn`
  shows its current state, and once generation is back on, a `dead` job
  needs a manual `go run ./cmd/jobsctl retry <job-id>` — it does not resume
  on its own the way a `pending` job that has not yet exhausted its
  attempts does.
- **Disable the feature entirely** (also stop identity projection):
  additionally set `OPENWEBUI_ENABLED=false` and restart. `Registry.Seed`
  only runs while this flag is `true`, so the seeded workspace/model rows
  are left exactly as they are — nothing is deleted or explicitly
  deactivated — and re-enabling later reconciles onto that same identity
  rather than minting a new one. Entries the VirtualActor already authored
  are unaffected; anywhere its author would otherwise be resolved instead
  falls back to the ordinary actor-ID projection used for any other
  unresolvable author — the same fallback already used when a model is
  deactivated or its workspace disabled directly.

The owner's own posts are never affected by either step —
`POST /api/notes/create` never depends on the bridge succeeding, enabled or
not.

## Secret rotation

None of this service's secrets are readable back once set —
`Config.Redacted()` only ever reports whether a secret field is set, not
its value (see [configuration.md](configuration.md#redaction)) — so
rotation is always "replace the config value and restart," never an
in-place update through any API:

1. `LLM_API_KEY`: obtain a new key from the LLM provider, update it in
   `.env` (bare host) or the container's environment variable source
   (`docker run -e` / Compose `environment:` / your orchestrator's
   secret store — never a mounted `.env` file in a container, per
   [configuration.md](configuration.md#running-in-a-container)), then
   restart the server. There is no dual-key overlap window: rotate during
   a maintenance window if the provider invalidates the old key
   immediately.
2. `IMAP_PASSWORD` (and `IMAP_USERNAME`, if it also changes): same
   procedure — update the config source, restart `bin/server` (which
   owns `internal/ingest/imap.Adapter`, the RPC client). `cmd/mailfetch`
   itself never sees these values as its own environment or command-line
   arguments (ADR-0003); it receives them only in each request's payload
   from the server, so `cmd/mailfetch` does not need restarting for a
   credential rotation alone, only the server does.
3. `OPENWEBUI_API_KEY`: same procedure — reissue the key on the Open
   WebUI instance (its dedicated adapter account, per ADR-0005 D10),
   update `OPENWEBUI_API_KEY`, restart the server. Reissuing a key on
   that instance invalidates the old one immediately; there is no
   dual-key overlap window, so rotate during a maintenance window if
   Open WebUI's outbound bridge (Issue #53) is in active use. The
   database never holds this value — only `OPENWEBUI_API_KEY`'s own
   *name* is stored as a workspace's `secret_ref` — so rotating the key
   never requires a database change, only the restart.
4. Any other config-only credential added by a future issue should follow
   the same pattern: it is not a case this runbook needs to special-case
   individually.

Rotation never requires a schema or code change; it only ever touches
the config source and a restart.

## Revoking access

`go run ./cmd/miauthctl` is the only way to grant or revoke access — see
the README's [Approving Aria sign-ins](../../README.md#approving-aria-sign-ins)
section and [configuration.md](configuration.md#approving-sessions-and-managing-tokens)
for the full command set. For an incident (a lost/compromised device, or
simply rotating routine access):

```sh
go run ./cmd/miauthctl tokens               # list issued tokens by ID (no hashes/raw values shown)
go run ./cmd/miauthctl revoke <token-id>     # revoke one immediately
```

A revoked token is rejected by `RequireScope` on its very next request;
there is no propagation delay to wait out. Revoking a token does not
remove the Owner actor itself — Aria must complete a fresh MiAuth flow
and be re-approved (`miauthctl approve`) to obtain a new one.

## Database and file permissions

- **Bare host**: `DB_PATH` and its directory should be owned by, and
  readable/writable only by, the OS user that runs `bin/server` (and
  `bin/backupctl`/`bin/jobsctl`/`bin/miauthctl`, which must run as that
  same user or one with equivalent access to operate on the same
  database — see configuration.md's "SQLite" section). There is no
  in-app enforcement of this; it is standard OS file permission hygiene
  (for example `chmod 600` on the database file, `chmod 700` on its
  parent directory), same as any other host-local SQLite deployment.
- **Container**: the `nonroot` base image runs as uid `65532` with no
  shell, so the host-side volume backing `/data` must already be
  writable by that uid before the container starts — see the README's
  [Container](../../README.md#container) section for the `chmod 777`
  (or matching `--user`) step. `chmod 777` is deliberately broad because
  the container has no shell to narrow it from the inside; if your host
  can pre-create the directory as uid `65532`, prefer `chown 65532` plus
  a narrower mode instead.
- **`cmd/mailfetch`**: on a bare host, run it under its own low-privilege
  OS user; in `docker-compose.yml` it already runs with a read-only root
  filesystem and every Linux capability dropped, per ADR-0003's
  untrusted-data hardening — see configuration.md's "Deploying
  `cmd/mailfetch`" section for both.

## Reverse proxy and TLS termination

This service has no TLS termination of its own — there is no certificate
or private-key configuration key anywhere in
[configuration.md](configuration.md)'s key table — and
`HTTP_HOST`/`HTTP_PORT` always serve plain HTTP. `LOCAL_ORIGIN` must
still be `https` in production (`Config.Validate`, per
configuration.md), which means **TLS termination is always the
responsibility of a reverse proxy sitting in front of this service** in
any production deployment; there is deliberately no bare-HTTP-facing
production configuration, and this service does not attempt to implement
one.

Any reverse proxy that terminates TLS and forwards to `HTTP_HOST:HTTP_PORT`
works; for example, an nginx `server` block:

```nginx
server {
    listen 443 ssl;
    server_name portal.example;

    ssl_certificate     /etc/letsencrypt/live/portal.example/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/portal.example/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

or a Caddy site block, which handles certificate acquisition/renewal
automatically:

```
portal.example {
    reverse_proxy 127.0.0.1:8080
}
```

`LOCAL_ORIGIN` must match the externally visible scheme+host the reverse
proxy serves (for example `https://portal.example`), not
`HTTP_HOST`/`HTTP_PORT`.

## Request rate and concurrency limits

This service's application layer intentionally implements no rate
limiting or concurrency limiting of its own. What it does implement is
request **size** and **timeout** bounding — `HTTP_MAX_BODY_BYTES` (via
`http.MaxBytesReader` on every request) and
`HTTP_READ_TIMEOUT`/`HTTP_READ_HEADER_TIMEOUT`/`HTTP_WRITE_TIMEOUT`/`HTTP_IDLE_TIMEOUT`
(see [configuration.md](configuration.md#known-configuration-keys)) —
which is a different concern from how many requests or connections are
allowed per unit time.

This is a deliberate, accepted design choice, not a gap: given the
single-owner, permission-gated nature of this deployment (only an
operator-approved Aria client ever holds a valid local API token, and
every route that is not `/healthz`, `/readyz`, `/api/meta`,
`/api/endpoints`, `GET /miauth/{session}`, or
`POST /api/miauth/{session}/check` requires one via `RequireScope` — the
last two are themselves the pre-authentication MiAuth flow, gated by
session-id knowledge and one-time consume rather than a token), the
realistic threat this would defend against is a
compromised or misbehaving already-trusted client, or an unauthenticated
flood against the small public surface — both are better handled at the
reverse proxy, which already terminates TLS and sees every connection
before this service does, than duplicated in the application. Introducing
an in-app rate/concurrency limiter with no concrete driving requirement
would also conflict with AGENTS.md's "new dependencies require a concrete
reason" and general small-surface guidance. If a future issue identifies
a concrete need the reverse proxy cannot satisfy, revisit this as its own
ADR-worthy decision rather than adding it silently here.

Configure rate and concurrency limits at the reverse proxy instead. For
example, nginx:

```nginx
limit_req_zone $binary_remote_addr zone=portal:10m rate=10r/s;
limit_conn_zone $binary_remote_addr zone=portal_conn:10m;

server {
    # ... TLS config as above ...
    location / {
        limit_req zone=portal burst=20 nodelay;
        limit_conn portal_conn 20;
        proxy_pass http://127.0.0.1:8080;
        # ... proxy_set_header directives as above ...
    }
}
```

or Caddy (with the `rate_limit` module, not bundled in Caddy's default
build — see the module's own installation instructions):

```
portal.example {
    rate_limit {
        zone portal {
            key {remote_host}
            events 10
            window 1s
        }
    }
    reverse_proxy 127.0.0.1:8080
}
```

Tune the actual numbers to your Aria client's real request pattern; the
values above are starting points, not a recommendation specific to this
service.

## Log retention

`internal/logging` writes structured logs to standard output only (text
in development, JSON in production — `LOG_FORMAT`, enforced by
`Config.Validate`; see [configuration.md](configuration.md#production-hardening)).
This service implements no log file, rotation, or retention policy of its
own — there is no `LOG_FILE` config key and none is planned, matching the
twelve-factor "logs are an event stream written to stdout" model. Log
rotation and retention are entirely the operator's responsibility, at
whichever layer captures stdout in your deployment:

- **Bare host under systemd**: `journald`'s own retention config
  (`SystemMaxUse=`, `MaxRetentionSec=`, etc. in `/etc/systemd/journald.conf`
  or a drop-in) governs how long logs are kept.
- **Bare host without systemd**: redirect `bin/server`'s stdout through
  `logrotate` (or an equivalent) if you are not already capturing it with
  a supervisor that rotates for you.
- **Container**: Docker's own logging driver and its rotation options
  (`--log-opt max-size=`/`max-file=` for the default `json-file` driver,
  or a different driver entirely) apply; `docker-compose.yml` does not
  currently set `logging:` on either service, so both use the Docker
  daemon's configured default driver and its default (often unbounded)
  retention — set `max-size`/`max-file` explicitly if that default is not
  acceptable for your host's disk.

Regardless of the retention window, remember that this service already
redacts known-sensitive log attribute keys and never logs request/response
bodies or prompts (see [configuration.md](configuration.md#redaction)) —
log retention policy is about disk usage and audit-trail length, not
about secrets appearing in logs in the first place.

## Backup and restore

`cmd/backupctl`, an online-safe SQLite backup/verify tool, and a
documented restore procedure are Issue #13 AC6's evidence. See
[backup-restore.md](backup-restore.md) for the full `backupctl backup`/
`backupctl verify` usage and the manual restore steps — this runbook's
"Incident response" section above only points to it for the corruption/
restore scenario, rather than duplicating the procedure.

Issue #76's `app_config`/`app_config_audit` tables (the runtime
configuration overlay above) live in the same SQLite database as
everything else, so this same backup/restore procedure already covers
them; no separate export is needed beyond `miauthctl config export`,
which is for moving values to a *different* host, not for disaster
recovery.
