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

**Amended by D24 (Issue #93).** "Streaming needs a separate contract" was
correct as a caution, not as a permanent boundary: D24 is that contract,
scoped narrowly to the one row of this section's own table this paragraph
already named ("the only true HTTP SSE path is the legacy one" — no
`chat_id` sent) and to the one case (native multi-round tool execution)
that path turned out to solve. No socket.io client was needed, because
D24 deliberately never uses either of the *other* two `stream:true`
behaviors this section describes (both of which are socket.io-delivered).
The chat-managed path D1–D23 describe still has no streaming mode of any
kind; this amendment does not reopen that.

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

**Amended by D27 (Issue #123):** `"params": {"function_calling":
"legacy"}` is no longer sent for a tool-carrying turn at all — D27 replaces
it with `stream:true` and no `params` key, letting Open WebUI's native loop
run instead of forcing legacy pre-resolution. `runTurn` no longer has any
caller that sets `params`, so `completionsRequestBody.Params`/`paramsBody`
are removed from the adapter. This paragraph's account of *why* legacy mode
was chosen over native under `stream:false` remains accurate history; it
no longer describes this adapter's present behavior, since no code path
sends a `stream:false` tool-carrying request anymore.

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

### D21. `OPENWEBUI_WEB_SEARCH_ENABLED` becomes tri-state: explicit config always wins, unset follows the model's own `defaultFeatureIds`

D17 and D20 both explicitly left `OPENWEBUI_WEB_SEARCH_ENABLED` a plain,
always-explicit, deployment-wide boolean — D17 because a tri-state upgrade
was out of scope for Issue #72, D20 because the owner (2026-09-07) confirmed
that reasoning rather than revisiting it even as `tool_ids` itself moved
per-model in the same decision. The owner reversed that call the same day,
while `issue-75-pr1-catalog-sync` (this issue's own implementation branch)
was being rebased onto D20's `OPENWEBUI_TOOL_IDS` retirement: leaving
`defaultFeatureIds` already captured on every `GET /api/models` call
(`RemoteModel.DefaultFeatureIDs`, D18) but never read was judged an
incomplete reading of Issue #75's Acceptance Criterion 11 ("Tool/default
feature selection is resolved per selected model"), not a deliberate scope
boundary worth keeping now that the branch was being finished rather than
merely planned.

**Decision: `OPENWEBUI_WEB_SEARCH_ENABLED` is now tri-state — unset /
`true` / `false` — with unset (not `false`) the value that defers to the
model.** `internal/config.OpenWebUIConfig.WebSearchEnabled` changes from
`bool` to `*bool`: a nil pointer means "not configured", distinct from an
explicit `false`. The priority rule, resolved once per turn per model
(`TurnJob.resolveWebSearchEnabled`), mirrors `OPENWEBUI_TOOL_IDS`'s own
pre-D20 three-state shape (D17) despite that key's retirement:

| `OPENWEBUI_WEB_SEARCH_ENABLED` | resolved `features.web_search` |
| --- | --- |
| `true` | always on, for every model, regardless of that model's own `defaultFeatureIds` |
| `false` | always off, for every model, regardless of that model's own `defaultFeatureIds` |
| unset | on only for a model whose most recently synced `info.meta.defaultFeatureIds` contains `"web_search"`; off otherwise, including a model never yet synced |

An explicit `true`/`false` always overrides every model uniformly — the
same "whatever value is configured is sent verbatim" behavior D17 already
described for the old plain-bool shape, just no longer the *only*
reachable state. Only the unset state is new, and it is resolved per
model rather than once for the whole deployment, matching D20's own
per-model precedent for `tool_ids`.

**Mechanism.** A new `internal/openwebui.FeatureDefaultCache`
(`internal/openwebui/featurecache.go`) mirrors `ToolConfigCache`'s shape
exactly (D20): keyed by `ExternalModelID`, written only by
`Registry.SyncCatalog` at the end of a successful round, read only by
`TurnJob`. Unlike `ToolConfigCache`, resolving it needs no second provider
call and no fail-closed filtering step — a model's own advertised
`defaultFeatureIds` is trusted the same way its `DisplayName` already is,
not treated as a capability claim to verify against a separate
accessible-tools list — so `resolveFeatureDefaults` runs unconditionally
alongside the registry write, never skipped the way `toolCache`'s update
can be on a `ListAccessibleTools` failure. `StartChatRequest`/
`ContinueTurnRequest` each gain a `WebSearchEnabled bool` field
(`internal/openwebui/provider.go`), computed by
`TurnJob.resolveWebSearchEnabled` from `TurnJobConfig.WebSearchOverride`
(the explicit config value, if set) or else `FeatureDefaultCache.Get` —
moving `OPENWEBUI_WEB_SEARCH_ENABLED` from a `Client`-construction-time
setting to a per-call one, exactly the move D20 already made for
`tool_ids`. `internal/provider/openwebui.Client.Config.WebSearchEnabled`
is removed entirely: `runTurn` now takes the resolved value as a
parameter on every call instead of reading a field fixed at construction.

This does not relitigate Issue #72's original "independent of, and never
inferred from, any per-model web-search setting configured in the Open
WebUI instance's own **admin/web UI**" statement
([`docs/operations/configuration.md`](../operations/configuration.md)),
which D17 was careful to distinguish from `defaultFeatureIds`:
`defaultFeatureIds` is a value `GET /api/models` itself already returns to
any caller of that endpoint (compat's own documented contract, not the
separate web-UI-only per-model settings surface D17's own text warns
against conflating), so reading it back here consumes the same API
contract this adapter already depends on for `tool_ids`, not inferring
provider-internal configuration from a side channel Open WebUI never
exposes to an API-key caller.

### D22. `sources` is parsed and rendered as reply footnotes, never persisted as raw tool/document text

Issue #81's real-instance check (2026-09-07, non-streaming +
`params.function_calling=legacy`, the D17 path) found the completions
response carries a top-level `sources[]` array whenever a tool call or web
search actually ran, distinguished by the presence of `tool_result: true`.
Two shapes were observed: a tool-execution source (`source.name`,
`document[]` as the tool's raw JSON output, `metadata[].parameters` as the
call arguments) and a web-search source (`source.{name,type,urls,queries}`,
multiple `document[]` page-text chunks, `metadata[].{title,description,
source}` per chunk, plus `distances`).

This adapter normalizes both shapes into a small internal
`openwebui.Source{Kind, DisplayName, URL, Arguments}` record
(`internal/provider/openwebui/client.go`'s `normalizeSources`) before
anything else touches them: `document[]` — untrusted tool/page text,
potentially large — is **never stored, logged, or forwarded to Aria**;
`wireSource`/`wireSourceMetadata` (the raw decode target) carry no field for
it at all, so encoding/json's default unknown-field-skipping enforces that
for free rather than depending on a caller to remember not to read it. Only
each source's short descriptive fields survive, each bounded to
`maxSourceFieldLen` (200 bytes) the same defense-in-depth D9 already gives a
model display name. `sources` is captured from the completions response
body itself, at the point `runTurn` decodes it — not re-derived from a later
`GET /api/v1/chats/{id}` call: whether the chat-managed GET path also
carries `sources` for a completed turn is unverified, so the adapter must
not depend on being able to retrieve it a second time. `sources` is decoded
as `json.RawMessage` at the top level and only then parsed into typed
records (`decodeSources`); a shape this adapter cannot parse — schema
drift, or a provider bug — yields no sources for that reply rather than
failing the whole completions decode, since a turn whose actual content
(`choices`) decoded fine must still succeed: citations are enrichment,
never load-bearing for turn success. One consequence
(documented on `TurnOutcome.Title`'s own field, not repeated here) is that a
turn recovered through the uncertain-outcome lookup-and-adopt path (Issue
#53's `handleReadyRetry`) never gets sources attached, even if the original
completions call that produced it would have — a possibly-missing footnote
list on an already-rare recovery path, not a correctness or security
concern.

The `[n]` citation markers Open WebUI writes into the model's own answer
text are assumed to correspond to `sources[]` in array order — the only
case verified so far (2026-09-07) has exactly one source. **This assumption
is recorded here, not invented silently (D15), and — per the owner's
2026-09-08 direction to implement Issues #81/#84 without further
real-instance access — it ships as an explicit, documented, unverified
assumption rather than blocking the feature:** `openwebui.Source`'s own doc
comment, `internal/provider/openwebui/client.go`'s `normalizeSources`, and
`internal/httpserver/noteapi_wire.go`'s `renderSourceFootnotes` each repeat
the same warning at the point a reader would need it. The concrete failure
mode if this assumption is wrong — for example, a real multi-source
response nesting several web-search result chunks under one `sources[]`
element rather than one element per chunk — is a footnote whose number
disagrees with the reply text's own `[n]` markers, or a footnote that shows
only the first chunk of a multi-chunk group. That is a cosmetic display
defect only: `document[]` is never captured regardless of whether this
assumption holds, so no additional data can leak from it being wrong, and a
sources-normalization anomaly never fails the turn itself (`runTurn`'s
success path does not depend on `sources` decoding to anything in
particular). This must be re-verified against a real multi-source turn
before the assumption note is removed from the referenced files.

An INFO log records `turn_id`, source kind, and tool/display name, and —
for a tool call only — its arguments, never `document`, never a web-search
result's page text, mirroring D6's existing rule that provider response
bodies are never logged verbatim.

### D23. One narrow, opt-in exception to D2: an owner-facing "view in Open WebUI" link

D2 states that `chat_id` and the other remote correlation ids never surface
in an Aria payload, token, or URL. Issue #84 asks for exactly that, in one
specific, bounded form: a link back to the same conversation on the same
Open WebUI instance, shown only to the owner, only in the reply's own text.

The decision is to allow it, narrowly:

- The link is built only when the operator has explicitly set a new,
  opt-in config key naming a browser-reachable origin for the same instance
  (`OPENWEBUI_VIEWER_BASE_URL`) — distinct from `OPENWEBUI_BASE_URL` for the
  same reason `OPENWEBUI_PRESENTATION_HOST` is already distinct from it
  (D9): a value this server calls and a value a browser can reach are not
  guaranteed to be the same address, and nothing may infer one from the
  other. Leaving it unset reproduces today's behavior exactly — no link, no
  `title_generation` call, no exception in effect.
- The link is display-only: `remote_chat_id` is rendered into
  `<OPENWEBUI_VIEWER_BASE_URL>/c/<remote_chat_id>` (`internal/httpserver/
  noteapi_wire.go`'s `enrichOpenWebUIReplyText`) and nothing else. It is
  never accepted back as input, never used to resolve a reply's parentage,
  model, or workspace, and this feature adds no endpoint that reads a chat
  id from an Aria request. D2's actual guarantee — that remote ids are never
  authoritative for local identity, ordering, or authorization — is
  completely unchanged; only the narrow "never surface... in URLs" clause
  gets this one named exception.
- The only viewer who ever sees this URL is the local owner, reading their
  own Aria timeline — the same principal who already holds (or can be
  issued) full access to the Open WebUI account the bridge uses. The link
  discloses no capability the owner does not already have.
- `OPENWEBUI_VIEWER_BASE_URL` is validated the same way `OPENWEBUI_BASE_URL`
  is (HTTPS-only origin, no userinfo/path/query/fragment;
  `validateOpenWebUIViewerBaseURL`, `internal/config/config.go`) but is
  **not** added to `OPENWEBUI_ALLOWED_ORIGINS`/the SSRF allowlist: this
  server never dials it, so D11's connection-time policy does not apply.
  Validation exists to keep a malformed value out of rendered Note text, not
  to gate an outbound request that never happens.

Every other remote id this service holds — `message.id`, `parentId`,
`currentId` — is unaffected and stays exactly as opaque and un-surfaced as
D2 already requires.

**The chat title half of the same issue is tied to the same config key, and
is separately unverified.** Issue #84's own write-up recorded a known
constraint: even with `background_tasks.title_generation` never requested,
Open WebUI overwrites `createChat`'s `"bridge-precreated"` placeholder title
with the raw first user message almost immediately — so this adapter's own
`resolvedTitle` filters only the literal placeholder, never attempting to
detect "is this actually a generated summary, or just the raw first
message echoed back." Whether `title_generation: true` resolves
synchronously (visible on the very next `GET /api/v1/chats/{id}` `runTurn`
already performs for outcome confirmation) or asynchronously (visible only
much later, if ever) was **never confirmed against a real instance** — per
the owner's 2026-09-08 direction, this ships anyway, gated end to end by
`OPENWEBUI_VIEWER_BASE_URL` being configured at all:
`StartChatRequest.EnableTitleGeneration` is set from exactly that (Bridge
never requests title generation, and `TurnJob.complete` never persists a
title, while the key is unset — see `TurnJobConfig.ViewerBaseURL`'s own doc
comment), and no extra polling call is added to chase a title that has not
appeared yet. If generation turns out to be asynchronous in practice, the
observable effect is that most replies simply show no title (the
`"[reply]"` marker stays as it always was) rather than a summarized one —
degraded to "feature quietly does nothing yet," never to a wrong title, a
data leak, or a failed turn. This must be re-verified against a real
instance before this note is removed from `EnableTitleGeneration`'s and
`precreatedChatTitle`'s doc comments.

### D24. A new, stateless, true-SSE turn mode carries native multi-round tool execution — mechanism only, not yet dispatched to

> **Superseded (Issue #120 found the premise false; Issue #123/D27 replaces
> the mechanism, 2026-09-08).**
> This decision rests on "row 1 of (h) delivers native multi-round tool
> execution over plain HTTP". A real-instance capture falsifies that: row 1 is
> served by `stream_wrapper` (`utils/middleware.py:6320`), a pure proxy, while
> the native tool loop (`:5576`) sits inside `if event_emitter:`, and
> `event_emitter` exists only when the request carries `chat_id` **and**
> `message_id` (`:3127`). A row-1 request therefore cannot enter the branch that
> runs tools, and the same branch is what returns `null` instead of a body
> (`:4252`, returned at `:6315`) — so receiving tokens over HTTP and having the
> server execute tools are mutually exclusive on this endpoint. Captures and the
> full trace are in `docs/compat/openwebui-0.11.3.md` (h.1)/(j).
>
> In production this surfaces as every tool- or web-search-carrying first turn
> failing `contract_failed` (the stream ends at the upstream provider's own
> terminal frame, never at `data: [DONE]`), which D25 classifies as permanent,
> so the turn is not retried and the post receives no reply.
>
> Per AGENTS.md ("stop implementation and record or request an ADR update; do
> not silently invent a protocol") the PR that recorded this finding (#121)
> changed no behavior. **Decided and implemented as D27 below (Issue #123):**
> send the tool-carrying turn **chat-managed** (`chat_id` + `id` +
> `user_message`, `stream: true`), ignore
> the `null` body, and wait for completion by polling
> `GET /api/v1/chats/{id}` — the read `Client.LookupTurnOutcome`
> (`internal/provider/openwebui/client.go`) already performs — until the
> assistant message reports `done`, bounded by `OPENWEBUI_TOOL_TURN_TIMEOUT`.
> That branch is the one that runs the native loop to
> `CHAT_RESPONSE_MAX_TOOL_CALL_ITERATIONS` rounds and persists the result with
> `done: true` (`:6249`); it needs no socket listener, because
> `get_event_emitter` (`socket/main.py:1057`) emits into an unattended room and
> then writes to the database. Its costs, all accepted in D27: D25 is
> withdrawn (polling reintroduces exactly the `ambiguous` outcome D25
> removed, and D6's chat-state classification returns), `LookupTurnOutcome`
> gains a caller that polls it instead of reading it once, and `StreamTurn`'s
> stateless request shape is retired in favor of the chat-management keys the
> buffered path already sends. Browser-side "direct" tools and the pyodide
> code interpreter stay unavailable either way — they need `event_caller`,
> hence a real `session_id` (`:3131`).
>
> Alternatives considered and not recommended: implementing a socket.io client
> (large surface, and the same result is already durably readable from the chat);
> teaching the reader the Responses API framing (the frames arrive, but nothing
> executes the tool call they carry, so multi-round execution is still absent);
> and withdrawing native mode entirely (returns to D17's one-round ceiling, which
> is the limitation Issue #93 exists to remove).

Issue #93 found that D17's `params.function_calling: "legacy"` fix, while
correct for the `done:false`-forever bug it targeted, has its own side
effect: legacy mode resolves tool calls and web search in exactly **one**
pre-completion round, so a turn can never look at a tool's result and
decide to call another tool, or rephrase a web search that returned
nothing, the way Open WebUI's *native* function-calling loop
(`utils/middleware.py`'s `process_chat_payload`, bounded by
`CHAT_RESPONSE_MAX_TOOL_CALL_ITERATIONS`, default 256) can. Native mode
is exactly what D17 moved *away* from, because the buffered
(`stream:false`) response path this adapter otherwise always uses never
finishes a message whose model decided to call a tool natively
(`non_streaming_chat_response_handler` reads only
`choices[0].message.content`).

The other row of the same "(h)" table D8 already recorded supplies the
missing piece: with `stream:true` and **no `chat_id` at all**, Open WebUI
passes the upstream model's own SSE stream straight through over plain
HTTP (`sse_passthrough_finish.sse.txt`, a real capture). Reading that
stream, rather than the buffered response, is what lets a `stream:true`
call reach a native tool-calling model's actual final answer instead of
hanging on an empty `content`.

**Decision: a new method, `internal/provider/openwebui.Client.StreamTurn`
(`openwebui.StreamTurnRequest`/reuses `openwebui.TurnResult`), implements
this path as a second, independent turn mode.** Issue #93's own
write-up frames the migration as staged ("streaming を実装し、tool/
web-search を使う呼び出しだけ native へ切り替え、問題があれば legacy へ戻
せるようにする" — implement streaming first, then switch only tool/
web-search calls over, with a way back to legacy if it misbehaves); this
section (D24) is stage one, the mechanism itself. **Amended by D26
(Issue #93, same PR series): stage two — `TurnJob` actually dispatching
tool/web-search-using turns to this mechanism — followed immediately
after, on a new branch's first turn only.** Declaring the mechanism
inside this same ADR ahead of D26's own wiring follows the pattern
D22/D23 already set (a decision recorded and shipped as soon as its own
scope is settled, not held back to be bundled with a later section's).

The new mode's request shape is deliberately minimal, not merely
`completionsRequestBody` with `stream:true`: it sends `model`, `stream:
true`, `messages` (the same locally-reconstructed root-to-parent context
D5 already builds), and the opt-in `features`/`tool_ids` D16 describes —
**and nothing else**. `chat_id`, `parent_id`, `id`, `user_message`, and
`background_tasks` are all omitted entirely, not merely left at their
zero value: D4's "every completions request carries the `parent_id` key"
rule is specific to the chat-managed path this mode does not use, and
sending a stray `chat_id`/`id` naming no real chat would risk exactly the
"overwrites an existing message" failure mode D4 warns about. Most
importantly, **`params.function_calling` is never set on this mode's
request** — the entire reason this mode exists is to let Open WebUI
resolve tool calls natively, so forcing `legacy` here would silently
defeat it.

Because this mode creates no chat, several things D1-D23 give the
chat-managed path have no counterpart here, by design, not by omission:

- No `OnChatCreated` hook, no `remote_chat_id`, no `ConversationLink`
  state machine (`internal/openwebui/registry.go`) involvement at all.
  `TurnResult.RemoteCurrentID` and `.Title` are always nil from this
  method.
- No usage accounting: the request never sets a `stream_options.
  include_usage`-equivalent field (whether Open WebUI even honors one on
  this path is itself unverified), so `TurnResult.PromptTokens`/
  `.CompletionTokens` are always nil.
- No `sources[]` (Issue #81, D22): whether the true-SSE path carries any
  equivalent signal for a tool call or web search that ran was never
  captured. `TurnResult.Sources` is always nil from this method until
  that is checked against a real instance.

**UNVERIFIED (2026-09-08, per the owner's direction to implement Issue
#93 without further real-instance access — the same "ship on a
documented assumption" precedent D22/D23 already set): whether a
`stream:true` request that also carries `tool_ids`/`features` — the
combination this mode actually needs — reaches the same "no `chat_id` →
real SSE passthrough" row of D8's table at all.** The only real capture
behind that row (`sse_passthrough_finish.sse.txt`) used neither `tool_ids`
nor `features`; whether Open WebUI's tool-decision branch changes which
of the three `stream:true` behaviors a request lands in was never
exercised, and neither was whether an intermediate tool-call round ever
surfaces as its own SSE chunk (a `delta.tool_calls` payload, OpenAI's own
shape for "the model is calling a tool, not answering yet" — carrying
`finish_reason: "tool_calls"` on the chunk that ends that round) as
opposed to resolving entirely server-side with only the final round's
answer streamed as `delta.content`. This adapter is written to tolerate
either shape without guessing wrong either way: `decodeStreamingCompletion`
(`internal/provider/openwebui/client.go`) ends the read on the literal
`data: [DONE]` event alone, **never** on a chunk's own `finish_reason`
becoming non-null — a `finish_reason: "tool_calls"` chunk mid-stream is
recorded (last non-null value wins) but never treated as the stream's
end, precisely so a passed-through intermediate round does not get
mistaken for the turn's actual completion. `Client.StreamTurn`'s own doc
comment and `streamChunkBody`'s repeat this same warning. The concrete
failure mode if the broader assumption (this path is reached at all) is
wrong is that the stream this method reads never reaches `[DONE]` (Open
WebUI having actually queued in-band the way D8's other two rows
describe, or having closed the connection for an unrelated reason) —
which D25 already treats as an ordinary failure, not a wrong answer, a
hang, or a security concern. This must be re-verified against a real
instance — including whether an orphaned, untracked chat is silently
created server-side despite no `chat_id` being sent, since that would be
an operational (not correctness) concern worth its own follow-up —
before this mode is ever dispatched to in production.

### D25. The stateless mode has no `ambiguous` outcome: every failure discards partial content and ends the whole turn

> **Withdrawn (Issue #123/D27, 2026-09-08).** This decision held only while
> the mode was stateless. D27 replaces it with a chat-managed mode, so a
> chat *does* exist for a later `GET /api/v1/chats/{id}` to read — the exact
> premise this decision denied — and D6's classification now applies to
> tool-carrying turns exactly as it does to every other chat-managed turn.
> The paragraphs below are retained as the record of this decision while it
> held, from Issue #93 until Issue #120 found its premise false.

D6's `ambiguous` classification exists because the chat-managed path can
always fall back on `GET /api/v1/chats/{id}` to find out what actually
happened to an uncertain completion. D24's mode has nothing to fall back
on: no `chat_id` was ever sent, so there is no chat for a later GET to
read. **Decision: `StreamTurn` never returns `openwebui.
CategoryAmbiguous`.** Every way the stream can end other than reaching
the literal `data: [DONE]` event — a decode error, the configured byte
bound being exceeded, a connection drop, a timeout, or the stream simply
closing cleanly without ever reaching it — is classified into one of the
ordinary failure categories
(`CategoryTransport`, `CategoryTimeout`, `CategoryContractFailed`, ...)
and, whichever it is, the content accumulated so far is always discarded,
never returned as a partial answer.

This is a deliberate trade-off, not an oversight, and Issue #93's own
"検討事項" section poses exactly this question ("部分出力を破棄するのか、
`ambiguous` として chat 側の確認に委ねるのか"): the chat-managed path's
`ambiguous` state is a safety net against double-generation (a process
crash mid-turn can be resolved later by reading back what the provider
already committed, rather than blindly retrying); this mode has no
provider-side record to read back, so retrying the whole turn from
scratch is the only option regardless of what the failure classification
says — an explicit `ambiguous` state would not change what the caller
does next, only add a state that can never be resolved. The concrete
cost: **a retried turn after a mid-stream disconnect may re-run the same
tool call** (a web search, most likely) that a still-in-flight upstream
call had already started, since nothing here suppresses that the way D7's
local idempotency covers a *provider-committed* duplicate. For this
deployment's actual tool set (web search; no destructive operation is
configured) the cost of an extra search is negligible. **This must be
re-examined before any future tool with a side effect (a write, a
purchase, a message send) is ever added to this deployment's tool_ids**,
since D25's safety argument depends specifically on every currently
configured tool being idempotent-enough to re-run for free.

### D26. `TurnJob` dispatches a branch's first turn to StreamTurn when its resolved tool config calls for it — and never a continuation

> **Withdrawn (Issue #123/D27, 2026-09-08), including the "never a
> continuation" restriction below, not only the StreamTurn dispatch
> mechanism.** D27 folds tool-carrying turns back into the ordinary
> `StartChat`/`ContinueTurn` call sites via `runTurn`, so there is no longer
> a separate dispatch decision to make at `handleCreationPending`, and no
> reason a continuation cannot use native tool execution too — the
> restriction this section documents is resolved, not merely obsolete. The
> paragraphs below are retained as the record of this decision while it
> held, from Issue #93 until Issue #123 replaced it.

D24 built the mechanism; this is Issue #93's own "switch tool/web-search
calls over" stage. `TurnJob.handleCreationPending`
(`internal/openwebui/turnjob.go`) now resolves `tool_ids`/`web_search`
(the same `resolveToolIDs`/`resolveWebSearchEnabled` calls D20/D21
already made) *before* deciding which provider call to make: a non-empty
resolution hands off to `handleCreationPendingStateless`, which calls
`StreamTurn` instead of `StartChat`. Everything else about
`handleCreationPending` — the `AllowsInitialStartChat` claim check, the
per-thread lock `Handle` already took, path (re)validation — is
unchanged and shared by both branches, so the split is purely about
which provider method eventually gets called and what the link records
about the outcome.

**Scope: only a branch's first turn ever considers this switch.** A
continuation on an already-`ready` link (`handleReady`,
`resendContinue`, `handleReadyRetry`) never re-resolves the question and
always stays on `ContinueTurn`, even if that specific turn's own tool
config would now resolve to tool use. This is a deliberate, documented
limitation, not an oversight: a link only ever reaches `LinkReady`
through a chat-managed `StartChat`, and there is no remote-chat-
preserving way to move a link that already has one onto the chat-less
streaming path mid-conversation — StreamTurn creates no chat for a
continuation to attach to in the first place. A reply that wants native
tool execution and is *not* eligible to continue an existing ready link
(a different position, a different model, or simply a branch's first
message) is unaffected: it starts a new branch through
`handleCreationPending` exactly as before, free to go stateless there.

**A successful stateless turn reaches a new terminal `LinkState`,
`LinkStateless` (migration `0032`, a `-- migrate:rebuild` widening the
`state` `CHECK` list the same way `0016`/`0027` did for
`actors.actor_type`), never `LinkReady`.** Nothing in this state was
available to D5/D19's branch rule before Issue #93; `AllowsContinue`'s
existing `state == LinkReady` check already excludes it with no change
needed there, so `SelectBranch` treats any later reply to a stateless
branch exactly like a reply to a failed or dead one: it always starts a
new branch, free to go stateless again on its own first turn. The
transition is written by a new repository method, `MarkStateless`, whose
one caller is `TurnJob.complete` — inside the *same* transaction that
records the turn's own `succeeded` outcome and creates its generated
reply, never as an earlier separate write the way `OnChatCreated`'s
`MarkReady` call is for the chat-managed path. This placement is load-
bearing, not a style choice: a `StreamTurn` call has no earlier "the
remote side effect definitely happened" checkpoint the way chat creation
does (there is no remote side effect at all), so there is nothing to
gain from writing the transition any earlier — and D25's own "no
`ambiguous` outcome, ever" guarantee would not actually hold if a crash
between an earlier write and this transaction could leave a link stuck
looking unresolved with no lookup able to recover it. `complete` takes a
new `stateless bool` parameter for this: when true, it writes
`MarkStateless` in place of `SetRemoteCurrent` (whose own `WHERE state =
'ready'` guard would otherwise turn every stateless completion into a
spurious `ErrConflict`, since a stateless link never becomes ready) and
skips the `isContinuation` capability-verification block, which only
ever applies to the chat-managed path.

**A StreamTurn failure classifies through a new function,
`handleStreamTurnError`, structurally simpler than `handleCreateError`/
`handleTurnError`: there is no `ambiguous` branch anywhere in it (D25).**
A definitive category (`auth_failed`, `client_rejected`,
`contract_failed`, `policy_violation`) fails the turn and freezes the
link `failed` on the very first attempt, the same as a StartChat
creation failure — even though, unlike chat creation, a stateless call
could safely have been retried; these categories simply are not worth
retrying regardless of path. Every other category is retried up to the
job's own `MaxAttempts`, exactly like a chat-managed continuation's
bounded retry — but where that path's exhaustion freezes the link
`ambiguous` (a GET lookup might still resolve it later), this path's
exhaustion calls `failPermanent` instead, with the *specific* provider
category preserved as the turn's own `domain.FailureCategory`
(`rate_limited`/`server_error`/`timeout` map onto their own
already-declared-but-previously-unused constants; anything else,
`transport`) rather than a generic placeholder — there being no
`ambiguous` state left to absorb the "which kind of transient failure
was it" detail the way `failAmbiguous`'s nil category currently
discards it for the chat-managed path.

**Also unlike `handleCreationPending`'s StartChat call, this dispatch
carries no "already attempted once, freeze ambiguous" guard at all.**
`StreamTurn` creates nothing remote to duplicate (D25), so every
delivery of the job — first attempt or last — simply calls it again from
scratch on a transient failure, the same safe-to-repeat shape
`resendContinue`'s continuation retries already have; the local
`turn.attempt` counter still advances via `BeginAttempt` each time, for
observability, but nothing gates re-entry on its value the way
`handleCreationPending`'s own `turn.Attempt > 0` check does for
StartChat.

### D27. Tool-carrying turns run chat-managed with `stream:true`, polling `GET /api/v1/chats/{id}` for completion — StreamTurn's stateless mode is retired

**Decided (Issue #123, 2026-09-08), replacing D24's stateless mechanism and
withdrawing D25 and D26's dispatch restriction, per the recommendation D24's
own "Superseded in premise" note (Issue #120) already recorded.** The premise
falsification stands as found: row 1 (`stream:true`, no `chat_id`) can never
enter Open WebUI's native tool loop, because that loop lives inside
`if event_emitter:` and `event_emitter` requires `chat_id` *and*
`message_id` (`utils/middleware.py:3127`,`:5576`). The fix is not a
correction to `StreamTurn`; it is retiring the whole stateless mechanism and
returning tool-carrying turns to the chat-managed path D1-D23 already
describe, with two changes: `stream:true` instead of `stream:false`, and no
`params.function_calling: "legacy"` — so Open WebUI's native loop actually
runs — plus polling in place of D6's single confirming `GET`.

**The change is confined to `internal/provider/openwebui.Client.runTurn`.**
`StartChat` and `ContinueTurn` already funnel every turn through `runTurn`,
which already decides whether to set `params.function_calling: "legacy"`
from the same `len(toolIDs) > 0 || webSearchEnabled` condition. That
condition now also selects `Stream: true` (instead of the previous
unconditional `false`) and skips setting `params` entirely, and replaces the
single `LookupTurnOutcome` confirmation with a new `awaitTurnDone` helper
that calls the same, unmodified `LookupTurnOutcome` (`GET
/api/v1/chats/{id}`) repeatedly — on a fixed interval, bounded by a
`context.WithTimeout(ctx, c.toolTurnTimeout)` deadline — until the assistant
message reports `done` or carries an error, or the deadline passes. The
poll loop returns whatever outcome (or error) it last observed; `runTurn`'s
own success/failure switch, unchanged, then classifies it exactly as it
already does for a single-shot confirmation: `Found && Done` succeeds,
`HasError` fails, and anything else — now including "still polling when the
deadline passed" — is `CategoryAmbiguous`, D6's ordinary "resolve it later
from a GET" case.

**`OPENWEBUI_TOOL_TURN_TIMEOUT`'s meaning changes.** It bounded a single
open SSE connection under D24; it now bounds the whole polling loop's
wall-clock budget after the initiating POST returns. Its default (10
minutes) is unchanged — sized, both times, for "however many rounds Open
WebUI's own native loop runs before answering." The initiating POST itself
keeps using the ordinary, shorter `Timeout` (via the existing `c.post`),
since (h)/(h.1)'s observed rows 2-3 both return immediately with a `null`
body while generation continues server-side — **UNVERIFIED for the specific
combination this mode sends** (`chat_id` + `stream:true` + `tool_ids`/
`features` together, no `session_id`): the real captures behind that
"returns immediately" observation used neither `tool_ids` nor `features`.
If a real instance instead blocks the initiating POST for the whole
native-loop duration under this exact combination, the initiating POST
needs `ToolTurnTimeout` too — a bounded, one-line fix, not a redesign,
should it come to that.

**D25 is withdrawn.** Every reason it gave no longer holds once the mode is
chat-managed: a chat now exists, so an inconclusive completion is no longer
unrecoverable — it is exactly what a later `GET` can still resolve, D6's
`ambiguous` case, unchanged from every other chat-managed turn. The
practical costs D25's own text flagged when it was written are accepted:
`LookupTurnOutcome` gains a caller that polls it, rather than only calling
it once; a chat now exists for this mode, so `TurnJob` gains its own state
to track for it, in place of nothing.

**D26 is withdrawn — including its "never a continuation" restriction, not
only its dispatch mechanism.** `TurnJob.handleCreationPending` no longer
special-cases a tool-carrying first turn at all: it always calls `StartChat`
(Issue #93's own change to it is fully reverted), which reaches the new
`runTurn` behavior exactly as any other call site does. Because
`ContinueTurn` shares the same `runTurn`, `handleReady`'s continuations gain
native tool execution too, as a direct consequence rather than new code —
resolving, not merely leaving in place, the "a link only ever reaches
`LinkReady` through a chat-managed StartChat... every later turn... stays on
legacy tool handling" limitation D26's own text documented and
`docs/operations/configuration.md` recorded. `TurnJob.complete`'s
`stateless` parameter, `handleCreationPendingStateless`,
`handleStreamTurnError`, and `streamExhaustionCategory` are removed, not
adapted: with the dispatch split gone, `complete` always takes the
`SetRemoteCurrent` branch its `else` case already had, and every failure
classification a tool-carrying turn can now produce is one
`handleCreateError`/`handleTurnError` already handles for a chat-managed
turn, unchanged.

**`domain.LinkStateless` and `OpenWebUIConversationLinkRepository.
MarkStateless` are removed from the Go layer.** No code path writes this
state any longer. Migration `0032`'s widened `state` CHECK constraint is
*not* reverted — AGENTS.md's "do not edit an applied migration" rule, and
the same precedent `0016`/`0027` already set for a since-superseded `CHECK`
value — so the column merely permits a value nothing writes anymore, the
same harmless state those earlier migrations' own now-unused values are in.
Any link that somehow reached `LinkStateless` before this decision is
already handled correctly by existing code with no migration or backfill:
`AllowsContinue`'s `state == LinkReady` check treats it, like `LinkFailed`/
`LinkDead`, as a dead branch a reply must start fresh from. In practice
none is expected to exist, for two independent, stacking reasons covering
the whole time `StreamTurn` was ever dispatched to: from Issue #93's own
original merge until Issue #116 fixed it, `ToolTurnTimeout` was never
wired into the turn client, so every call failed a
`context.WithTimeout(ctx, 0)` deadline before ever reaching the network;
after Issue #116's fix, Issue #120's capture shows the upstream connection
speaks the Responses API regardless of whether a tool was actually called
(`sse_passthrough_responses_api.sse.txt`, no `tool_ids` needed to
reproduce it), so the read never reaches the literal `data: [DONE]`
`StreamTurn` required for success either way.

**`Client.StreamTurn` and its entire SSE-reading mechanism —
`postStream`, `decodeStreamingCompletion`, `streamChunkBody`,
`streamCompletionsRequestBody`, `cancelOnCloseBody`,
`classifyStreamReadError` — are deleted, along with `Provider.StreamTurn`,
`StreamTurnRequest`, and `PhaseStream`.** Issue #120's finding is
structural, not a parsing gap this code could be salvaged to work around:
row 1 never reaches a branch that runs a tool, so nothing this mechanism
reads is ever the result of native tool execution. Its dedicated test
suites (7 cases in `internal/openwebui/turnjob_test.go`, 12 in
`internal/provider/openwebui/client_test.go`) are removed with it, replaced
by tests of `runTurn`'s new native branch and `awaitTurnDone` directly.

**UNVERIFIED (2026-09-08, per the owner's direction to implement Issue #123
without further real-instance access — the same "ship on a documented
assumption" precedent D22/D23/D24 already set): whether `chat_id` +
`stream:true` + `tool_ids`/`features` together (no `session_id`) actually
reaches `done: true` with real content, the way the plain, toolless capture
behind (h)'s row 3 does.** D24's own recommendation text reasoned from
source (`event_emitter`'s condition, the native loop's location, `Chats.
upsert_message_to_chat_by_id_and_message_id`'s unconditional persistence)
that it must, but no capture exists of this exact combination — only of row
3 without tool config, and of row 1 (no `chat_id`) with tool config, which
is the combination Issue #120 showed fails. This is the one load-bearing
assumption this decision rests on that Issue #120 did not itself verify;
the concrete failure mode if it is wrong is the same non-alarming one D24
already named for a different unverified assumption — polling never
observes `done: true`, `ToolTurnTimeout` elapses, and the turn ends
`ambiguous` (D6) rather than hanging, answering incorrectly, or posing a
security concern.

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
- **#75** gets its catalog-sync, mention-routing, and per-model tool/
  feature-resolution inputs from D18, D19, D20, and D21 — each amending,
  not replacing, the #52/#53/#72/#74 decisions it builds on: D18 amends D9
  (identity projection now spans every visible model, not one seeded
  row), D19 amends D5 (the branch rule's "reply to an earlier node"
  generalizes to "reply naming a different model"), D20 amends D16/D17
  (tool resolution is per model and per sync round, never a single
  deployment-wide override resolved once at boot), and D21 amends D17/D20
  again (web_search's own deployment-wide-only shape, confirmed once
  already by D20, is reversed into the same explicit-wins/unset-follows-
  model-default tri-state shape D17 originally gave `tool_ids`).
- **#81 (citation resolution) and #84 (chat title / viewer link)**, shipped
  as one PR on top of #75's branch, get D22 and D23. Both decisions carry an
  explicit unverified-assumption note (2026-09-08): the owner directed that
  they ship without further real-instance access, on the condition that
  every place the assumption matters says so in writing and describes what
  a wrong assumption would look like, rather than the D14 pattern of
  blocking on re-verification first. D22 amends nothing structurally (it is
  a new field, not a changed one) but extends D6's "provider response
  bodies are never logged or persisted verbatim" rule to a new response
  member (`sources[].document`). D23 is a narrow, named exception to D2 —
  the first ever granted — scoped to exactly one rendered link, gated by one
  new opt-in config key.
- **#93 (native multi-round tool execution)** gets D24, D25, and D26, on
  top of #75's per-model `tool_ids`/`features` resolution (D20/D21) and
  #81's `sources[]` handling (D22), which the new mode's own gaps are
  defined relative to. Like D22/D23, D24/D25 carry an explicit
  unverified-assumption note (2026-09-08) rather than blocking on further
  real-instance access; D26, the dispatch wiring itself, does not
  introduce a new such assumption — it only decides *when* the
  already-recorded uncertainty from D24 is reached. `TurnJob` now sends a
  branch's first turn through `StreamTurn` whenever its resolved
  `tool_ids`/`web_search` call for it, and every continuation through
  D16/D17's chat-managed path unchanged, exactly the split D26 describes
  — Issue #93's own staged migration plan, both stages landing in the
  same PR series. (Superseded by D27, Issue #123: D24's mechanism, D25,
  and D26's dispatch split are all retired — see D27 for what replaced
  them.)
- Every Open WebUI upgrade is a documentation event, not just a config change.
- Regeneration, remote branch management, and cancellation each still need
  their own contract work before they can be picked up; none of them is a
  leftover TODO inside the #52–#54 sequence. D8's "streaming stays out of
  the MVP" is *not* one of these anymore — D24 is exactly that contract
  work, for the one case (native tool execution) Issue #93 needed it for;
  D8 itself is otherwise unchanged, since the *chat-managed* path still has
  no streaming mode of any kind.
- This ADR merges the two ADR items the roadmap's OWUI-C section lists (the
  boundary ADR and the outbound-only lifecycle ADR) into one document, as
  agreed in #51. It is numbered 0005 because the roadmap's proposed
  `0003-openwebui-boundary.md` collides with the existing ADR-0003 and
  ADR-0004.
