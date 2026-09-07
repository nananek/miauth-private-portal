# ADR-0006: DB-backed runtime configuration overlay, with secrets and network destinations excluded

- Status: Accepted for Issue #76
- Date: 2026-09-08
- Scope: Issue #76 (runtime configuration DB migration and `miauthctl config`)

## Context

Every configuration value this service reads comes from
`internal/config.Load`: defaults, an optional dotenv-style file, and
environment variables, in that increasing priority order
(`internal/config/config.go`'s own package doc). Changing any of it —
adding an RSS feed, tuning a job's poll interval, switching an LLM model
— requires editing the file or environment and restarting the process.
Issue #76 asks for a subset of these values to be readable and writable
through a host-local CLI (`miauthctl config ...`, ADR-0002's existing
"SSH to the host" authorization boundary) and to take effect without a
restart, with changes recorded in an audit trail.

Three things bound this ADR's scope, each decided explicitly (with the
issue owner, 2026-09-08) rather than left implicit:

1. **Secrets stay out of the database.** ADR-0005 D10 already decided
   that Open WebUI's API key lives in config (env/file), never a
   database column, and declined to build a dedicated secret store or
   encryption-at-rest mechanism. This ADR extends the same decision to
   every other credential-shaped key (`LLM_API_KEY`, `IMAP_USERNAME`,
   `IMAP_PASSWORD`) rather than opening a second, narrower exception:
   key management is a real subsystem this service does not have, and
   Issue #76 is not the place to build one.
2. **Network-destination settings are excluded.** `OPENWEBUI_BASE_URL`,
   `OPENWEBUI_ALLOWED_ORIGINS`, `IMAP_HOST`/`PORT`/`TLS_MODE`,
   `LLM_BASE_URL`, and similar keys name where this service connects to.
   `config.Validate`'s SSRF-allowlist scrutiny (ADR-0005 D11) runs once,
   at startup, against a value that is then trusted for the rest of the
   process's life; accepting a live change to a connection target
   through a CLI would mean re-deriving that scrutiny at runtime, or
   silently weakening it. Neither is worth it for this issue.
3. **Reload is not limited to RSS/Open WebUI.** The issue's own examples
   were RSS feeds and Open WebUI settings, but this service has several
   other continuously-running or continuously-polled components with
   the same "restart is the only way to change a tunable" problem
   (`internal/jobs.Manager`'s polling/leasing/retry knobs,
   `internal/llmreply`/`internal/llmclassify`'s per-request model and
   bounds, the IMAP fetch job handler's per-request bounds). This ADR's
   reload mechanism is written generically enough to cover all of them,
   not special-cased to RSS/Open WebUI.
4. **Dynamic subsystem on/off is excluded.** `LLM_ENABLED`,
   `LLM_CLASSIFICATION_ENABLED`, `RSS_ENABLED`, `IMAP_ENABLED`, and
   `OPENWEBUI_ENABLED` each gate a one-time `if cfg.X.Enabled { ... }`
   branch in `cmd/server/main.go` that constructs (or does not
   construct) a whole subsystem — service, scheduler, and job handler —
   at startup. Making one of these toggleable at runtime would require a
   supervisor able to start and stop goroutines this service currently
   only ever builds once, which is a different, larger design problem
   than "reload a tunable value." These five keys stay restart-only;
   a dynamic subsystem supervisor, if ever needed, is a separate future
   issue.

## Decision

### Scope

A DB-backed runtime configuration store (`app_config`, migration 0022)
that overlays `internal/config.Load`'s resolved value for a fixed,
explicit subset of known keys. Secret encryption/key management and an
HTTP administration API are both out of scope (the latter also matches
AGENTS.md's "no custom web UI" boundary; ADR-0002's host-local CLI
pattern already covers authorization for host-local operators).

### Precedence

**DB (when a row exists for that key) > environment variable > config
file > default.** This is `internal/config.Load`'s existing
file/env/default resolution with one more, higher-priority layer added
on top, read only after the database is open — bootstrap-only keys
(anything needed to open the database or start the process at all, plus
every key in categories 2 and 4 above) are read exactly as before, only
through `Load`, and never consulted against the database.

### The three-way key classification

Every key `internal/config/schema.go` knows about falls into exactly one
of three classes (`internal/config.ClassOf`, `internal/config/keyclass.go`
is the single source of truth — this table documents it, but the code,
not this ADR, is authoritative if the two ever drift):

| Class | Meaning | Examples |
|---|---|---|
| **bootstrap-only** | Read once at startup through `config.Load`; never consulted against the database. Includes process topology (`DB_PATH`, `HTTP_HOST`/`PORT`), every network-destination setting, the five `*_ENABLED` subsystem-construction flags, `JOBS_WORKER_ID` (identifies this process's own in-flight lease ownership), and `OPENWEBUI_GENERATION_ENABLED` (already independently live via the `openwebui_workspaces` row, outside this mechanism entirely). | `LOCAL_ORIGIN`, `OPENWEBUI_BASE_URL`, `IMAP_HOST`, `RSS_ENABLED` |
| **secret** | Never written to `app_config`; `miauthctl config set/import` reject these keys outright. `internal/config.IsSecretKey`. | `LLM_API_KEY`, `OPENWEBUI_API_KEY`, `IMAP_USERNAME`, `IMAP_PASSWORD` |
| **db-eligible** | May have an `app_config` row overriding the bootstrap value. `internal/config.IsDBEligibleKey`/`DBEligibleKeys()`. | `JOBS_POLL_INTERVAL`, `RSS_FEED_URLS`, `LLM_MODEL`, `OPENWEBUI_WEB_SEARCH_ENABLED` |

The db-eligible set (29 keys as of this ADR) groups by the component
that reloads it:

- **`internal/jobs.Manager`**: `JOBS_POLL_INTERVAL`,
  `JOBS_CLAIM_BATCH_SIZE`, `JOBS_LEASE_DURATION`,
  `JOBS_LEASE_RENEW_MARGIN`, `JOBS_MAX_ATTEMPTS`, `JOBS_BACKOFF_BASE`,
  `JOBS_BACKOFF_MAX`, `JOBS_MAX_CONCURRENT`, `JOBS_SHUTDOWN_GRACE_PERIOD`.
- **`internal/ingest.Scheduler`** (the same type backs both RSS and
  IMAP polling — a fix applied to one automatically applies to the
  other): `RSS_POLL_INTERVAL`, `RSS_FEED_URLS`, `RSS_SUMMARY_MAX_CHARS`,
  `IMAP_POLL_INTERVAL`.
- **The IMAP fetch job handler** (not `cmd/mailfetch` itself, which
  carries no config of its own — ADR-0003): `IMAP_FETCH_TIMEOUT`,
  `IMAP_MAX_MESSAGE_BYTES`, `IMAP_SNIPPET_MAX_CHARS`,
  `IMAP_STORE_FULL_BODY`, `IMAP_FULL_BODY_MAX_CHARS`.
- **`internal/llmreply.Service` / `internal/llmclassify.Service`**:
  `LLM_MODEL`, `LLM_TIMEOUT`, `LLM_MAX_OUTPUT_TOKENS`,
  `LLM_THREAD_CONTEXT_MAX_MESSAGES`, `LLM_THREAD_CONTEXT_MAX_CHARS`,
  `LLM_CLASSIFICATION_MODEL`, `LLM_CLASSIFICATION_MAX_OUTPUT_TOKENS`,
  `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_MESSAGES`,
  `LLM_CLASSIFICATION_THREAD_CONTEXT_MAX_CHARS`.
- **Open WebUI's catalog scheduler and turn job**:
  `OPENWEBUI_CATALOG_SYNC_INTERVAL`, `OPENWEBUI_WEB_SEARCH_ENABLED`.

`RSS_FEED_URLS` is db-eligible even though it names URLs, unlike the
network-destination keys excluded above: it has no SSRF allowlist to
re-derive (feed fetches go through `internal/ingest/safehttp`'s own
policy, enforced per request regardless of when the URL was configured),
and it is this issue's own motivating example.

### Storage

`app_config` (current value) and `app_config_audit` (immutable change
log), migration 0022. `{key, value, version, updated_at, updated_by}`:
no type, description, or secret-flag column — `internal/config` already
owns the type/bounds (`ValidateKeyValue`, reusing `parse()`'s per-field
logic), description, and classification for every key, so duplicating
any of that into the schema would just be a second place for it to go
stale. `value` is the same raw text representation an environment
variable would carry.

`version` starts at 1 on a key's first write and increments by one on
every subsequent write, backing `domain.ConfigRepository.Set`'s
compare-and-set semantics: a caller supplies the version it last read (or
0 for "no row must currently exist yet"), and a write against a since-
changed or since-deleted row fails with `domain.ErrConflict` rather than
silently overwriting a change it never saw — the same WHERE-clause CAS
shape this codebase's other repositories already use (for example
`OpenWebUIConversationLinkRepository.MarkReady`).

`app_config_audit` records every `Set`/`Unset` (including the startup
auto-seed path's first-time writes) in the same transaction as the
`app_config` write it describes (`domain.UnitOfWork.WithinTx`), so a
change and its audit trail can never disagree.

### Migration from file/env to DB

Startup performs an automatic, idempotent seed: for every db-eligible
key with no existing `app_config` row, the current effective value
(file/env/default) is written as that key's first DB row (an existing
row is never overwritten). `miauthctl config import [--from-env]
[--file <path>]` offers the same operation manually, for bulk import,
moving to a new host, or re-seeding after a database reset.

### Auth and audit

No new permission model: ADR-0002's "SSH to the host and run the CLI
against the same `DB_PATH` the server process uses" is the entire
authorization boundary, exactly as every other `*ctl` command already
works. `updated_by`/`changed_by` record the single owner actor's id
(`domain.ActorOwner`, resolved the same way `cmd/openwebuictl` already
does).

### Backup

Configuration lives in the same SQLite database as everything else, so
the existing `backupctl` (`VACUUM INTO`-based online backup) already
covers it; no new backup mechanism is introduced.

## Consequences

- A db-eligible key's effective value can now differ from what
  `config.Load`, `Config.Redacted`, and `docs/operations/configuration.md`
  alone would suggest — any diagnostic or documentation describing "the"
  value of such a key must account for a possible `app_config` override.
- Every reload-consuming component (`internal/jobs.Manager`,
  `internal/ingest.Scheduler`, `internal/llmreply`/`internal/llmclassify`,
  the IMAP fetch job handler, Open WebUI's catalog scheduler/turn job)
  gains a dependency on reading the DB overlay at the point it consumes
  a tunable value, rather than reading a `Config` value captured once at
  construction. Each component's own reload wiring is a separate PR
  (Issue #76 PR4a/4b/4c) built on this ADR's storage and classification.
- A future request to make network-destination settings or a `*_ENABLED`
  flag DB-backed is a new decision, not an extension of this one: both
  are excluded here for reasons (SSRF-allowlist re-derivation, dynamic
  subsystem lifecycle) that a future issue would need to resolve on
  their own terms, not by widening `dbEligibleKeys`.
- Secret rotation remains exactly what it was before this issue: edit
  the config file or environment and restart. `miauthctl config`
  provides no path around that, by design.
