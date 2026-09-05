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
3. Any other config-only credential added by a future issue should follow
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
