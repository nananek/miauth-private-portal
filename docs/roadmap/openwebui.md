# Open WebUI outbound chat and Aria reply-tree roadmap addition

- Status: Planned, opt-in P1 extension; not required for the Issue #1 MVP
- Source plan: document board key `openwebui-reply-tree-plan`
- Scope: single-owner `miauth-private-portal` service and one allowlisted Open WebUI workspace

## Goal

Add an opt-in outbound bridge where an Aria root message or first/unlinked
reply starts a new Open WebUI persistent chat and returns the model response
into the same Aria thread. The Aria `reply_to_id` reply tree is the
conversation source of truth; Open WebUI `chat_id`, `message.id`, `parentId`,
and `currentId` are
opaque correlation metadata only. This does not change the existing
single-owner Aria/MiAuth MVP or make an external model a login-capable user.
Each newly created remote chat is linked to one local Aria thread and branch,
preserving the chat-to-Aria-thread mapping without reading existing chats.

Open WebUI integration is a dedicated feature track, not merely an alternate
provider for #9. It adds workspace/model identity projection, durable outbound
turn jobs, local reply-tree mapping, and a completion bridge with separate
permission and secret boundaries. Existing Open WebUI chat open/import/list,
pull sync, reconciliation, history browsing, and bidirectional edit/delete
are explicit non-goals. The existing #2 auth topology and Aria contract remain
unchanged and are prerequisites.

## Roadmap placement

The work is inserted at two implementation points and one conditional release
gate in the existing roadmap:

- **Phase 0.5, after #2 and before #4:** OWUI-C freezes the Open WebUI API,
  identity, security, and storage contract. It may proceed in parallel with
  #3, but must finish before #4 starts its schema addendum.
- **Phase 2, after #8 and alongside #9:** OWUI-P adds the local thread/link,
  turn, branch, and VirtualActor domain projection after #4/#6 are
  available. Its Aria-facing serialization is consumed when #7 reaches the
  relevant projection work. OWUI-B then adds the outbound Aria-message →
  new-chat/continued-turn → local-assistant bridge after #7/#8, in parallel
  with #9.
- **Phase 3 / #13 opt-in gate:** OWUI-R adds the release E2E and operations
  evidence only if this feature is promoted into the same release.

The original #13 release gate does not depend on this feature. If the feature
is promoted into the same release, OWUI-R becomes an explicit #13 dependency;
otherwise the feature remains disabled by default and cannot change the #1 MVP
behavior.

PR #19 is the Issue #2 contract-document PR; it is not the parent of this
feature. This roadmap is scoped under Issue #1. OWUI-C, OWUI-P, OWUI-B, and
OWUI-R are separate child issue/PR units to create under #1 (or under a new
Open WebUI umbrella issue if the tracker requires one). Each implementation
PR must handle exactly one child, link the applicable parent requirement, use
`Closes #<child>` for that child only, and never close #1 or #2 as a side
effect. The current Issue #2 contract-document PR is not one of those OWUI
children. The outbound revision supersedes the former import-oriented OWUI-S
label.

The existing #7 Misskey wire layer remains the transport consumer for any
Aria-visible projection and keeps its original #2/#5/#6 dependency order.
OWUI-P does not promote new endpoints or make #7 an earlier prerequisite; it
only supplies the local projection for #7 to consume when that issue reaches
it.

Dependency graph:

```text
#2 -> OWUI-C
#3 + OWUI-C -> #4 schema addendum
#4 + #6 + OWUI-C -> OWUI-P (domain/storage)
#7 + #8 + OWUI-P -> OWUI-B (Aria bridge)
OWUI-B -> OWUI-R
OWUI-R -> #13 only if promoted into the same release
```

## Integration boundary

Open WebUI is an external provider, not an inbound source for this feature.
Domain and use-case code must not depend on its HTTP API, provider wire-auth
format, or database model. The narrow boundary is:

- **Domain/use-case:** `WorkspaceDefinition`, `ModelDefinition`,
  `VirtualActor`, `OpenWebUIConversationLink`, `OpenWebUITurnLink`, local
  `reply_to_id`, and `OpenWebUITurnJob`. The MVP has one enabled workspace and
  one configured default model; model selection is never a client-supplied
  capability.
- **Provider port:** `StartChat(initial_message_sequence, model_id,
  idempotency_key, correlation_id)` for a new persistent chat and
  `ContinueTurn(remote_chat_id, message_sequence, new_turn, idempotency_key,
  correlation_id)` for a linked local branch. Each operation returns a
  buffered final result or an explicitly sequenced stream; completion is not
  a second provider operation layered on top of the same turn. `CancelTurn`
  is added only if the target provider proves safe cancellation. The names
  are domain operations, not unverified Open WebUI endpoint names.
  `ListChats`, `GetChat`, existing-chat import, history pull, and
  reconciliation are not part of this port.
- **Adapter:** absorbs Open WebUI endpoint/version and configured credential
  authentication differences. It verifies whether the first request creates
  a persistent chat, translates final/stream responses, and returns opaque
  remote IDs with redacted provider errors.
- **Local API/Aria projection:** exposes the local thread and default-model
  VirtualActor through the existing #6/#7 projections. Open WebUI credentials,
  remote chat IDs, and chat APIs never reach Aria.

The adapter must use a pinned Open WebUI API/version and a fixed HTTPS origin
allowlist. It must not discover or connect to an arbitrary URL supplied by a
user, and it must revalidate every redirect hop (or disable redirects) against
that allowlist and the resolved-IP SSRF policy. This feature's VirtualActor
is a local presentation of an external model; it does not add Misskey or
ActivityPub federation.
No Open WebUI source is copied into this repository; the contract is captured
with redacted fixtures and a narrow adapter. A target instance must not be
assumed to have a dedicated chat-creation endpoint: OWUI-C must pin the
endpoint, persistence conditions, chat/message ID return behavior, and save
completion before implementation. If that cannot be verified, disable the
feature; completion-only mode is not success for this persistent-chat goal.

## OWUI-C: contract and identity ADR

Placement: after #2 and before #4 schema work.

Acceptance criteria:

- [ ] Add `docs/decisions/0002-openwebui-boundary.md` covering the provider
  boundary, local source of truth, VirtualActor, non-federation scope, and
  auth/secret threat model.
- [ ] Add a pinned compatibility document covering target base URL policy, API
  version, outbound chat creation/continuation endpoints, request/response
  and stream fields, auth, errors, rate limits, nullable/unknown fields, and
  opaque chat/message ID semantics.
- [ ] Complete target-instance verification that the first request creates a
  persistent chat, returns a stable `chat_id` and response message ID when
  available, and provides the required default-model workspace permission.
  If persistence cannot be verified, disable this feature; completion-only is
  not a successful fallback for this roadmap.
- [ ] Store redacted fixtures for linear turns, optional remote
  `parentId`/`currentId` correlation, user/assistant/system/tool metadata,
  401/429, malformed responses, finish events, duplicate/out-of-order
  chunks, and response-loss ambiguity.
- [ ] Add an outbound-only ADR covering source of truth, new chat/continued
  turn lifecycle, branch policy, edit/delete policy, secret/notification
  boundary, and non-goals for existing-chat import/list/pull/reconciliation.
- [ ] Update #1/#2 traceability for #3, #4, #6, #8, #9, and #13 impacts;
  never treat Open WebUI credentials as MiAuth or local API tokens.

Implementation must not begin until the target Open WebUI version/API surface,
persistent chat creation/continuation endpoint and save semantics, real
workspace/default model IDs, workspace/model permissions, remote ID return
behavior, and credential provision/rotation ownership are recorded. These are
implementation start conditions, not blockers for publishing this roadmap.

## OWUI-P: local thread/link and identity projection

Placement: after #4 and #6; feature flag remains off by default.

The minimum registry model is:

### WorkspaceDefinition

- immutable local `id`
- owner-configured display `name`
- canonical HTTPS `base_url` (origin only; no userinfo, path, query, or
  fragment), restricted to the fixed Open WebUI origin allowlist
- `secret_ref`, never the provider credential itself
- `default_model_id` FK to a model in the same workspace
- fixed deployment-provisioned `presentation_host` (synthetic example:
  `openwebui.tail1a2b3c.ts.net`; never inferred from `base_url`)
- `enabled`, `generation_enabled`, timestamps, and remote chat
  creation/continuation capability status

### ModelDefinition

- immutable local `id` and `workspace_id`
- opaque `external_model_id`; never regenerate it from a display name
- `display_name`, `actor_id`, `active`, and verified `capabilities`
- optional provider `external_updated_at`
- default status is resolved by the owning workspace's `default_model_id`

`default_model_id` points to exactly one model in its workspace. Workspace
disable/delete and related actor/job behavior must be one transaction. A model
actor's stable local ID must survive display-name or handle changes. MVP
publishes only the default model as an actor.

### VirtualActor

The default model is a presentation actor, not a login-capable user:

- handle: `@model-slug@openwebui.tail1a2b3c.ts.net`
- host: `openwebui.tail1a2b3c.ts.net` (a fixed presentation value, not a
  federation or discovery target)
- stable local `actor_id`, `source=openwebui`, external model/workspace metadata
- `is_remote=true`, `is_loginable=false`, `can_miauth=false`,
  `can_own_secret=false`; it never becomes an owner, credential holder, or
  Aria account

The host is a fixed presentation/lookup value. The service does not discover
external actors. A workspace is a container, not normally a user; if a future
requirement needs a workspace actor, it must be a separate non-loginable actor
row rather than reusing the model actor.

Acceptance criteria:

- [ ] Add workspace/model/thread/turn-link migrations and narrow repository
  interfaces after OWUI-C and the #4 schema addendum.
- [ ] Enforce same-workspace default-model FK, unique external IDs,
  stable actor IDs, and enabled/disabled constraints.
- [ ] Connect VirtualActor to #6 entries and #7 `UserLite`/`Note` projections;
  exclude it from login and MiAuth paths.
- [ ] Enforce owner-only workspace changes, `secret_ref`, feature flags, and
  fixed presentation-host validation.
- [ ] Add thread-to-workspace, local `branch_id`, conversation, turn-link,
  revision, tombstone, and status migrations; keep remote IDs
  nullable/opaque and never use them for local identity, ordering, or
  authorization.
- [ ] Add conversation-link state-transition tests proving only `ready` can
  continue; only the owning `creation_pending` claim can issue its single
  initial `StartChat`, and `creation_pending`/`ambiguous`/`failed`/`dead`
  cannot issue another create, continue, or auto-retry.
- [ ] Add migration, serialization, unknown/null field, local reply-tree, and
  local stable-cursor contract tests.

## OWUI-B: outbound adapter, durable turn, and thread bridge

Placement: after #7, #8, and OWUI-P, in parallel with #9. OWUI-P may land its
local domain/storage pieces before #7, but this bridge cannot expose an Aria
action or assistant projection until the #7 wire layer exists. This replaces
the former import-oriented OWUI-S track.

### Local thread, message body, and reply tree

- An Aria `thread_id` is the conversation container and remains the local
  source of truth.
- An `OpenWebUIConversationLink` stores the local `thread_id`, a stable local
  `branch_id`, `workspace_id`, `default_model_id`, optional opaque
  `remote_chat_id`, state, optional `remote_current_id`, and timestamps. A
  remote chat maps to exactly one local thread within its workspace; a thread
  can have multiple links, but a local branch has at most one. Persistence
  enforces unique `(thread_id, branch_id)` and, for non-null IDs, unique
  `(workspace_id, remote_chat_id)` mappings. A remote chat never becomes the
  branch or thread identity.
- An `OpenWebUITurnLink` stores `local_message_id`, `local_parent_id`,
  `branch_id`, optional `remote_chat_id`, optional `remote_message_id`,
  optional `remote_parent_id` and `remote_current_id`, `request_id`, attempt,
  and provider status.

The conversation-link state machine is explicit:

```text
unlinked --claim--> creation_pending --confirmed--> ready
                         |                         |
                         |                         +--uncertain continuation--> ambiguous
                         +--definitive failure--> failed
                         +--uncertain/lost response or lease expiry--> ambiguous

ambiguous --owner confirms the same chat--> ready
ambiguous --owner abandons branch-------> dead
```

`unlinked` means that no outbound branch link has been claimed. The atomic
claim in step 3 creates `creation_pending` and assigns exactly one initial
`StartChat` operation to its durable job. That owning job may make that one
provider call while the link is pending; `creation_pending` is not a
retryable or re-claimable state. Only `ready`, with confirmed chat persistence
and a stable `remote_chat_id`, may receive `ContinueTurn`. A
`creation_pending` link permits only its already-claimed initial call; it and
`ambiguous`/`failed`/`dead` are not targets for another remote-chat creation,
another `StartChat`, a `ContinueTurn`, or automatic retry. Thus step 4's
initial `StartChat` is the claimed operation, not a second create from a
pending link.

A lost or uncertain creation response, including an expired lease before a
definitive result, transitions to `ambiguous`; restart recovery must freeze
the link rather than issue another `StartChat`. The state remains there until
explicit owner/operator recovery verifies the provider outcome. Recovery may
mark the same confirmed chat `ready` or deliberately abandon the branch and
start a separately audited local branch; it must never silently create a
second remote chat. A definitive creation failure is `failed`; a terminal
operator decision or exhausted, non-replayable operation is `dead`. These
states are independent of a definitive turn `failed` result after a `ready`
link: a known-good ready link may continue only when the individual
continuation is safe to retry. An uncertain continuation response transitions
the link to `ambiguous` and freezes it until recovery; a definitive failure
whose remote outcome is known may leave the link `ready` while marking only
the turn failed.

- Aria `reply_to_id` is the only source of truth for local parentage. Remote
  `chat_id`, `message.id`, `parentId`, and `currentId` are correlation metadata;
  they never replace local identity, ordering, authorization, or
  `reply_to_id`.
- `remote_chat_id` may be used only inside the adapter to address the linked
  outbound chat for continuation; it is not a local thread ID or a client
  capability.
- An owner-authored Aria root or reply with no existing outbound branch link
  starts one new persistent Open WebUI chat. For a reply, the eligible local
  root-to-parent path is sent as the initial message sequence; it is not an
  import or a claim about an existing remote chat. A linear owner reply to the
  current local head continues that branch's remote chat. All local entries
  remain in the same Aria thread.

The linear MVP example is `M0 -> A0 -> M1 -> A1`: `M0` is the owner root,
`A0` is a child from the default-model VirtualActor, `M1` replies to `A0`, and
`A1` replies to `M1`. A reply to an earlier node creates a new local
`branch_id` and, when the pinned contract supports the initial sequence, a
separate remote chat; it never activates a remote branch in the original chat.
Owner-authored bodies are immutable and authoritative;
assistant responses are separate `llm_reply` entries. System prompts, hidden
context, tool output, and provider debug data stay out of normal timeline
body. Model, usage, provider timestamp, finish reason, request ID, remote IDs,
and status are provenance metadata. Untrusted provider content is escaped for
rendering and never executed as instructions.

Each assistant child is inserted with local `reply_to_id` set to the triggering
owner message ID. A remote `parentId` or `currentId` can enrich correlation
metadata only; it never changes that local edge.

### Remote ID correlation

- `message.id`, when returned, is stored as `remote_message_id`; if absent,
  the local `request_id` remains the correlation key.
- `message.parentId`, when accepted or returned, is stored as
  `remote_parent_id`; the local parent is always `local_parent_id`/
  `reply_to_id`.
- `history.currentId` or `current_message_id`, when returned, is stored as
  `remote_current_id`; the local current head is selected from the Aria reply
  tree. A remote current mismatch fails closed and must not mix another branch
  into the same remote chat.
- Remote IDs are opaque nullable strings. They must not be exposed in Aria
  account tokens, public URLs, or used as a chronology cursor.

The provider message sequence contains only normalized messages selected from
the local path. Historical context is eligible only when an owner-authored
post has terminal local status `completed` and is not tombstoned, or a local
`llm_reply` has terminal status `completed`, `done=true`, and is not
tombstoned. For an owner-authored post, `completed` means that the local post
was durably accepted and validated; it is independent of the Open WebUI turn
or job status, so a provider outage or pending generation does not make the
owner's saved post disappear from eligible local history. `failed`,
`incomplete`, `streaming`, `pending`, `ambiguous`, `cancelled`,
dead/retry-exhausted, and deleted/tombstoned messages are never sent when
those statuses apply to the message itself. The newly accepted owner message
is passed separately as `new_turn` (and is appended to the initial sequence
for `StartChat`); it is the only current, non-historical message allowed in
the request. If any required path node is ineligible, the job fails closed
without silently skipping it or treating it as a root. The sequence carries
no local IDs,
Misskey metadata, credentials, system prompt, tool call, or provider debug
field; those remain local provenance or are excluded.

For `StartChat`, the request sequence is the eligible root-to-parent history
followed by `new_turn`. For `ContinueTurn`, the linked persistent chat is
addressed by its opaque remote IDs and `new_turn` is the current turn;
`message_sequence` is the already-validated local context supplied to the
provider port only as allowed by the pinned target contract. It must never be
filled by reading remote history, and the adapter must not blindly resend
already-persisted turns when the target contract does not require that form.

Whether the target API creates and saves a persistent chat on the first
request, lets the adapter continue it, accepts an initial message sequence and
parent/current fields, and returns message IDs is version-specific. OWUI-C
must verify these behaviors before OWUI-B starts; no direct Open WebUI
database access is permitted.

### Outbound order, cursor, and single-flight

This feature has no inbound Open WebUI cursor. It does not list, open, import,
pull, reconcile, or browse existing remote chats. Local timeline pagination
continues to use #6's stable cursor; remote IDs never determine chronology.

The send/receive order is:

1. Accept an owner-authenticated Aria create/reply request; generated
   VirtualActor entries cannot enqueue another turn.
2. Validate same-thread parentage, owner/model actor permissions, the selected
   branch, cycle, orphan, and eligible-message rules.
3. Commit the local message, a `creation_pending` link for a new branch when
   needed, and the durable turn-job intent atomically.
4. Under a thread-level lease/single-flight, execute the one already-claimed
   `StartChat` for a `creation_pending` branch, or resolve the linked branch
   for continuation. A continuation is eligible for this step only when its
   link is `ready`; every other link state fails closed without a provider
   call. No worker may issue an initial create unless it owns the corresponding
   `creation_pending` claim.
5. Send the initial sequence through `StartChat`, or the new owner turn through
   `ContinueTurn`, using the configured default model and turn idempotency key.
6. Receive a buffered final response or ordered stream events.
7. Save the assistant child, remote correlation metadata, provenance, and
   final job status atomically; provider failure never removes the local post.

The selected local path follows `reply_to_id` from the chosen parent to the
root, then sends root-to-parent order followed by the new owner turn. A
visited set rejects a self-reference or any pre-existing cycle before a
provider call. An unknown parent is rejected as an invalid Aria request before
the local post is inserted; a persisted dangling edge discovered during
restart or repair is quarantined and never treated as an implicit root. Each
local branch has one remote chat in the MVP; a concurrent request in the same
thread is pending/conflict/retry according to local policy, never an
unbounded concurrent remote turn. Before the provider call, the worker
revalidates that every historical path message is an eligible completed
owner/assistant message and rejects the turn if a tombstone, failed,
incomplete, or streaming message would otherwise be resent.

### Idempotency and failure boundary

- Logical turn key: `thread_id + branch_id + local_message_id +
  selected_parent_id + model_id + revision`. It is unique for the lifetime of
  that logical turn. `attempt` is retry state (or an attempt record) attached
  to the same job, not part of job identity.
- A duplicate delivery of the same durable job returns or reuses the existing
  job/result and does not repeat a provider call. A new user-requested
  generation must use a new revision/key and is not a duplicate.
- One logical request has one local assistant entry. Stream chunks append to
  that entry with sequence/checkpoint deduplication.
- Response upsert uses a unique local `request_id` and compare-and-set. The
  provider idempotency key is derived from the logical turn key and is never a
  substitute for local identity. If a
  remote ID exists, `remote_chat_id + remote_message_id` is also checked; a
  remote ID attached to different local requests is a contract failure.
- A provider idempotency key or result lookup is required before automatic
  replay of an ambiguous completion. Without it, timeout or connection loss
  is `ambiguous/dead` and requires an explicit owner recovery action.
- Loss of the initial chat-creation response must not automatically create a
  second remote chat. It is also `ambiguous/dead` until manually resolved.
- Parent self-reference, an ancestor cycle, an unknown parent, a parent in
  another thread, or a parent owned by another principal is rejected before
  sending. Stale branch responses do not move the local current head.

The Aria v1.5.11 `notes/create` contract has no documented idempotency field.
Therefore this keying guarantees idempotency for a committed local message
and its durable outbound job, including duplicate worker delivery; it must
not claim to deduplicate two independent HTTP `notes/create` requests. Adding
an Aria-compatible idempotency header or body field requires a separate wire
contract and test.

Manual resolution of an ambiguous remote result is an explicit owner/operator
action (for example, mark the local branch unrecoverable after out-of-band
provider verification). It never means silently listing, opening, importing,
or pulling remote chats through this feature.

The local post is always durable before any provider call. Remote success or
failure is an asynchronous result and cannot determine whether the Aria post
exists.

### Acceptance criteria

- [ ] Add an `OpenWebUIProvider` port and HTTP adapter implementing
  `StartChat` and `ContinueTurn` with a pinned credential mechanism, timeout,
  redirect-hop, request/message/response-size bounds, TLS/origin,
  cancellation, and redacted-error handling in one boundary. The adapter must
  not expose unverified endpoint names as a domain contract.
- [ ] Add an `openwebui_turn` durable job using #8 leases, bounded retry/dead
  state for safe ready continuations, explicit terminal creation states,
  restart recovery, thread single-flight, and local idempotency.
- [ ] Implement local reply-tree path construction, new-chat versus
  continuation, one-new-chat-per-local-branch policy, assistant-child
  mapping, remote correlation metadata, provenance, partial/failure status,
  and no source-text overwrite.
- [ ] Implement owner action -> local post -> durable job -> outbound chat
  turn -> assistant entry E2E; local post success must not depend on provider
  success.
- [ ] Test linear ordering, independent branch chat creation or explicit
  unsupported-branch rejection, duplicates, response-loss ambiguity, cycles,
  orphans, stale branches, concurrent turns, outage, auth failure, rate limit,
  malformed response, schema drift, state-transition guards, and
  cancellation.

## OWUI-R: optional release gate

Placement: attached to #13 only when the outbound feature is promoted into
the same release. It never blocks the original #1 MVP while the feature flag
is off.

Acceptance criteria:

- [ ] Feature-off regression proves the existing #1 auth, post, reply, thread,
  and source-ingestion behavior is unchanged.
- [ ] Owner root → assistant → follow-up → assistant is restored after
  restart, with the same local thread and local `reply_to_id` tree.
- [ ] A reply to an earlier local ancestor remains a sibling in that same
  local thread and uses a distinct remote chat; no remote branch is silently
  mixed into the linear chat.
- [ ] Provider outage still saves the Aria post; duplicate delivery creates no
  second assistant entry or remote chat.
- [ ] Initial remote chat creation response loss becomes `ambiguous` (and may
  become `dead` only through explicit recovery) and never silently creates a
  duplicate chat; no new creation, continuation, or automatic retry occurs
  until owner/operator recovery.
- [ ] Target-instance evidence covers persistent chat creation/continuation,
  default-model permission, completion finish, and any enabled stream finish.
- [ ] Raw provider credentials, session capabilities, cookies, prompts, and
  stream chunks are absent from logs, traces, fixtures, and error responses.
  Encrypted,
  access-controlled backups may contain the local post/assistant bodies and
  opaque remote IDs required for restart and continuation, but never raw
  provider secrets.
- [ ] Same-remote-chat branch management, regeneration, provider edit/delete,
  existing-chat import/list/pull, and history-browsing behavior is not
  advertised as successful API/UI capability.

## Auth, permission, and secret boundary

The authentication and execution boundaries remain distinct:

1. local owner MiAuth/session and local API token used by Aria;
2. workspace-scoped Open WebUI credential/API key;
3. default-model VirtualActor, which is presentation-only and not an
   authentication principal;
4. the server-side `OpenWebUITurnJob` worker identity.

The local owner must already be authenticated through the #2 MiAuth/local API
token boundary. An Open WebUI credential cannot authenticate Aria, create a
local owner, mint a local API token, or expand the existing local API scopes.
Open WebUI calls are made only by the server-side adapter with the
workspace-scoped credential.

Authentication authorizes enqueueing only. The durable job stores the local
owner ID and correlation data, never the Aria token, MiAuth route/state, or raw
Open WebUI credential. At execution time the worker resolves `secret_ref`
through the adapter's secret store and rechecks that the workspace, model,
feature flag, and owner policy still permit the turn.

The completion bridge always uses the enabled workspace's configured
`default_model_id`; no Aria request may select an arbitrary workspace, model,
remote chat ID, or parent ID. The server resolves the local thread/link and
parent path, and the adapter verifies that the model belongs to the workspace
before any provider call.

Open WebUI uses a dedicated low-privilege workspace account/credential. Only a
`secret_ref` crosses the domain/config boundary; the raw provider credential is
isolated in the adapter/secret store and is never written to the database,
fixtures, URL, error body, logs, traces, backups, or planning documents.
Rotation/revocation and the responsible operator are documented before
enabling the feature.
401/403 are permanent `auth_failed` states until reauthentication, permission
correction, or rotation; they are not blindly retried. Remote chat/message
bodies, prompts, stream chunks, provider request IDs, and local correlation
IDs are excluded from diagnostics unless a bounded opaque ID is required for
correlation; request/response bodies are never logged.

Default permissions:

- workspace configuration, credential rotation, model refresh, bridge
  triggering, and status viewing are owner-only;
- local create/reply, thread reload, and timeline access follow the existing
  #1/#5/#7 owner policy;
- VirtualActor cannot log in, use MiAuth, own credentials, or perform remote
  actions;
- arbitrary workspace/model/remote-chat switching and tool/function/MCP
  execution are outside MVP;
- user-supplied base URL, host, redirect, and callback are never used as-is;
  adapter validation enforces HTTPS, fixed origin allowlist, redirect-hop
  revalidation (or redirects disabled), timeout, response-size limits, and
  private-IP/localhost rejection at connection time. A Tailnet address is
  permitted only as an explicit deployment-provisioned origin in the fixed
  allowlist; arbitrary private addresses remain rejected.

Notifications are deliberately narrow: local owner-post persistence is
independent of provider success; completion updates the same thread's local
assistant entry and job status; and MVP uses Aria poll/reload rather than a
required streaming/WebSocket notification. There is no VirtualActor
notification, remote federation notification, or other-user fan-out. An
`auth_failed`, `ambiguous`, or `contract_failed` result is owner-only
status/system metadata, never a model response or implicit mention/unread
notification.

Tailnet reachability is a deployment prerequisite, not application
authorization. Network identity cannot replace owner permission checks.

## Outbound job, retry, and error contract

This feature has no inbound Open WebUI synchronization. There is no
`ListChats`, `GetChat`, remote history cursor, existing-chat reconciliation, or
remote edit/delete pull. The only durable job is an owner-triggered outbound
turn on #8's SQLite-backed lease/retry infrastructure.

- The local post and `OpenWebUITurnJob` intent commit atomically before the
  provider call.
- A thread-level lease/single-flight serializes the active turn across local
  branches.
  Expired leases recover after restart without losing the local post.
- The local turn key and unique `request_id` suppress duplicate local jobs and
  assistant entries. Stream chunks append to the same entry by sequence and
  checkpoint.
- A cursor is used only by the existing local #6 timeline. Remote IDs and
  provider timestamps never become a chronology cursor for this feature.
- Workspace capability status distinguishes `healthy`, `degraded`, and
  `auth_failed`; turn/job state distinguishes `pending`, `running`,
  `streaming`, `completed`, `failed`, `ambiguous`, `contract_failed`,
  `cancelled`, and `retry_exhausted`/dead.

Error behavior:

- 401/403: no blind retry; mark `auth_failed` and require owner
  reauthentication, permission correction, or credential rotation.
- Invalid model/request 4xx: permanent failure until configuration changes;
  do not retry automatically.
- 429/5xx/network timeout/disconnect: automatic retry is allowed only when the
  pinned provider contract supplies a safe idempotency key or result lookup.
  Honor a valid `Retry-After` only within a configured maximum delay and use a
  bounded retry count; otherwise mark an ambiguous outcome dead for explicit
  owner recovery.
- This retry rule applies only to a `ready` link's safe continuation. An
  initial `StartChat` is never automatically replayed: a definitive failure
  becomes `failed`, while a timeout, disconnect, or response loss becomes
  `ambiguous` (and may later be made `dead` only by explicit recovery).
- Loss of the response after remote chat creation is ambiguous and must not
  automatically create a second chat. The same applies to an uncertain
  completion response.
- Malformed/unknown response or stream event is `contract_failed`; retain only
  redacted payload metadata and metrics.
- Streaming disconnect, timeout, or late chunk leaves the assistant entry
  incomplete/failed and never silently marks it done or creates a second entry.
- Process context cancellation stops the in-flight provider operation before
  making the lease reclaimable; it does not roll back the already committed
  local post. An explicit owner cancellation is terminal only when the
  provider's cancellation safety is verified.

## Follow-up branch, regeneration, edit, delete, and streaming lifecycle

### Branches

Aria may create a sibling by replying to any local ancestor. The local tree
keeps both `M0 -> A0 -> M1 -> A1` and `M0 -> M1b -> A1b`; `A1b` is a new child,
not an update to `A1`. A reply to the current local head continues its branch;
a reply to an earlier node creates a new local branch and a separate remote
chat seeded with that branch's root-to-parent sequence, once OWUI-C has
verified that initial-sequence persistence. This avoids remote branch
management and prevents `currentId` from mixing branches in one remote chat.
If that target behavior cannot be verified, the feature stays disabled (or
the branch action is an explicit unsupported error); it must not silently send
an incomplete context or claim a persistent chat.

### Regeneration

Regeneration is a later feature and is not advertised through the Issue #2
Aria surface. A future regeneration for the same local parent must create a
new revision/branch and a separate remote chat; it never overwrites the
original assistant. A durable-job retry is different: it retains the same
logical turn key and is automatic only when provider idempotency or result
lookup makes replay safe. Otherwise an ambiguous/dead outcome requires an
explicit owner recovery action.

### Edit and delete

Provider-side edit/delete calls and inbound reconciliation are non-goals. The
Issue #2-compatible `notes/update` path is not implemented and must return an
explicit unsupported error. If a later local edit feature is added, it must
create an immutable new revision/branch and optional new outbound turn; the
already-sent body remains unchanged and no provider edit is attempted. A
future local delete is a soft delete/tombstone that preserves reply edges and
provenance. A deleted message is not silently reused as a fresh context root;
local policy chooses a new branch or rejects the request. Assistant
revisions/tombstones are local only.

### Streaming lifecycle

Buffered final response is the MVP default. If streaming is enabled after the
target contract is verified, the lifecycle is:

1. Commit owner message and durable job intent.
2. In a transaction before the provider call, create one assistant-child
   placeholder linked to the request with `status=streaming` and `done=false`.
3. Append provider chunks to that entry by local request ID and the provider's
   verified sequence/checkpoint. Duplicate chunks are idempotent; an
   out-of-order or sequence-less stream is rejected or disables streaming for
   that provider version.
4. On a valid finish event, compare-and-set the non-terminal entry and
   atomically persist body, usage, finish reason, remote metadata, and
   `done=true`. Late chunks cannot reopen a terminal entry.
5. Let Aria observe the placeholder through poll/reload; streaming/WebSocket
   is not required for notification.
6. On disconnect, malformed event, timeout, cancellation, or late chunk,
   mark the entry incomplete/failed (or cancelled for an explicit owner
   cancellation) and never mark it done silently.
7. Retry only when remote idempotency/result lookup makes replay safe; never
   create a second assistant entry for one local request.

System prompts, tool calls, hidden context, and chain-of-thought remain out of
the timeline even if they appear in a provider stream. `CancelTurn` is not
implemented unless the target provider proves cancellation safety. On process
shutdown, transport cancellation must finish before the lease is made
reclaimable; releasing a live lease could issue a duplicate remote turn.

## MVP and non-goals

### Included when enabled

- one fixed, allowlisted workspace;
- one owner-selected default model;
- one non-loginable VirtualActor projection;
- an Aria owner root or reply without an existing outbound branch link starts a
  new persistent Open WebUI chat only after the target contract proves
  creation, initial-sequence, and save semantics;
- a linear Aria owner reply to the current local head continues that branch's
  remote chat, while a reply to an earlier node uses a new remote chat for its
  new local branch;
- Aria local messages and assistant responses remain in the same local thread,
  with local `reply_to_id` as the reply-tree source of truth;
- opaque remote chat/message/parent/current IDs stored only as correlation
  metadata;
- atomic local post plus durable turn-job intent;
- buffered final response by default, or streaming only when the target
  contract and lifecycle are verified safely;
- duplicate request, response-loss ambiguity, cycle, orphan, stale branch,
  single-flight, auth/rate-limit, malformed response, restart, and
  credential-redaction handling/tests.

### Excluded

- opening, browsing, listing, searching, importing, or pulling existing
  Open WebUI chats/history/messages;
- `ListChats`, `GetChat`, remote cursor sync, existing-chat reconciliation, or
  remote deletion-event ingestion;
- converting an existing Open WebUI message tree into an Aria reply tree;
- automatic discovery or management of all Open WebUI workspaces/models;
- Misskey/ActivityPub federation, remote discovery, signatures,
  inbox/outbox, or remote callbacks;
- tool/function/MCP execution or autonomous agent loops;
- arbitrary Open WebUI URLs or unrestricted tenant switching;
- complete bidirectional local/remote edit/delete synchronization;
- same-remote-chat full branch management, remote branch replay, and
  regeneration until a later pinned contract and policy;
- provider-side edit/delete calls; local edits/deletes are later local
  revision/tombstone behavior only;
- required streaming, unlimited attachment-body ingestion, or system-prompt
  publication;
- multi-user/role model, PostgreSQL, and custom UI.

The feature must preserve #1's single-owner, non-federation, and
non-tool-execution boundaries. Shared provider code with #9 may be reused,
but outbound chat creation/continuation and VirtualActor remain behind a
separate feature flag and release gate. Existing Open WebUI chat import and
pull-sync code is deliberately not a deferred TODO; it is outside this track.

## Issue #1/#2 traceability

| Existing requirement | Outbound feature impact | Owner / dependency |
| --- | --- | --- |
| #2 local/upstream MiAuth and token separation | Open WebUI credentials never authenticate Aria, bind an owner, or mint a local token | #2 → OWUI-C; authentication boundary unchanged |
| #1 local post survives LLM/provider outage | Save Aria post and `OpenWebUITurnJob` intent atomically before any remote call | #4 + #8 + OWUI-B |
| #1 thread/reply/restart behavior | Local `reply_to_id` and `thread_id` own the tree; remote IDs are metadata | #6 + OWUI-P + OWUI-B |
| #1 LLM reply/follow-up remains separate | Default-model VirtualActor writes a separate assistant child; source text is immutable | #9 + OWUI-B, feature off by default |
| #1 release/security/E2E gate | Outbound root/reply, ambiguity, restart, secret redaction, and notification policy are opt-in evidence | OWUI-R → #13 only on release promotion |
| #1 non-goals | Existing-chat open/import/list/pull/reconciliation/history browsing, federation, tools, custom UI, and multi-user behavior stay excluded | #1/#2 boundary; no feature dependency |

## Implementation start conditions

The first implementation item is OWUI-C: freeze the outbound-only
boundary/compatibility contract, identity ADR, and redacted fixtures before
writing migrations or adapters. Start only after these are known:

- target Open WebUI version and API surface;
- concrete persistent-chat creation and continuation endpoint, including
  first-request creation, initial message-sequence handling, save completion,
  and response-loss semantics;
- real workspace/default-model IDs and permissions;
- whether remote `chat_id`/`message.id`/`parentId`/`currentId` are returned or
  accepted, and the local one-chat-per-branch policy independent of remote
  branch controls;
- credential provisioning, rotation, and revocation owner;
- fixed HTTPS base URL and presentation-host allowlist;
- request/response/stream size, timeout, cancellation, and rate-limit bounds.

If any target-instance contract is unverified, do not emulate success. Keep
the feature disabled; completion-only mode does not satisfy the persistent-chat
MVP goal.

## Verification and release gate

- **Existing regression:** `gofmt`, `go test ./...`, `go vet ./...`, and
  `go test -race ./...` where concurrency/jobs are changed; feature flag off
  must leave #1 auth, posts, threads, and source ingestion unchanged.
- **Contract:** fixture-only creation/continuation, nullable/unknown/error,
  response-loss, finish-event, stream-order, API-version drift, rate-limit,
  and redaction tests with no credentials or network.
- **Identity:** stable actor ID across display-name/handle changes,
  disabled/default switches, same external model IDs, and rejection of
  VirtualActor login/MiAuth.
- **Thread:** root→assistant→reply→assistant local `reply_to_id` tree,
  selected-path ordering, independent branch chat mapping, duplicate delivery,
  cycles, orphans, stale branch, local revision/tombstone, and restart
  persistence.
- **Jobs:** outage, 429/5xx, 401/403, timeout, ambiguous create/completion,
  malformed response, lease expiry, restart, retry exhaustion, single-flight,
  cancellation, and partial stream.
- **Security:** arbitrary base URL/redirect/host, private-IP/SSRF, oversized
  payload, key/session/cookie/prompt/body leakage in logs/traces/fixtures or
  error responses, unauthorized backup access to local bodies/remote IDs,
  owner-only permission, arbitrary remote identifiers, and no tool/function/
  MCP execution.
- **Notification:** local post success independent of provider result, no
  implicit mention/unread or VirtualActor fan-out, and owner-only
  auth/ambiguous/contract status metadata.
- **Opt-in Aria E2E:** owner-only root/reply, default-model display, same-thread
  reload/restart, duplicate request, response-loss recovery, rotation,
  permission denial, and provider outage against the target instance only
  when explicitly enabled.

Only if the feature is promoted into the same release does #13 require the
OWUI-R E2E/operations runbook, backup/restore, rotation, and security gate.

## References

- Open WebUI API reference: <https://docs.openwebui.com/reference/api-endpoints/>
- Open WebUI API keys: <https://docs.openwebui.com/features/authentication-access/api-keys/>
