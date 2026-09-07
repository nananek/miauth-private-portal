# ADR-0005: Open WebUI outbound boundary, chat lifecycle, and identity

- Status: Accepted for Issue #51 (OWUI-C)
- Date: 2026-09-06
- Scope: Issues #50–#54 (umbrella #50; OWUI-C #51, OWUI-P #52, OWUI-B #53,
  OWUI-R #54); amended for Issue #72 (opt-in `features`/`tool_ids`), Issue #74
  (native-FC/legacy fix, startup tool/model resolution), and Issue #75
  (multi-model catalog sync, mention-based model selection, per-model tool
  resolution — filed under the same umbrella #50, extending #52/#53)

## Context

[`docs/roadmap/openwebui.md`](../roadmap/openwebui.md) proposes an opt-in
outbound bridge: an Aria root message or reply starts (or continues) a
persistent Open WebUI chat, and the model's answer comes back as a child in the
same Aria thread. The Aria `reply_to_id` tree stays the conversation's source
of truth; Open WebUI identifiers are correlation metadata. Reading, listing,
importing, or reconciling existing Open WebUI chats is out of scope, as is
federation and any custom UI. This service never executes a tool, runs an
MCP server, or interprets a tool call itself (Issue #72); it may only ask
the target Open WebUI instance — a separately administered service the
owner already trusts — to use its own configured tools via request-level
`features`/`tool_ids` flags, the same way it already asks for a persistent
chat or a continuation.

The roadmap deliberately refused to assume the target's API shape. Its
"Implementation start conditions" require the version, the persistent-chat
creation and continuation endpoints, save semantics, response-loss behavior,
model IDs and permissions, and credential ownership to be **recorded** before
any migration or adapter is written, and it explicitly warns that a dedicated
chat-creation endpoint must not be assumed to exist. It also states that if
persistence cannot be verified, the feature stays disabled — completion-only
mode is not an acceptable fallback for this goal.

Those conditions have now been checked against a real instance. On 2026-09-06
the pinned image
`ghcr.io/open-webui/open-webui@sha256:1a6399d237dc392a2313e0ca826020b3fd5d22536357840eb63393d18dc8b924`
(`GET /api/version` → `0.11.3`) was run in Docker against a purpose-built
OpenAI-compatible mock backend, with no production instance, real credential,
or third-party network involved. The full record — endpoint by endpoint, with
redacted fixtures — is
[`docs/compat/openwebui-0.11.3.md`](../compat/openwebui-0.11.3.md); this ADR
records the decisions that follow from it.

Answering the roadmap's central caution directly: **a dedicated chat-creation
endpoint does exist** (`POST /api/v1/chats/new`), and a completions call on its
own does **not** create a chat unless the request opts into chat management.
Persistence, `chat_id`/message-id return behavior, and save completion are all
verified, so the feature is not blocked on this ground.

The MVP this depends on (#2–#13, #28) is complete, so the roadmap's original
"Phase 0.5, before #4" placement is moot; the remaining ordering is simply
#51 → #52 → #53 → #54.

## Decision

### D1. The provider port keeps the roadmap's domain-level shape

The port stays `StartChat(initial_message_sequence, model_id, idempotency_key,
correlation_id)` and `ContinueTurn(remote_chat_id, message_sequence, new_turn,
idempotency_key, correlation_id)`. Only the adapter knows Open WebUI endpoint
names, and no endpoint name is promoted into a domain contract.

`CancelTurn` is **not** added. `POST /api/tasks/chat/{chat_id}/stop` exists,
but it is only meaningful for background (streaming) generation, which is out
of scope, and its safety was not verified. The roadmap already conditions
`CancelTurn` on proven cancellation safety, so the condition simply is not met.

**Addendum (2026-09-06, Issue #53).** The two operations above are kept, and
the bridge implementation added two things to the port that it could not be
built without. Neither promotes an endpoint name into the domain contract:

- `StartChat` takes an `OnChatCreated(ctx, remoteChatID)` callback, which the
  adapter invokes after `POST /api/v1/chats/new` has returned the
  server-assigned id and *before* the first completion is requested. The
  bridge uses it to persist `remote_chat_id` and move the link from
  `creation_pending` to `ready`. "Confirmed" in the roadmap's state machine
  therefore means "the chat exists with a stable id" — exactly what D3's
  pre-creation step establishes — and the first assistant turn is an ordinary
  turn on a ready link. Without this hook, a completion response lost *after*
  a successful creation would have to be treated as a lost creation and frozen
  as `ambiguous`, even though its outcome is recoverable (see the next point).
- `LookupTurnOutcome(ctx, remoteChatID, assistantMessageID)` is the "result
  lookup" the roadmap (§"Idempotency and failure boundary") requires before
  any automatic replay of an uncertain completion. It reads
  `GET /api/v1/chats/{id}` on the chat this bridge itself created and returns
  only `done`, whether an `error` object is present, `content`, `currentId`,
  and usage for the one assistant message the bridge itself named. It is not
  `GetChat`: no other message, no title, and no history is returned or
  retained, so the roadmap's exclusion of `ListChats`/`GetChat`/history pull
  stands. The adapter never decodes `error.content` (D6).

The creation call has no client-generated id to look up, and finding a lost
chat would mean listing chats, which is excluded — so a lost creation
response remains `ambiguous` until owner recovery, as D7 requires.

### D2. Local identifiers are the only source of truth

Aria's `thread_id` and `reply_to_id` own the conversation. `chat_id`, message
ids, `parentId`, and `currentId` are opaque, nullable correlation strings.
They are never used for local identity, ordering, authorization, or pagination,
and never surface in Aria payloads, tokens, or URLs.

### D3. Pre-create the chat, then run each turn as a buffered completion

`StartChat` is two calls:

1. `POST /api/v1/chats/new` with an **empty** history. The server assigns the
   chat id, which becomes `remote_chat_id` and is therefore known and
   persistable *before* any generation is triggered.
2. `POST /api/chat/completions` with `stream:false`, `chat_id`, `parent_id:
   null`, a client-generated `user_message` (`id`, `parentId`, `childrenIds`,
   `role`, `content`, `timestamp`), `id` for the assistant message, the
   locally assembled `messages` context, and `background_tasks` with all three
   generation flags off.

`ContinueTurn` is the same second call with `parent_id` **and**
`user_message.parentId` both set to the previous assistant message id.

Message ids for both the user and the assistant message are **client-generated
UUIDs**; the server stores them verbatim. The completion response's `id`
(`chatcmpl-…`) belongs to the upstream model provider and is never stored as a
remote message id.

Turn completion is confirmed by `GET /api/v1/chats/{id}` and reading
`chat.history.messages[<assistant id>].done` and `.content`, with
`chat.history.currentId` (mirrored by `current_message_id`) recorded as
`remote_current_id` **only once that `done` is true**. The server advances
`currentId` onto a failed assistant message too, so recording it unconditionally
would parent the next turn on a node that holds an error and no content.

The alternative — letting a completions call create the chat implicitly — is
rejected: it works, but the buffered response contains no `chat_id` anywhere,
so the id could only be recovered by diffing the chat list, which is exactly
the remote-history reading this feature refuses to do. Pre-creation also means
a lost response can be investigated with a targeted `GET` instead of becoming
unrecoverable.

### D4. The server validates nothing about the tree, so we must

Open WebUI accepts a `parentId` naming a message that does not exist and
stores it dangling, with HTTP 200. It does not rebuild context from stored
history either: `messages` is passed to the upstream model exactly as sent,
with no system prompt added and no model substitution. Setting `parent_id`
without `user_message.parentId` silently produces an orphan pair.

One consequence is a rule the adapter must follow rather than merely a caution:
**every completions request carries the `parent_id` key.** A request that omits
it while still naming `chat_id` and an existing message `id` is not a harmless
"generate only" call — it writes the generated content into that message and
resets its `parentId` to `null`, destroying a link the client had already
stored.

Therefore every eligibility, path, cycle, orphan, and ordering rule in the
roadmap is enforced **locally, fail-closed, before the provider call**. The
remote side is treated as a store that will faithfully persist whatever it is
told, including nonsense.

### D5. One local branch maps to one remote chat

Open WebUI supports siblings under one parent inside a single chat, and
`POST /api/v1/chats/{id}/fork` will copy a `parentId` chain into a new chat.
Neither is used. A reply to an earlier node starts a **new** `StartChat` whose
initial sequence is the locally reconstructed root-to-parent history.

Both remote alternatives require reading or duplicating remote history, and
sharing one remote chat across branches would let `currentId` mix branches.
Rebuilding the context locally keeps the local tree authoritative and costs
only a message list this service already has.

### D6. Classify errors from the chat state, never from the HTTP status

Two failure surfaces were observed, and neither is usable as-is:

- **Legacy path** (no `parent_id` key): upstream 401, 429, and 500 all arrive
  as HTTP **400** with the upstream's own error text in `detail`.
  `Retry-After` is not propagated. A non-JSON upstream response arrives as
  HTTP **200** whose body is a bare JSON string.
- **Chat-managed path** (the one this integration uses): a failure arrives as
  HTTP **200** with a `null` body, while the user message is already persisted
  and the assistant message carries `done:false`, `content:""`, and
  `error:{content}`.

So the adapter's rule is:

| Observation | Outcome |
| --- | --- |
| 200 with a decodable completion | success (still confirmed by `GET`) |
| 200 `null` → `GET` shows `error` | `failed` (category only; text discarded) |
| 200 `null` → `GET` shows `done:false`, no `error` | `ambiguous` |
| 200 `null` → `GET` shows `done:true` | success |
| body is a JSON string or otherwise undecodable | `contract_failed` |
| 401 | `auth_failed`, never retried blindly |

Provider error text is **always discarded** and replaced with a local category.
Open WebUI performs no redaction of its own: the mock's pseudo-secret
`sk-mock-upstream-secret` appears verbatim in both `detail` and the stored
`error.content`. That string is kept in the fixtures on purpose so that #53 can
assert it never reaches a log or an error response.

### D7. Idempotency and single-flight are entirely local

Open WebUI has no idempotency key. Re-sending a turn with identical ids calls
the model again and **overwrites** the assistant message's content, leaving the
node count unchanged — so a duplicate is silently destructive rather than
rejected.

The roadmap's local rules therefore stand unchanged and are load-bearing: a
`creation_pending` `StartChat` is never automatically replayed; only a `ready`
link's continuation may be retried within a bounded count; an `ambiguous`
outcome needs an explicit owner recovery action. The `idempotency_key` in the
port signature stays a local correlation value, not a provider feature.

**Addendum (2026-09-06, Issue #53).** A continuation on a `ready` link may be
retried within the job's bounded attempt count because two verified facts
make the replay safe: re-sending a turn with the same client-generated ids
overwrites the assistant node in place rather than adding one (compat
§"POST /api/chat/completions" (e)), and `LookupTurnOutcome` (D1 addendum)
lets the worker read the earlier attempt's outcome first, so a turn that did
complete is adopted rather than regenerated. A `creation_pending`
`StartChat` is still never replayed. Single-flight per thread is an
in-process lock in the worker (this deployment runs one worker process); the
job's own lease and the "attempt recorded before the call" rule cover restart.

### D8. Streaming stays out of the MVP

For a chat-managed request, `stream:true` returns either
`{"status":true,"task_ids":[…],"chat_id":…}` (with `session_id`) or a literal
`null` (without), and the chunks are delivered over socket.io, not HTTP.
The only true HTTP SSE path is the legacy one, which persists nothing — and
its chunks carry no sequence number or per-chunk id, so the roadmap's required
duplicate/out-of-order detection is impossible on the wire. Streaming also
*appends* to an existing message where buffered mode replaces it (observed
during the capture session; no fixture of that state was retained — see the
compat document's streaming table).

Buffered is the default and the only supported mode. Streaming needs a
socket.io client and a separate contract; it is a future issue, not a TODO
inside #53.

### D9. Identity projection follows the roadmap, with the registry terms pinned

`VirtualActor`, `is_loginable=false`, `can_miauth=false`, `can_own_secret=false`,
and the `@model-slug@<presentation_host>` handle are unchanged. Two terms are
pinned to what the target actually has:

- Open WebUI has **no** "workspace" API object. The roadmap's
  `WorkspaceDefinition` corresponds to one instance (base URL) plus one
  account (credential); the product's own "Workspace" screen is unrelated
  model/prompt/tool management.
- `external_model_id` is a `data[].id` from `GET /api/models` (a connection's
  base model) or a workspace custom-model id from `GET /api/v1/models`. It is
  opaque and is never regenerated from a display name.

`presentation_host` remains a deployment-provisioned value, never inferred
from the base URL. **TBD (see #50):** its concrete value.

### D10. Credentials are Open WebUI API keys, held the way this repo already holds secrets

The adapter authenticates with a dedicated account's API key
(`Authorization: Bearer sk-…`). Enabling keys requires **both** the admin
setting `ENABLE_API_KEYS=true` and the default permission
`features.api_keys=true`. An account holds exactly one key, and re-issuing
invalidates the previous one immediately, so rotation is re-issue plus config
update with **no overlap window**.

An Open WebUI credential is not an Aria credential: it cannot authenticate
Aria, create a local owner, mint a local API token, or widen a local scope
(roadmap §"Auth, permission, and secret boundary", #28 traceability).

**`secret_ref` interpretation — decided by the owner on 2026-09-06.** The
Open WebUI API key is held exactly the way this repository already holds
third-party secrets: as a typed config value under a key such as
`OPENWEBUI_API_KEY`, supplied through the ordinary config source (`.env` on a
bare host; the container's environment-variable source otherwise) and reported
by `internal/config.Config.Redacted()` only as set/unset — the same treatment
`LLM_API_KEY` and `IMAP_PASSWORD` get. The roadmap's `secret_ref` therefore
means *"a reference to a config key"*, and **no separate secret store or
secret-management subsystem is built**: neither #52's schema nor #53's adapter
introduces one, and the raw key never reaches the database, a domain object, or
a fixture. The key referenced is one declared in `internal/config`'s schema,
not a free-form environment-variable name — `Load` reads only the known keys,
by name, and never scans the environment
([`docs/operations/configuration.md`](../operations/configuration.md#why-environment-variable-scanning-is-scoped-to-known-keys)).
The adapter then reads that value from the typed `Config` it is given, not
from the process environment itself: an `os.Getenv` shortcut would bypass
`Load`'s validation and would miss the config file a bare-host `.env`
deployment relies on.

Rotation follows the existing procedure in
[`docs/operations/runbook.md`](../operations/runbook.md#secret-rotation) —
replace the config value and restart, never an in-place update through an API.
Because re-issuing an Open WebUI key invalidates the previous one immediately
(no overlap window, as above), rotation belongs in a maintenance window; the
runbook already records that same caveat for `LLM_API_KEY`.

**TBD (see #50):** who provisions the dedicated account and who owns rotation.
Where the key lives is no longer open — it is a config key, per the decision
above.

### D11. Network policy mirrors the existing SSRF boundary

A fixed HTTPS origin allowlist, the resolved-IP SSRF policy already
implemented in `internal/ingest/safehttp`, and **redirects disabled** (simpler
than revalidating every hop against the allowlist). A Tailnet origin is
permitted only when it is explicitly listed. Because no server-side rate limit,
size bound, or timeout could be observed, the adapter imposes its own
client-side timeout and response-size bound rather than relying on the target's.

**TBD (see #50):** the allowlisted origin(s) and the concrete bounds.

### D12. Remote edit, delete, and regeneration are not implemented

`POST /api/v1/chats/{id}` (update), `POST /api/v1/chats/{id}/messages/{message_id}`,
`.../event`, `/fork`, `/clone`, and `/compact` all exist and are all
deliberately unused. They are enumerated in the compat document's allowlist
table so a later reader can see they were considered and declined, not missed.

### D13. The notification boundary is unchanged

Turn status is owner-only metadata. There is no VirtualActor notification, no
fan-out, and no required streaming channel; Aria observes results by
poll/reload. Local post durability never depends on the provider's result.

### D14. The target is pinned by digest, and an upgrade forces re-verification

The pin is the image digest, not a version range. The decisive request fields
(`chat_id`, `parent_id`, `id`, `user_message`, `background_tasks`) are **not**
described by the target's own OpenAPI document — `/api/chat/completions`
declares a free-form object — so they are empirical properties of this build.
`parent_id`-driven chat management is also relatively new and may be absent
from older builds.

Before running against a different version, re-run the compat document's
"Observation record" procedure and update that document. If the behavior
cannot be reproduced, the feature stays disabled; completion-only operation is
not an acceptable degraded mode.

**TBD (see #50):** the production instance's version, and whether it matches
this digest.

### D15. Unknowns are marked, never invented

Six operational questions remain open (credential provisioning and rotation
ownership — storage is settled by D10; allowlisted origin and presentation
host; sizes/timeouts/rate limits; the production version; the model-access
grant procedure for a non-admin account; socket.io streaming). Each is
written as `TBD (see #50)` or `要実機確認` at the point where it matters, with
no placeholder value that could be mistaken for a decision.

### D16. Tool/web-search execution is Open WebUI's own, never this service's

Issue #72 adds `OPENWEBUI_WEB_SEARCH_ENABLED` and `OPENWEBUI_TOOL_IDS`,
surfaced as request-level `features.web_search`/`tool_ids` fields on every
completions call. These are opt-in flags this adapter sets on the outbound
request, nothing more: the tool call itself (a web search, an MCP-backed
tool, or any other builtin) always runs inside Open WebUI's own request-
handling loop, within the same single buffered HTTP call D3 already
describes, **provided the request also sets `params.function_calling:
"legacy"` (Issue #74 — see D17)**. This repository implements no MCP
protocol, spawns no tool process, and never sees an intermediate tool-call
step, only the final answer.

Web search results and other tool output reach the model's final answer
the same way any other upstream text does, so AGENTS.md's existing rule —
treat posts, feeds, mail, remote API responses, and LLM output as
untrusted data — applies to them without any new mechanism: nothing here
parses, executes, or otherwise trusts a tool's output differently from
the rest of a completion's content.

Every model's tool ids are sent as opaque strings; this service has no way to
list or validate a target instance's tool registry, so a typo'd id simply
never matches anything server-side rather than failing closed here (D17 and
D20 amend this: resolution filters each model's own list against the
target's own accessible-tools list, so a stale or inaccessible id is dropped
and logged rather than sent as-is — the completions request itself still
cannot validate an id Open WebUI accepts and silently no-ops on). Which
tools exist, and what they are allowed to do, remains entirely the Open
WebUI administrator's responsibility.

**Amended by D20 (Issue #75):** `OPENWEBUI_TOOL_IDS` itself — the
deployment-wide override this paragraph originally named — no longer
exists. A model's tool ids now come solely from its own configured
`toolIds`, never a config override.

### D17. `function_calling: "legacy"` is required, and tool_ids resolves against the target's own registry at startup

Issue #74 (real-instance verification, see
[`docs/compat/openwebui-0.11.3.md`](../compat/openwebui-0.11.3.md)'s Phase 0
observation record) found that D16's "single buffered call" claim was only
true under Open WebUI's *legacy* function-calling path. Under the
*native* path — this adapter's pre-#74 default — Open WebUI puts the
resolved tool specs directly on the completions request as an OpenAI-style
`tools` array and lets the model decide whether to call one. A model that
does call one returns `tool_calls` with an empty `content`; Open WebUI's
`non_streaming_chat_response_handler` (the handler this adapter's
`stream:false` calls always hit) reads only `choices[0].message.content` /
`response_data['output']`, both empty in that case, and returns without
ever marking the assistant message done — the exact "turn wedged forever
undone" bug Issue #74 reports. `features.web_search` fares worse under the
native path via this adapter: Open WebUI skips its own forced web-search
injection specifically *because* native function calling is available, and
offers the alternative (a builtin `web_search` tool) only to a request that
carries `metadata.session_id` — a UI-session concept an API-key bridge
call never has — so `features.web_search=true` silently does nothing at
all, neither wedging nor searching.

The fix: whenever this adapter sets `features` or `tool_ids` on a
completions request, it also sets `"params": {"function_calling":
"legacy"}`. Under legacy mode, Open WebUI resolves tool calls and web
search via its own internal pre-completion calls *before* the actual
generation request this adapter's response depends on, so by the time this
adapter's `stream:false` call is answered, the model has already produced
an ordinary content-bearing response — the same shape this adapter always
expected. `params` is otherwise never sent: a plain turn with neither
`features` nor `tool_ids` carries no `params` key at all, unchanged from
before Issue #74. Phase 0 verified all of this against a locally run pinned
0.11.3 instance (a real Tool registered via the "Tools" admin feature, a
real self-hosted SearXNG backend) — not source reading alone — including
that `history.currentId`/`current_message_id` correlation (D2, D6) still
names this adapter's own client-chosen assistant message id under legacy
mode, so no change was needed to `LookupTurnOutcome`'s correlation logic.

`OPENWEBUI_TOOL_IDS` additionally gained a startup-time resolution step
(`internal/openwebui.ResolveEffectiveToolConfig`, called once from
`cmd/server/main.go` before the turn-serving client is built, never
per-turn): unset defers to the target model's own configured `toolIds`
(`GET /api/models`'s `info.meta.toolIds`); a non-empty configured list
overrides the model's default entirely; the literal single-entry value
`"none"` explicitly disables tools even when the model has its own
`toolIds` configured (the only way to force zero tools onto such a
model). Either way, the resulting candidate list is then filtered to ids
`GET /api/v1/tools/` actually reports for this adapter's own account —
fail-closed, never "assume a stale id would still work" — and every
excluded id is logged individually rather than silently dropped. If
either read-only resolution call itself fails (the target is unreachable
at boot, say), the whole server does not fail to start over it: `main.go`
disables both `tool_ids` and `web_search` for that run and logs the
failure at error level, the same "a dead provider never blocks the rest
of the server" principle `OPENWEBUI_GENERATION_ENABLED`'s own gate
already follows.

**Amended by D20 (Issue #75):** this whole paragraph describes machinery
that no longer exists. `ResolveEffectiveToolConfig`,
`internal/openwebui.ToolConfigResolver`, the `"none"` sentinel, and the
`cmd/server/main.go` bootstrap-client dance it required are all retired;
`OPENWEBUI_TOOL_IDS` itself is gone from `internal/config`. See D20 for
what replaced it: the fail-closed accessible-tools filtering rule
described here is the one part that carries over unchanged, now applied
per model on every catalog sync round instead of once at boot for a
single model.

`OPENWEBUI_WEB_SEARCH_ENABLED` gained no equivalent model-default
fallback: it is a plain, always-explicit bool (Issue #72), with no
"unset" state distinct from `false` to defer to the model's own
`defaultFeatureIds` from — unlike `OPENWEBUI_TOOL_IDS`, which has a clean
three-state shape (unset / list / `"none"`) an empty slice already
represents. Whatever value is configured is sent verbatim; adding a
model-default fallback for web search would need
`OPENWEBUI_WEB_SEARCH_ENABLED` to become a tri-state config key first,
which this issue's scope does not require and which no other
`OPENWEBUI_*` key in this codebase does today. This is not only a
config-shape gap: Issue #72 already decided, and this key's own row in
[`docs/operations/configuration.md`](../operations/configuration.md)
already states, that `OPENWEBUI_WEB_SEARCH_ENABLED` is "independent of,
and never inferred from, any per-model web-search setting configured in
the Open WebUI instance's own admin/web UI". A model-default fallback for
the "unset" state would relitigate that decision, not merely extend it —
so even a future tri-state upgrade should not wire `defaultFeatureIds`
into this key without first revisiting Issue #72's own reasoning.

### D18. Catalog sync replaces the single seeded model with every model the account can see

The roadmap's OWUI-P section originally said the MVP "publishes only the
default model as an actor" and listed "automatic discovery or management of
all Open WebUI models" as a non-goal. Issue #75 reverses both: a deployment's
VirtualActor set now tracks the configured account's entire visible model
catalog, not one config-named row.

`Registry.SyncCatalog` (`internal/openwebui/catalog.go`) is the mechanism.
Each round: `provider.ListModels` (`GET /api/models`) is called *before* any
database transaction opens, and an error from it — a timeout, a 5xx, a
malformed body — returns immediately with the registry completely untouched;
a provider outage must never deactivate every model this service already
knows about. A *successful* response is trusted at face value, including an
empty one: a genuine "this account can currently see nothing" deactivates
every previously active model, the same way a model simply missing from a
non-empty response does. This is the one place D6's "classify from state,
never assume" principle applies to a whole-catalog read rather than a single
turn's outcome.

Eligibility (`eligibleRemoteModels`) excludes only what is unambiguous: a
blank id, the built-in `arena-model` entry (checked by both its literal id
and its own `arena: true` field — compat's "GET /api/models" section), and
every occurrence of an id after its first (logged, never silently picked a
winner from — schema drift this adapter has not observed). Everything else
is kept: the hidden/visibility fields `GET /api/models` may carry are
compat's own "weakest-evidenced part" of the whole contract, so a model this
filter is unsure about is registered rather than silently dropped.

A model's slug — the local half of its `@<slug>@<presentation_host>` handle
— is generated once, the first time that model is registered
(`GenerateActorSlug`), and never recomputed afterward, even once a later
sync round learns a nicer display name for it. This is the roadmap's
stable-actor-ID requirement applied to the *handle* specifically, not only
the row id: generation normalizes the model's own name to `[a-z0-9_]`
(lowercased, truncated to 32 characters), falls back to an 8-hex-character
hash of the model's opaque external id when that normalization yields
nothing usable (empty, or a non-Latin name that collapses entirely), and
appends a further hash suffix if the result collides with another model's
already-assigned slug, the owner's own username, or the reserved
`assistant`/`system` presentation names — the same three-way collision rule
`OPENWEBUI_MODEL_SLUG`'s own validation enforced before this issue removed
that config key (D9 amendment, below). A model discovered before its
display name is known (`Registry.Seed`'s own fallback row — see next
paragraph) therefore keeps whatever slug it was first assigned, by design,
even after a later sync learns its real name; only its `DisplayName` field
tracks the provider from then on.

**Amends D9:** `OPENWEBUI_MODEL_DISPLAY_NAME` and `OPENWEBUI_MODEL_SLUG`
(owner-supplied overrides for the single pre-#75 model) are removed
entirely from `internal/config`. `Registry.Seed` still guarantees exactly
one model row exists for `OPENWEBUI_DEFAULT_MODEL_ID` — the fallback this
deployment can rely on even if the provider has never once answered a
catalog sync — but it no longer takes a display name or slug from
configuration to do it: a brand-new such row's `DisplayName` provisionally
equals its own opaque external id, and its slug is generated exactly like
any model `SyncCatalog` discovers. `Seed` also no longer deactivates any
*other* model in the workspace on its own; that decision belongs entirely
to `SyncCatalog` now, run at startup (bounded, best-effort — a failure there
is logged and never blocks the server from starting, `Registry.Seed`'s own
fallback row carrying the deployment until the next successful round) and
on `OPENWEBUI_CATALOG_SYNC_INTERVAL` thereafter (`CatalogScheduler`,
`internal/openwebui/catalogjob.go` — a reduced `internal/ingest.Scheduler`:
one job per interval covers the whole catalog, not one per configured
source). Both run whenever `OPENWEBUI_ENABLED` is true, independent of
`OPENWEBUI_GENERATION_ENABLED`: the VirtualActor projection and search
results stay current even on a deployment that never turns outbound
generation on.

`last_seen_at` (migration `0020`) records when a sync round most recently
reported a given model, for operator visibility only
(`docs/operations/runbook.md`) — `SyncCatalog` decides a model's active
state from whether it appeared in the round just completed, never from how
recently, so nothing reads this column to make that call.

### D19. A post's `@mention` selects its model; a mismatched mention on a reply starts a new branch

With more than one active model, an owner post needs a way to say which one
it is for. The options were: always the workspace default (no per-post
choice at all — untenable once multiple models are routinely active);
inventing new addressing syntax (a command prefix, a structured field); or
reusing the `@mention` syntax D2's stripping already recognized
(`stripMentionTagsForProvider`, Issue #71) for something more than text
cleanup. The owner decided (2026-09-07) on the third: `@<slug>` (optionally
`@<slug>@<presentation_host>`; a *different* host is never a candidate for
this workspace's own models, however its bare slug might otherwise read)
addresses one of the workspace's own active models by its generated
`actor_slug`, matched case-insensitively since a person typing a mention
will not necessarily reproduce a generated slug's case exactly.

`ResolveModelMentions` (`internal/openwebui/mentionresolve.go`) reduces a
post's body to the distinct set of active models it names, and
`Bridge.EnqueueTurn` applies the owner-approved decision table on the
result:

| mentions resolved | outcome |
| --- | --- |
| none | fall back to the workspace's configured default model — unchanged pre-#75 behavior |
| exactly one active model | route the turn to that model instead of the default |
| two or more distinct active models | `ambiguous_model_selection` (below) — never guessed between |
| a slug naming no model, or an inactive one | folds into "none" — never an error of its own |

**Cross-model reply rule (owner decision, 2026-09-07).** `SelectBranch`
(`internal/openwebui/path.go`) gained a `targetModelID` parameter: a reply
continues a `ready` link only when that link is *also* bound to the
resolved model, on top of D5's existing "parent is exactly the link's
current head, and no turn already replies to it" check. A reply
`@mention`-ing a different model than the one its parent link is talking to
therefore always starts a new branch against the mentioned model, exactly
as if it had replied to an earlier node (D5) — generalizing that same rule
from "a different local position in the tree" to "a different model",
rather than adding a new link state or rejecting the reply outright. The new
branch's own `StartChat` seeds a fresh remote chat with that model, never
attaching to — or silently redirecting — the original model's chat.

**Ambiguous selection is recorded, never guessed or dropped (owner decision,
2026-09-07).** Two or more distinct `@mention`ed active models is not
folded into "use the default" (a silent, surprising choice) and not left
entirely unrecorded (an owner post that visibly vanishes with no trace).
`Bridge.EnqueueTurn` instead records — for owner-facing visibility only,
never automatic generation — a link and its one turn, both immediately
`failed` with the new `domain.FailureCategoryAmbiguousModelSelection`, and
enqueues no durable job at all: the provider is never contacted for that
post. The link is bound to the workspace's own default model purely to
satisfy the schema's `NOT NULL` foreign keys
(`openwebui_conversation_links.model_id`,
`openwebui_turn_links.link_id → openwebui_conversation_links.id`) — this is
bookkeeping, never a claim that the default model was "the" intended
recipient, and since the link is created already-`failed` and never leaves
that state, nothing downstream ever reads it back as a routing decision.
Mechanically this reuses D5's own `creation_pending → failed` transition
(`Claim` then `MarkFailed`) rather than inventing a new terminal state:
"ambiguous model selection" is a definitive, known-at-creation-time outcome
in exactly the sense D5's `failed` state already means, not an *uncertain*
one (D5's `ambiguous` state, reserved for a genuinely unknown remote
outcome).

### D20. Per-model tool resolution replaces the single deployment-wide `OPENWEBUI_TOOL_IDS` override

D17's tool_ids resolution ran once, at boot, for the one configured default
model. That stopped making sense once D18 made every active model a
first-class citizen: different models may have their own `toolIds`
configured in Open WebUI's own admin UI, and a single deployment-wide
override would either apply identically to every model (wrong when their
intended capabilities differ) or need to become a per-model config surface
of its own — which the owner decided (2026-09-07) not to build.

**Decision: `OPENWEBUI_TOOL_IDS` is removed entirely.** Every model's
`tool_ids` now come solely from its own `GET /api/models`
`info.meta.toolIds` — never a config override, and never a value one model
borrows from another. Resolution happens once per catalog sync round, not
per turn: `Registry.SyncCatalog` filters each eligible model's own `ToolIDs`
(already captured by the same `GET /api/models` call that reconciles the
registry — D18) against `GET /api/v1/tools/`'s accessible-tools list —
exactly D17's fail-closed exclusion rule, now applied per model instead of
to one resolved-at-boot default — and writes the result into a small
in-memory `ToolConfigCache` (`internal/openwebui/toolcache.go`) keyed by
`ExternalModelID`. `TurnJob` looks up its own turn's model in that shared
cache when building `StartChatRequest`/`ContinueTurnRequest`; a cache miss —
a model discovered by a post before the next sync round has priced it in, or
no cache at all — sends no `tool_ids` key, the same safe default an
inaccessible or unconfigured id already fell back to under D17.
`StartChatRequest`/`ContinueTurnRequest` each gained a `ToolIDs` field for
this, moving what was a `Client`-construction-time value under D17 to a
per-call one.

A transient failure resolving the accessible-tools list during a sync round
is fail-open for the cache specifically, independent of the registry
reconciliation half of that same round: it is logged and the previous
round's cache is kept exactly as it was, rather than blanking every model's
`tool_ids` or failing the whole sync over an auxiliary check.

**`OPENWEBUI_WEB_SEARCH_ENABLED` is explicitly unchanged.** It stays one
plain, deployment-wide boolean and a `Client`-construction-time setting,
per explicit owner decision (2026-09-07) — the same "no per-model
config-shape change this issue's scope does not require" reasoning D17
already gave for not wiring `defaultFeatureIds` into it, now confirmed
rather than revisited even though `tool_ids` itself did move per-model.

This retires `internal/openwebui.ResolveEffectiveToolConfig`,
`ToolConfigResolver`, `ToolIDsNone`, and `ResolvedToolConfig` (D17's own
machinery) along with `internal/provider/openwebui.Client.GetModelTools` and
its `Config.ToolIDs` field; `cmd/server/main.go`'s startup
bootstrap-client/two-read-only-call dance is removed with them; a single
`ToolConfigCache` is built once and shared between the startup sync, the
periodic `CatalogSyncJob`, and `TurnJob`.

## Consequences

- **#52 (OWUI-P)** gets its domain and migration inputs from D2, D3, D9, and
  D10: which remote ids exist, that they are client- or server-generated, that
  `WorkspaceDefinition` is instance-plus-account, and how the credential is
  referenced. D10's `secret_ref` reading is now an owner decision (2026-09-06),
  so #52 can freeze the schema against it: the field holds a reference to a
  config key, and #52 builds no secret store of its own.
- **#53 (OWUI-B)** gets its adapter and job inputs from D3, D6, D7, D10, and
  D11 — including that the worker resolves `secret_ref` from config, never a
  store of its own — and its fixtures from
  [`docs/compat/fixtures/openwebui/`](../compat/fixtures/openwebui/). The
  redaction test asserts on `sk-mock-upstream-secret`.
- **#54 (OWUI-R)** inherits D14 as release evidence: the digest, the
  observation record, and the re-verification requirement.
- **#75** gets its catalog-sync, mention-routing, and per-model tool-
  resolution inputs from D18, D19, and D20 — each amending, not replacing,
  the #52/#53/#72/#74 decisions it builds on: D18 amends D9 (identity
  projection now spans every visible model, not one seeded row), D19
  amends D5 (the branch rule's "reply to an earlier node" generalizes to
  "reply naming a different model"), and D20 amends D16/D17 (tool
  resolution is per model and per sync round, never a single
  deployment-wide override resolved once at boot).
- Every Open WebUI upgrade is a documentation event, not just a config change.
- Streaming, regeneration, remote branch management, and cancellation each
  need their own contract work before they can be picked up; none of them is
  a leftover TODO inside the #52–#54 sequence.
- This ADR merges the two ADR items the roadmap's OWUI-C section lists (the
  boundary ADR and the outbound-only lifecycle ADR) into one document, as
  agreed in #51. It is numbered 0005 because the roadmap's proposed
  `0003-openwebui-boundary.md` collides with the existing ADR-0003 and
  ADR-0004.
