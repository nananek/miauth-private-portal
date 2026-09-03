# Open WebUI integration roadmap addition

- Status: Planned, opt-in P1 extension; not required for the Issue #1 MVP
- Source plan: document board key `openwebui-roadmap-plan`
- Scope: single-owner `miauth-private-portal` service and one allowlisted Open WebUI workspace

## Goal

Add an opt-in Open WebUI workspace/model projection, durable chat import, and
an explicit owner-triggered completion bridge without changing the existing
single-owner Aria/MiAuth MVP or treating an external model as a login-capable
user.

Open WebUI integration is a dedicated feature track, not merely an alternate
provider for #9. It adds workspace/model identity projection, durable external
chat synchronization, branch-preserving message mapping, and a completion
bridge with separate permission and secret boundaries. The existing #2 auth
topology and Aria contract remain unchanged and are prerequisites.

## Roadmap placement

The work is inserted at two points in the existing roadmap:

- **Phase 0.5, after #2 and before #4:** OWUI-C freezes the Open WebUI API,
  identity, security, and storage contract. It may proceed in parallel with
  #3, but must finish before #4 starts its schema addendum.
- **Phase 2, after #8 and alongside #9/#10:** OWUI-P adds workspace/model and
  VirtualActor projections after #4/#6/#7 are available. OWUI-S then adds
  durable chat sync and the explicit completion bridge after #8, in parallel
  with #9/#10.

The original #13 release gate does not depend on this feature. If the feature
is promoted into the same release, OWUI-S and its operations/E2E gate become a
new explicit #13 dependency. Otherwise, the feature remains disabled by
default and cannot change the #1 MVP behavior.

The parent issue should use the next available GitHub issue number (currently
expected to be #19, but not fixed here). OWUI-C, OWUI-P, and OWUI-S are child
task names to split into separate issues or PRs under that parent.

Dependency graph:

```text
#2 -> OWUI-C
#3 + OWUI-C -> #4 schema addendum
#4 + #6 + #7 + OWUI-C -> OWUI-P
#6 + #8 + OWUI-P -> OWUI-S
OWUI-S -> #13 additional E2E/operations gate (only if promoted)
```

## Integration boundary

Open WebUI is an external provider/source. Domain and use-case code must not
depend on its HTTP API, JWT format, or database model. The boundary is:

- **Domain/use-case:** `WorkspaceDefinition`, `ModelDefinition`,
  `VirtualActor`, `ExternalChatLink`, `ExternalMessageLink`, `SyncCursor`,
  and `OpenWebUIJob`. The MVP has one enabled workspace and one configured
  default model; model selection is never a client-supplied capability.
- **Provider port:** `ListChats(updated_after/cursor)`,
  `GetChat(external_chat_id)`, and
  `Complete(model_id, messages, correlation/idempotency metadata)`. `CreateChat` and
  `AppendMessage` are allowed only after a pinned target contract proves they
  are needed.
- **Adapter:** absorbs Open WebUI endpoint/version differences and Bearer API
  key/JWT authentication. OpenAI-compatible chat completions are used only by
  the explicit generation bridge.
- **Local API/Aria projection:** exposes synchronized local threads through
  #6 and #7 projections. Open WebUI credentials and chat APIs never reach Aria.

The adapter must use a pinned Open WebUI API/version and a fixed HTTPS origin
allowlist. It must not discover or connect to an arbitrary URL supplied by a
user, and it must revalidate every redirect hop (or disable redirects) against
that allowlist and the resolved-IP SSRF policy. This feature's “virtual
federation” means local presentation of an external model actor only; it does
not add Misskey or ActivityPub federation.
No Open WebUI source is copied into this repository; the contract is captured
with redacted fixtures and a narrow adapter.

## OWUI-C: contract and identity ADR

Placement: after #2 and before #4 schema work.

Acceptance criteria:

- [ ] Add `docs/decisions/0002-openwebui-boundary.md` covering the provider
  boundary, local source of truth, VirtualActor, non-federation scope, and
  auth/secret threat model.
- [ ] Add a pinned compatibility document covering target base URL policy, API
  version, used endpoints, request/response fields, pagination, auth, errors,
  rate limits, nullable/unknown fields, and chat/message ID semantics.
- [ ] Complete target-instance verification for persistent chat, workspace
  permission, and the default model. If persistent chat is unavailable,
  explicitly select completion-only mode and remove chat import from this MVP.
- [ ] Store redacted fixtures for linear and branched `parentId`/`currentId`
  histories, user/assistant/system messages, tool metadata, edit/delete
  cases, 401/429, and malformed responses.
- [ ] Update #2/#1 traceability for #3, #4, #6, #7, #8, #9, and #13 impacts;
  never treat Open WebUI credentials as MiAuth or local API tokens.

Implementation must not begin until the target Open WebUI version/API surface,
persistent chat endpoint availability, real workspace/default model IDs,
workspace/model permissions, and credential provision/rotation ownership are
recorded. These are implementation start conditions, not blockers for
publishing this roadmap.

## OWUI-P: registry and identity projection

Placement: after #4, #6, and #7; feature flag remains off by default.

The minimum registry model is:

### WorkspaceDefinition

- immutable local `id`
- owner-configured display `name`
- HTTPS `base_url` restricted to the fixed Open WebUI origin allowlist
- `secret_ref`, never the API key itself
- `default_model_id` FK to a model in the same workspace
- fixed presentation `federation_host` (MVP value:
  `openwebui.tail1a2b3c.ts.net`)
- `sync_enabled`, `generation_enabled`, timestamps, and `last_sync_status`

### ModelDefinition

- immutable local `id` and `workspace_id`
- opaque `external_model_id`; never regenerate it from a display name
- `display_name`, `actor_id`, `active`, and verified `capabilities`
- optional provider `external_updated_at`

`default_model_id` points to exactly one model in its workspace. Workspace
disable/delete and related actor/job behavior must be one transaction. A model
actor's stable local ID must survive display-name or handle changes. MVP
publishes only the default model as an actor.

### VirtualActor

The default model is a presentation actor, not a login-capable user:

- handle: `@model-slug@openwebui.tail1a2b3c.ts.net`
- host: `openwebui.tail1a2b3c.ts.net`
- stable local `actor_id`, `source=openwebui`, external model/workspace metadata
- `is_remote=true`, `is_loginable=false`, `can_miauth=false`,
  `can_own_secret=false`

The host is a fixed presentation/lookup value. The service does not discover
external actors. A workspace is a container, not normally a user; if a future
requirement needs a workspace actor, it must be a separate non-loginable actor
row rather than reusing the model actor.

Acceptance criteria:

- [ ] Add workspace/model/link/cursor migrations and narrow repository
  interfaces after OWUI-C and the #4 schema addendum.
- [ ] Enforce same-workspace default-model FK, unique external IDs,
  stable actor IDs, and enabled/disabled constraints.
- [ ] Connect VirtualActor to #6 entries and #7 `UserLite`/`Note` projections;
  exclude it from login and MiAuth paths.
- [ ] Enforce owner-only workspace changes, `secret_ref`, feature flags, and
  fixed presentation-host validation.
- [ ] Add migration, serialization, unknown/null field, and stable-cursor
  contract tests.

## OWUI-S: adapter, sync, and thread/completion bridge

Placement: after #8 and #6/OWUI-P, in parallel with #9/#10.

### Chat-to-thread mapping

- one persistent `chat_id` maps to one local thread;
- `message.id` maps to `external_message_id`;
- `(workspace_id, external_chat_id, external_message_id)` is the import
  idempotency key;
- `message.parentId` maps to the local parent relation;
- `history.currentId` is stored as current/head metadata;
- provider timestamp and local `received_at` remain separate;
- title, model, sources, files, citations, and usage stay provenance metadata,
  separate from source text.

Branches are retained when #6 can represent parent relations. Aria displays
the `currentId` active branch by default; other branches remain children or
related entries. If the existing domain cannot retain all branches, the MVP
must explicitly select active-branch-only mode before implementation and
must not silently discard the others; that mode and its loss of inactive
branches must be recorded in OWUI-C and the release gate.

Role mapping:

| Open WebUI role | Local representation | Boundary rule |
| --- | --- | --- |
| `user` | owner post or imported user message | Keep authentication/provenance distinct; local source text is immutable |
| `assistant` | `llm_reply` from the default model VirtualActor | Show only through the fixed virtual host |
| `system` | system metadata | Do not expose system prompts in the normal timeline; owner-only/debug scope if required |
| `tool`/`function`/`developer` | restricted metadata | Do not promote to normal actors/messages in MVP |
| files/sources/citations | attachment/provenance metadata | Apply size, content-type, URL/SSRF, and secret-redaction rules |

External assistant edits create a revision/tombstone rather than destroying a
local entry. External deletes become `deleted`/`hidden` provenance. Equality is
based on external IDs/links, never body text.

Absence from a partial, filtered, or permission-limited response is not
treated as deletion. A tombstone requires an explicit provider deletion signal
or a verified complete snapshot according to the pinned contract.

Imported `user` messages are accepted only when the pinned workspace contract
maps their external author to the single local owner. Messages from any other
author are quarantined as restricted provenance or make the sync contract
fail; they must not become another login-capable or timeline actor. Message
content, system text, tool metadata, and citations are untrusted data and are
never executed or treated as instructions. File/source URLs are metadata-only
in this MVP unless a separately allowlisted, SSRF-safe fetch contract is
accepted.

### Completion bridge

An owner post and its `OpenWebUIJob` intent are committed atomically before
the provider call. The provider may fail without making the post disappear.
The bridge stores the submitted message sequence and response against the
local chat link. A stateless completion endpoint must not be presented as
persistent remote history. MVP uses a buffered final response or an explicit
partial/failed assistant entry; streaming is optional.

Acceptance criteria:

- [ ] Add an `OpenWebUIProvider` port and HTTP adapter with timeout, redirect,
  request/message/response-size bounds, TLS/origin, Bearer/key-header, and
  redacted-error handling in one boundary.
- [ ] Add `sync_workspace`, `sync_chat`, and `openwebui_completion` durable
  jobs using #8 leases, bounded retry/dead state, restart recovery, and
  idempotency. Cursor advancement must be transactional with the completed
  page/detail upserts and must stop on any partial or contract-failed page.
- [ ] Implement chat/message, parent/current branch, provenance,
  revision/tombstone mapping without overwriting user-authored text.
- [ ] Implement owner action -> post -> job -> completion -> assistant entry
  E2E; local post success must not depend on provider success.
- [ ] Test read-only cursor sync, duplicates, reordering, branches, edits,
  deletes, outage, auth failure, and schema drift.

## Auth, permission, and secret boundary

The four identities remain distinct:

1. local owner MiAuth/session;
2. local API token used by Aria;
3. workspace-scoped Open WebUI credential/API key;
4. VirtualActor, which is presentation-only and not an authentication
   principal.

The local owner must already be authenticated through the #2 MiAuth/local API
token boundary. An Open WebUI credential cannot authenticate Aria, create a
local owner, mint a local API token, or expand the existing local API scopes.
Open WebUI calls are made only by the server-side adapter with the
workspace-scoped credential.

The completion bridge always uses the enabled workspace's configured
`default_model_id`; no Aria request may select an arbitrary workspace or model.
The adapter also verifies that the model belongs to that workspace before any
provider call.

Open WebUI uses a dedicated low-privilege workspace account/key. Only a
`secret_ref` crosses the domain/config boundary; the raw key is isolated in
the adapter/secret store and is never written to the database, fixtures, URL,
error body, logs, traces, backups, or planning documents. Rotation/revocation
and the responsible operator are documented before enabling the feature.
401/403 are permanent
`auth_failed` states until reauthentication or rotation; they are not blindly
retried.

Default permissions:

- workspace creation, credential rotation, model refresh, sync, and
  generation are owner-only;
- local timeline access follows the existing #1/#5/#7 owner policy;
- VirtualActor cannot log in, use MiAuth, own credentials, or perform remote
  actions;
- arbitrary workspace/model switching and tool/function/MCP execution are
  outside MVP;
- user-supplied base URL, host, redirect, and callback are never used as-is;
  adapter validation enforces HTTPS, fixed origin allowlist, redirect-hop
  revalidation (or redirects disabled), timeout, response-size limits, and
  private-IP/localhost rejection at connection time. A Tailnet address is
  permitted only as an explicit deployment-provisioned origin in the fixed
  allowlist; arbitrary private addresses remain rejected.

Tailnet reachability is a deployment prerequisite, not application
authorization. Network identity cannot replace owner permission checks.

## Sync, idempotency, cursor, retry, and error contract

Sync runs on #8's SQLite-backed durable jobs:

- workspace pull obtains pinned chat list/detail pages and stores cursor,
  `updated_at`, and last successful sync only after the corresponding page and
  detail upserts commit;
- per-chat sync upserts the complete message tree;
- outbound generation runs after owner post commit under a leased job;
- imports use `(workspace_id, external_chat_id, external_message_id)`;
  generation persists a unique local request/correlation ID before calling the
  provider and uses an atomic local response upsert to suppress duplicate
  assistant entries. This does not make a non-idempotent remote completion
  exactly-once: OWUI-C must verify a provider idempotency key or result lookup;
  without one, an ambiguous timeout is not blindly retried and the job is
  marked dead for operator recovery;
- cursors include a provider-defined timestamp plus an opaque tie-breaker;
  timestamp alone is not sufficient. Cursor inclusivity/exclusivity and the
  provider's ordering must be pinned in OWUI-C; a cursor advances only after
  the complete page/detail transaction succeeds.
- statuses distinguish `healthy`, `degraded`, `auth_failed`,
  `contract_failed`, and `retry_exhausted`.

Error behavior:

- 401/403: no blind retry; mark `auth_failed` and require owner
  reauthentication, permission correction, or credential rotation;
- 408/429/5xx/network timeout on idempotent reads: bounded exponential
  backoff, honoring a valid `Retry-After` only within the configured maximum
  delay, then dead/retry-exhausted at the configured limit;
- completion timeouts or other ambiguous non-idempotent outcomes are not
  automatically replayed unless the pinned provider contract supplies an
  idempotency key or result lookup;
- invalid model/request 4xx: permanent failure until configuration changes;
- malformed/unknown schema: `contract_failed`; retain only redacted payload
  metadata and metrics;
- streaming disconnect: store incomplete partial content and do not duplicate
  the assistant entry for the same local request;
- remote edit/delete: revision/tombstone, never local hard delete;
- missing messages in a partial or filtered response are not inferred to be
  deleted;
- restart: expired leases recover jobs; provider failure never removes the
  committed owner post.

MVP is not automatic bidirectional editing. The local mirror is the source of
truth; external changes are provenance-bearing imports/revisions.

## MVP and non-goals

### Included when enabled

- one fixed, allowlisted workspace;
- one owner-selected default model;
- one non-loginable VirtualActor projection;
- read-only persistent-chat pull only when the pinned target contract passes;
- idempotent `chat_id`/message/parent sync;
- one explicit owner-triggered completion and assistant entry;
- cursor, job status, provenance, tombstone/revision;
- restart, duplicate delivery, 401/403, 429/5xx, malformed response, and
  credential-redaction tests.

### Excluded

- automatic discovery or management of all Open WebUI workspaces/models;
- Misskey/ActivityPub federation, remote discovery, signatures,
  inbox/outbox, or remote callbacks;
- tool/function/MCP execution or autonomous agent loops;
- arbitrary Open WebUI URLs or unrestricted tenant switching;
- complete bidirectional local/remote edit/delete synchronization;
- required streaming, unlimited attachment-body ingestion, or system-prompt
  publication;
- multi-user/role model, PostgreSQL, and custom UI.

The feature must preserve #1's single-owner, non-federation, and
non-tool-execution boundaries. Shared provider code with #9 may be reused,
but chat-history import and VirtualActor remain behind a separate feature flag
and release gate.

## Implementation start conditions

The first implementation item is OWUI-C-1 through OWUI-C-4: freeze the
boundary/compatibility contract, identity ADR, and redacted fixtures before
writing migrations or adapters. Start only after these are known:

- target Open WebUI version and API surface;
- persistent-chat endpoint availability and field semantics;
- real workspace/default-model IDs and permissions;
- credential provisioning, rotation, and revocation owner;
- fixed HTTPS base URL and presentation-host allowlist;
- whether full chat import is supported; otherwise select completion-only mode.

If any target-instance contract is unverified, do not emulate success. Keep
the feature disabled or use the explicitly documented completion-only mode.

## Verification and release gate

- **Existing regression:** `gofmt`, `go test ./...`, `go vet ./...`, and
  `go test -race ./...` where concurrency/jobs are changed; feature flag off
  must leave #1 auth, posts, threads, and source ingestion unchanged.
- **Contract:** fixture-only schema/nullable/unknown/error tests, API-version
  drift, rate-limit behavior, and redaction with no credentials or network.
- **Identity:** stable actor ID across display-name/handle changes,
  disabled/default switches, same external model IDs, and rejection of
  VirtualActor login/MiAuth.
- **Thread:** chat/message idempotency, parent/current branch, reordering,
  duplicate delivery, edit/delete revision, and source timestamp versus
  `received_at`.
- **Jobs:** outage, 429/5xx, 401/403, timeout, malformed response, lease
  expiry, restart, retry exhaustion, and partial stream.
- **Security:** arbitrary base URL/redirect/host, private-IP/SSRF, oversized
  payload, key leakage in logs/traces/fixtures/backups, and no
  tool/function/MCP execution.
- **Opt-in Aria E2E:** owner-only sync, default-model display, thread
  reload/restart, backup/restore, rotation, permission denial, and provider
  outage against the target instance only when explicitly enabled.

Only if the feature is promoted into the same release does #13 require the
OWUI-S E2E/operations runbook, backup/restore, rotation, and security gate.

## References

- Open WebUI API reference: <https://docs.openwebui.com/reference/api-endpoints/>
- Open WebUI API keys: <https://docs.openwebui.com/features/authentication-access/api-keys/>
- Open WebUI database schema: <https://github.com/open-webui/docs/blob/main/docs/reference/database-schema.md>
- Chat import/export and `history.currentId`:
  <https://github.com/open-webui/docs/blob/main/docs/features/chat-conversations/data-controls/import-export.md>
