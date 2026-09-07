# Open WebUI 0.11.3 compatibility contract

## Pinned target and observation record

- Umbrella issue: [Issue #50](https://github.com/nananek/miauth-private-portal/issues/50);
  this document is [Issue #51](https://github.com/nananek/miauth-private-portal/issues/51) (OWUI-C)
- Roadmap: [`docs/roadmap/openwebui.md`](../roadmap/openwebui.md)
- Boundary decisions: [ADR-0005](../decisions/0005-openwebui-boundary.md)
- Pinned image: `ghcr.io/open-webui/open-webui@sha256:1a6399d237dc392a2313e0ca826020b3fd5d22536357840eb63393d18dc8b924`
  (image created 2026-09-04T23:47:30Z; `org.opencontainers.image.revision` label
  `0a7c15832fb30b1903753e83f81dc7d27e5b0944`)
- Reported version: `GET /api/version` → `{"version":"0.11.3","deployment_id":""}`
- Observation date: 2026-09-06 (Asia/Tokyo)
- Method: the pinned image was run locally in Docker with `ENV=dev`,
  `ENABLE_OPENAI_API=true`, `ENABLE_OLLAMA_API=false`, and `OPENAI_API_BASE_URL`
  pointed at a purpose-built OpenAI-compatible mock backend on the host. The
  mock records every request it receives and can be told, through the prompt
  text, to reproduce a specific upstream failure (`[[status:NNN]]`,
  `[[retry-after]]`, `[[malformed]]`). No production instance, third-party
  model provider, real credential, or personal data was involved, so every
  observation below is reproducible offline.

`ENV=dev` only makes `/openapi.json` and `/docs` reachable; in the default
(production) mode those paths return the SPA's HTML instead. API behavior is
unaffected by that switch, so the endpoint observations hold for a production
deployment of the same digest.

The raw capture set (52 numbered request/response files, the full 485-path
`openapi.json`, the curl driver scripts, and the mock backend) is kept on the
ccserver file board as `owui-verification-fixtures.tar.gz`
(id `17ec60cc-311d-424b-ba34-5e6c4e9a7515`) and is deliberately **not**
committed. The redacted subset this contract cites lives in
[`fixtures/openwebui/`](fixtures/openwebui/); see "Fixtures" below for the
index and the redaction rules.

The following labels are used throughout this document:

- **必要**: the outbound adapter calls this endpoint at runtime. It is part of
  the pinned contract and of the credential's minimum endpoint allowlist.
- **運用のみ**: used by a human operator to provision or rotate the integration.
  The adapter never calls it at runtime.
- **不要**: the endpoint exists on the target and is deliberately not used. It
  is listed so that a later reader does not mistake its absence for an
  oversight.
- **要実機確認**: the call path or setting is known to exist, but its behavior
  could not be established with the local mock setup. It must be verified
  against the real target instance before the feature is enabled.

### Not verified

These are explicitly outside the observation record. Do not treat any of them
as contract:

- socket.io (WebSocket) streaming delivery, which is how the real UI receives
  chunks for a chat-managed `stream:true` request;
- the production instance's version, configuration, and whether it matches
  this digest's behavior — `parent_id`-driven chat management is a relatively
  new feature and is not assumed to exist on older builds;
- any rate limit, request/response/stream size bound, or server-side timeout
  (no default API rate limit was observed, but "not observed" is not "absent");
- the procedure for granting a non-admin account access to a base model
  (`POST /api/v1/models/model/access/update` is the candidate endpoint,
  **要実機確認**);
- `ENABLE_API_KEYS_ENDPOINT_RESTRICTIONS` / `API_KEYS_ALLOWED_ENDPOINTS`
  behavior — both settings exist, neither was exercised;
- `/api/chat/completed` outlet semantics beyond "it echoes the posted
  body", and attachment/file handling;
- a **production** instance's tool/web-search behavior: this document's
  Issue #74 observation record below used a locally run pinned instance,
  a real self-hosted SearXNG backend, and one real Python Tool — not the
  production deployment. A real MCP Tool Server, tool-call error handling
  against a real (non-mock) model backend, and
  `CHAT_RESPONSE_MAX_TOOL_CALL_ITERATIONS`-bounded loop latency remain
  **要実機確認**, tracked in Issue #50.

### Observation record: Issue #74 Phase 0 (tool/web-search execution)

- Date: 2026-09-07 (Asia/Tokyo)
- Method: the same pinned image and digest as above, run locally via
  `docker compose` alongside two more local-only containers: a purpose-
  built OpenAI-compatible mock backend (extended from the one above to
  also emulate Open WebUI's internal task calls — the tool-decision call
  and the search-query-generation call — and native OpenAI-style
  `tool_calls` responses), and an unmodified `searxng/searxng` instance
  with its JSON API enabled, configured as Open WebUI's `WEB_SEARCH_ENGINE`.
  One real Python Tool (a calculator — the exact example Issue #74's own
  acceptance criteria cite) was registered through the "Tools" admin
  feature (`POST /api/v1/tools/create`). No production instance, third-
  party model provider, real credential, or personal data was involved.
- **Native function calling (this adapter's pre-#74 default) reproduces
  Issue #74's bug exactly.** A `stream:false` completions request
  carrying `tool_ids` with no `params.function_calling` puts the tool
  spec on the request as an OpenAI-style `tools` array in the same call;
  when the (mocked) model responded the way a real tool-calling model
  does — `tool_calls` set, `content` empty — `GET /api/v1/chats/{id}`
  showed the assistant message present but **`done:false` forever**
  (polled repeatedly with no change). This matches
  `non_streaming_chat_response_handler` reading only
  `choices[0].message.content`, confirmed against the pinned backend's
  own source (revision `0a7c15832fb30b1903753e83f81dc7d27e5b0944`, the
  same one `Pinned image` above resolves to).
- **`features.web_search=true` alone (no `tool_ids`, no legacy param) is
  a silent no-op through this adapter**, not a hang: Open WebUI's own
  source skips the forced web-search injection specifically because
  native function calling is available, and the alternative (a builtin
  `web_search` tool offered to the model) requires `metadata.session_id`
  — a UI-session concept this adapter's API-key calls never set. No
  SearXNG request was observed, and the saved assistant message carried
  no search sources at all, while still completing `done:true`.
- **`params.function_calling: "legacy"` fixes both.** With `tool_ids` set
  and legacy mode requested, the real calculator Tool was genuinely
  invoked server-side — verified two ways: the completions response's
  `sources` field carried `"The result of 12 * 7 is 84."` (a value only
  the real Python tool's own `eval()` could have produced, not the mock),
  and the Tool's own code independently wrote that same expression/result
  pair to a file inside the container. With `features.web_search=true`
  and legacy mode requested, real SearXNG queries were observed (engine
  activity in SearXNG's own logs at matching timestamps) and real scraped
  page content reached the completions response's `sources` field. Both
  cases completed `done:true`, and in both cases
  `history.currentId`/`current_message_id` named this adapter's own
  client-chosen assistant message id — the same correlation D2/D6 already
  assume, unaffected by legacy mode.
- A plain turn using neither `features` nor `tool_ids` carries no
  `params` key at all, confirmed against
  `TestClient_StartChat_LinearAgainstFixtures`/
  `TestClient_ContinueTurn_LinearAgainstFixtures`'s fixture-equality
  checks, which still pass unmodified.

## Endpoint classification and allowlist

| Endpoint | Classification | Why | Boundary note |
| --- | --- | --- | --- |
| `POST /api/v1/chats/new` | **必要** | Creates the persistent chat and fixes its `chat_id` *before* any generation runs | Body is a free-form chat object; the server assigns the id |
| `POST /api/chat/completions` | **必要** | Executes one turn (`StartChat` and `ContinueTurn` both map here) | The only generating call. Chat management is opted into by sending a `parent_id` key |
| `GET /api/v1/chats/{id}` | **必要** | Reads back `done`/`error`/`currentId` to classify a turn's outcome | Read of *this* bridge's own chat only; never used to browse or import history |
| `GET /api/models` | **必要** | Catalog sync's one call (Issue #75, `Client.ListModels`): reconciles every model this account can see, including each one's own `info.meta.toolIds`/`defaultFeatureIds` for per-model tool resolution (superseding Issue #74's single-model `GetModelTools`, retired) | Also the health signal for "this credential can see at least the configured default model" |
| `GET /api/v1/tools/` | **必要** | Called once per catalog sync round (Issue #75, superseding Issue #74's once-at-boot call): resolves which tool ids this account may actually invoke, to filter every model's own `toolIds` fail-closed (`Client.ListAccessibleTools`) | Never per-turn |
| `GET /api/v1/auths/` | **必要** | Credential liveness / whoami probe | Cheapest call that proves the API key is still valid |
| `GET /api/version` | **必要** | Records the running version for drift detection against this document | Unauthenticated |
| `POST /api/v1/auths/signup` | **運用のみ** | Creates the dedicated account (first account becomes `admin`) | One-time provisioning |
| `POST /api/v1/auths/admin/config` | **運用のみ** | Sets `ENABLE_API_KEYS=true` | Admin-only; required before a key can exist |
| `POST /api/v1/users/default/permissions` | **運用のみ** | Sets `features.api_keys=true` | Admin-only; required *in addition to* the above |
| `POST` / `GET` / `DELETE /api/v1/auths/api_key` | **運用のみ** | Issue, read back, and revoke the credential | Rotation is "issue a new one"; see "Authentication" |
| `GET /api/models/base`, `GET /api/v1/models`, `GET /api/v1/configs/models` | **運用のみ** | Discovering the right model id when configuring the workspace | Not called at runtime; the id is configuration |
| `POST /api/v1/models/create`, `POST /api/v1/models/model/access/update` | **要実機確認** | Custom-model presets and granting a non-admin account model access | Needed only if the dedicated account is not an admin |
| `GET /api/tasks/chat/{chat_id}`, `POST /api/tasks/chat/{chat_id}/stop` | **不要** | Only meaningful for background (streaming) generation | Streaming is out of MVP scope; `CancelTurn` is not implemented (ADR-0005 D1) |
| `POST /api/v1/chats/{id}` (update), `POST /api/v1/chats/{id}/messages/{message_id}`, `.../event` | **不要** | Would let the bridge rewrite remote history | Edit/delete synchronization is a non-goal |
| `POST /api/v1/chats/{id}/fork`, `/clone`, `/compact` | **不要** | Remote-side branch management | The bridge maps one local branch to one remote chat instead (ADR-0005 D5) |
| `GET /api/v1/chats/`, `/all`, `/search`, `POST /api/v1/chats/import` | **不要** | Listing, searching, and importing existing chats | Explicit roadmap non-goals |
| `POST /api/chat/completed` | **不要** | Outlet hook; observed to echo the posted body | Not needed for a buffered turn |

The credential's minimum runtime endpoint set is therefore the seven
**必要** rows (Issue #74 adds `GET /api/v1/tools/` to what was six). If
`API_KEYS_ALLOWED_ENDPOINTS` is used to restrict the key (**要実機確認**),
that is the list to allow.

## Shared request, authentication, and error rules

### Request transport

- All calls are HTTP with JSON bodies; generating calls are `POST`.
- The base URL is the workspace's configured origin. There is no path prefix
  beyond the endpoint paths above.
- Authentication is `Authorization: Bearer <api key>`. Every endpoint in the
  allowlist declares the `HTTPBearer` scheme in the target's own OpenAPI
  document except `GET /api/version`, which declares no security at all. The
  API key is the `sk-`-prefixed string (35 characters as issued) returned by
  `POST /api/v1/auths/api_key`. A session JWT works on the same endpoints but
  is not what this integration uses.
- The `/api/chat/completions` request body is **not** described by the target's
  own OpenAPI document: it is declared as a free-form object
  (`{"additionalProperties": true, "type": "object"}`, see
  [`openapi-0.11.3.excerpt.json`](fixtures/openwebui/openapi-0.11.3.excerpt.json)).
  Every field this contract relies on (`chat_id`, `parent_id`, `id`,
  `user_message`, `background_tasks`) is therefore an *empirical* observation
  of this digest, not a published interface. This is the main reason the
  target is pinned by digest and must be re-verified on upgrade
  (ADR-0005 D14).

### Authentication and credential lifecycle

- The first account created through `POST /api/v1/auths/signup` becomes
  `role: admin`; `ENABLE_SIGNUP` then flips off and a second signup is
  rejected with 403. Signup returns a JWT (`token`, `token_type: "Bearer"`,
  `expires_at`; default `JWT_EXPIRES_IN=4w`).
- API keys are **off by default**: `POST /api/v1/auths/api_key` returns 403
  `API key creation is not allowed in the environment.` until *both* the admin
  config `ENABLE_API_KEYS=true` (`POST /api/v1/auths/admin/config`; the env var
  is `ENABLE_API_KEYS` — `ENABLE_API_KEY` is not recognized) **and** the
  default permission `features.api_keys=true`
  (`POST /api/v1/users/default/permissions`) are set. This holds for admin
  accounts too.
- One account holds exactly one key. Calling `POST /api/v1/auths/api_key`
  again issues a new key and the previous one starts failing with 401
  immediately. `GET` reads the current key back; `DELETE` revokes it.
  **Rotation is therefore re-issue plus configuration update, with no overlap
  window** — plan the swap accordingly.
- Chats are owned by their creating account. Another account reading the
  owner's chat gets **401**, not 404
  ([`error_401_chat_not_found.json`](fixtures/openwebui/error_401_chat_not_found.json)),
  and another account attempting to continue the owner's chat gets 400
  `Model not found`
  ([`error_400_model_not_found.json`](fixtures/openwebui/error_400_model_not_found.json)),
  because a non-admin account cannot see the base model by default. That
  rejected continuation persists no message.

### Error shape

Errors are JSON objects with a `detail` field — a string, or an array of
FastAPI/pydantic validation entries for a 422. The status codes that matter:

| Situation | Status | Body | Fixture |
| --- | --- | --- | --- |
| No credential | 401 | `{"detail":"Not authenticated"}` | [`error_401_not_authenticated.json`](fixtures/openwebui/error_401_not_authenticated.json) (synthetic) |
| Invalid/rotated-out key | 401 | `{"detail":"Your session has expired or the token is invalid. Please sign in again."}` | [`error_401_invalid_token.json`](fixtures/openwebui/error_401_invalid_token.json) (synthetic) |
| Chat not visible to this account | 401 | `{"detail":"We could not find what you're looking for :/"}` | [`error_401_chat_not_found.json`](fixtures/openwebui/error_401_chat_not_found.json) |
| Unknown or invisible model, or `model` omitted | 400 | `{"detail":"Model not found"}` | [`error_400_model_not_found.json`](fixtures/openwebui/error_400_model_not_found.json) |
| Schema violation on a typed body (e.g. `POST /api/v1/chats/new` without `chat`) | 422 | `{"detail":[{type,loc,msg,input}, …]}` | [`error_422_validation.json`](fixtures/openwebui/error_422_validation.json) (synthetic) |
| Upstream model error, legacy path | **400** | `{"detail":"<upstream text, verbatim>"}` | [`error_legacy_upstream_500.json`](fixtures/openwebui/error_legacy_upstream_500.json), [`…_401.json`](fixtures/openwebui/error_legacy_upstream_401.json), [`…_429.json`](fixtures/openwebui/error_legacy_upstream_429.json) |
| Upstream returns non-JSON, legacy path | **200** | a bare JSON *string* (no `choices`) | [`error_legacy_upstream_malformed.json`](fixtures/openwebui/error_legacy_upstream_malformed.json) |
| Upstream model error, chat-managed path | **200** | `null` (4 bytes) | [`error_chat_managed_upstream_500_response.json`](fixtures/openwebui/error_chat_managed_upstream_500_response.json) |

A 422 can only come from an endpoint with a typed body. `/api/chat/completions`
declares a free-form object, so it never validates the fields this contract
sends — an omitted or misspelled `model` surfaces as the 400 above, and every
other malformed field is simply forwarded or ignored.

Three consequences drive the adapter's error handling (ADR-0005 D6):

1. **HTTP status cannot classify an upstream failure.** Upstream 401, 429, and
   500 all arrive as HTTP 400 on the legacy path. `Retry-After` is **not**
   propagated: the 429 capture's response headers carry only `date`, `server`,
   `content-length`, `content-type`, and `x-process-time`.
2. **On the chat-managed path a failure looks like success.** HTTP 200 with a
   `null` body is the failure signal; the outcome is only readable by fetching
   the chat and inspecting the assistant message's `done` and `error` fields.
3. **Upstream error text is passed through verbatim and is never redacted by
   Open WebUI.** The mock embedded the pseudo-secret `sk-mock-upstream-secret`
   in its error message and it appears unchanged in both the legacy `detail`
   and the persisted `message.error.content`. The adapter must discard provider
   error text and keep only a category.

## Observed endpoint contracts

### `POST /api/v1/chats/new` (必要)

Request is `ChatForm`: `{chat: <free-form object>, variables?, folder_id?}`.
The `chat` object is stored as sent; sending an **empty history** is accepted
and is what this integration does, so the chat id exists before any generation:

```json
{"chat":{"id":"","title":"bridge-precreated","models":["mock-model"],"params":{},
         "history":{"messages":{},"currentId":null},"messages":[],"tags":[],
         "timestamp":1788672375000}}
```

([`chats_new_empty_request.json`](fixtures/openwebui/chats_new_empty_request.json))

The response is `ChatResponse`
([`chats_new_empty_response.json`](fixtures/openwebui/chats_new_empty_response.json)):
`{id, user_id, title, chat, updated_at, created_at, share_id, archived, pinned,
meta, variables, folder_id, tasks, summary, current_message_id, context_usage}`.

- `id` is a **server-assigned UUID** — this is the `remote_chat_id`. Sending
  `chat.id: ""` is what makes the server assign it; the inner `chat.id` stays
  `""` in storage and must not be read as the chat id.
- `current_message_id` is `null` for a fresh empty chat.
- The declared 200 schema is `ChatResponse | null`, so a `null` body is a
  documented possibility on this endpoint and on the GET below.

`title` is **not** stable: the first turn overwrites it with the user message's
content even when `background_tasks.title_generation` is `false` (the pinned
capture goes from `"bridge-precreated"` to `"precreated turn 1"`). Never use
`title` as correlation data.

### `POST /api/chat/completions` (必要)

Chat management is opted into per request by the presence of the `parent_id`
**key** (the target's own code comments call it "parent_id signals intent"):

| Request shape | Effect |
| --- | --- |
| no `parent_id` key, no `chat_id` | legacy: generate and return, persist nothing. An unknown `chat_id` is ignored the same way — no chat is created and no error is raised |
| no `parent_id` key, but `chat_id` + `id` naming an existing message | legacy generation, yet the content **is** written into that message — and its `parentId` is reset to `null` (see (d)) |
| `parent_id: null`, no `chat_id` | create a new chat and persist this root turn |
| `parent_id: null` + `chat_id` | persist this root turn into the named existing chat |
| `parent_id: "<assistant id>"` + `chat_id` | persist a follow-up turn under that parent |

**(a) The recommended `StartChat` shape** — against a chat created by the
previous endpoint
([`completions_start_request.json`](fixtures/openwebui/completions_start_request.json)):

```json
{"model":"mock-model","stream":false,
 "chat_id":"<remote_chat_id>","parent_id":null,"id":"<assistant message uuid>",
 "user_message":{"id":"<user message uuid>","parentId":null,
                 "childrenIds":["<assistant message uuid>"],
                 "role":"user","content":"precreated turn 1","timestamp":1788672375},
 "messages":[{"role":"user","content":"precreated turn 1"}],
 "background_tasks":{"title_generation":false,"tags_generation":false,"follow_up_generation":false}}
```

The response is a plain OpenAI-compatible completion
([`completions_start_response.json`](fixtures/openwebui/completions_start_response.json)):
`{id, object:"chat.completion", created, model, choices:[{index, message:{role,
content}, finish_reason}], usage:{prompt_tokens, completion_tokens,
total_tokens}}`. Its `id` (`chatcmpl-…`) is the **upstream provider's**
completion id, not an Open WebUI message id; it must not be stored as
`remote_message_id`.

**(b) The recommended `ContinueTurn` shape** — same endpoint with `parent_id`
and `user_message.parentId` both set to the previous assistant message id
([`completions_continue_request.json`](fixtures/openwebui/completions_continue_request.json),
[`completions_continue_response.json`](fixtures/openwebui/completions_continue_response.json)).

Setting `parent_id` alone is **not** enough: if `user_message.parentId` is left
unset, the stored user message keeps `parentId: null` and the previous
assistant's `childrenIds` is not updated, so each turn lands as its own
disconnected pair instead of extending the chain.
[`duplicate_delivery_chat.json`](fixtures/openwebui/duplicate_delivery_chat.json)
is that failure preserved: none of that chat's six turns set
`user_message.parentId`, and the five follow-ups among them each named an
existing assistant message in `parent_id`, yet all six user messages still sit
at `parentId: null` — six orphan pairs rather than one branch. The client must
set both fields — which is what [`chat_after_continue.json`](fixtures/openwebui/chat_after_continue.json)
shows working.

**(c) `messages` is entirely caller-supplied.** The server does not rebuild
context from the stored history: the mock backend received exactly the array
that was sent, in order, with no system prompt added and the model name
unchanged. Omitting `messages` sends only `user_message` to the backend. The
pinned continue capture makes this visible — the harness deliberately sent
`{"role":"assistant","content":"(prev reply)"}` as the middle turn instead of
the real stored reply, and the backend received that placeholder, proving the
server neither substituted nor validated the history.

**(d) There is no server-side tree validation, and a chat-bound legacy request
damages the tree.** A `parentId` naming a message that does not exist in the
chat is accepted with HTTP 200 and stored dangling. Worse, a request that omits
the `parent_id` key but still names `chat_id` + an existing `id` writes the
generated content into that message **and resets its `parentId` to `null`**,
discarding a link the client had already stored:
[`chat_after_completion_without_parent_id.json`](fixtures/openwebui/chat_after_completion_without_parent_id.json)
is a chat that was created with the assistant node correctly parented to the
user node, read back after exactly one such call with the parent link gone.
The adapter must therefore always send the `parent_id` key — omitting it is not
a safe "read-only" mode. Path eligibility, cycle rejection, and orphan handling
remain entirely this service's responsibility (ADR-0005 D4).

**(e) Duplicate delivery overwrites, it does not duplicate.** Re-sending a
request with the same `chat_id`/`parent_id`/`user_message.id`/`id` calls the
model again and **replaces** the existing assistant message's content in place.
No node is added: `chat.history.messages` is a map keyed by the
client-supplied message id, so reusing the ids can only overwrite. In the
capture, the re-run returned `"mock reply #13 …"`
([`duplicate_delivery_response.json`](fixtures/openwebui/duplicate_delivery_response.json))
and that exact text is what the pre-existing assistant node
`e8676c52-557e-4e4b-a92d-bc029c2d3be4` holds afterwards, still as the single
child of the same user message `3447161d-…`
([`duplicate_delivery_chat.json`](fixtures/openwebui/duplicate_delivery_chat.json));
the raw archive's earlier read of the same chat shows `"mock reply #4 …"` at
that id. (That earlier read holds 4 nodes and this one holds 12, but the eight
added nodes come from four unrelated experiments run between the two reads — a
third turn, a sibling branch, and the two upstream-error turns — not from the
duplicate.)
Open WebUI has **no idempotency key**, so suppressing duplicates is local work
(ADR-0005 D7).

**(f) Creating a chat through completions alone hides the id.** With
`parent_id: null` and no `chat_id`, a chat *is* created and persisted, but the
buffered response is a bare completion with no `chat_id` in the body or headers
([`completions_only_newchat_response.json`](fixtures/openwebui/completions_only_newchat_response.json)
vs. the chat it silently created,
[`completions_only_newchat_chat.json`](fixtures/openwebui/completions_only_newchat_chat.json)).
The only way to recover the id from a buffered call would be to diff the chat
list, which this integration refuses to do. (The `stream:true` + `session_id`
form in (h) *does* return the new chat's id in-band, but it delivers no content
over HTTP, so it is not an alternative.) This is why the chat is pre-created
(ADR-0005 D3).

**(g) `background_tasks`** accepts
`{title_generation, tags_generation, follow_up_generation}`; setting all three
to `false` suppresses those extra model calls but not the title overwrite
described above.

**(h) `stream: true` has three different behaviors**, none of which delivers
chunks usefully over plain HTTP:

| Request | Response | Persistence |
| --- | --- | --- |
| no `chat_id` | 200 `text/event-stream`, the upstream SSE passed straight through ([`sse_passthrough_finish.sse.txt`](fixtures/openwebui/sse_passthrough_finish.sse.txt)) | none |
| `id` + `session_id`, with or without `chat_id` | 200 JSON `{"status":true,"task_ids":["…"],"chat_id":"…"}` returned immediately ([`streaming_task_response.json`](fixtures/openwebui/streaming_task_response.json), captured from the no-`chat_id` variant, so its `chat_id` is the id of the chat the call had just created) | generated in the background and saved |
| `chat_id` + `id`, no `session_id` | 200 JSON `null` ([`streaming_no_session_response.json`](fixtures/openwebui/streaming_no_session_response.json)) | generated in the background and saved |

The chunks for the latter two go out over socket.io, not HTTP. The passed-
through SSE carries **no sequence number or chunk id** — the `id` field repeats
the same `chatcmpl-…` value on every chunk — so duplicate and out-of-order
detection is impossible from the wire alone (see the synthetic
`sse_duplicate_chunk` / `sse_out_of_order_chunk` fixtures). Also note that in
streaming mode a re-run **appends** to an existing message's content, where
buffered mode replaces it — this was observed during the capture session by
re-running a `stream:true` request against an already-filled message id, but
no fixture of the appended state was retained, so it rests on the observation
record alone (the weakest-evidenced streaming statement here). Streaming stays
out of MVP (ADR-0005 D8).

**(i) `sources[]` (Issue #81) and `title_generation`'s real completion timing
(Issue #84) — both partially 要実機確認.**

The 2026-09-07 real-instance capture behind Issue #81 additionally found a
top-level `sources` array on the completions response whenever
`params.function_calling=legacy` ((c) above) actually triggered a tool call
or web search — absent otherwise. Two shapes were observed, both
synthesized (not the real capture, which is not committed) into
[`completions_response_sources_tool.json`](fixtures/openwebui/completions_response_sources_tool.json)
and
[`completions_response_sources_websearch.json`](fixtures/openwebui/completions_response_sources_websearch.json):

- **Tool execution** (`tool_result: true`): `source.name` names the tool;
  `metadata[].parameters` is the call's own arguments; `document[]` is the
  tool's raw JSON output.
- **Web search** (`tool_result` absent, `distances` present): `source.
  {name, type, urls, queries}`; `metadata[]` has one entry per result chunk,
  each `{title, description, source}` (`source` being that chunk's URL);
  `document[]` is the page-text chunks themselves.

`internal/provider/openwebui/client.go`'s `normalizeSources` turns each
`sources[]` array element into one `openwebui.Source` record and never
decodes `document[]` into memory at all (ADR-0005 D22). `sources` is
decoded as raw JSON first and only then parsed into typed records
(`decodeSources`); a shape this adapter cannot parse degrades to "no
citations for this reply," never to a failed turn — the turn's own
content already decoded successfully.

**要実機確認, not yet resolved as of this PR (2026-09-08):** the real
capture observed exactly **one** `sources[]` element, and the model's own
`[1]` citation marker referred to it. Whether the model's `[n]` markers
always correspond 1:1 with `sources[]` array order for **more than one**
source — versus, say, one `sources[]` element grouping several web-search
result chunks under a single citation number — was never exercised. Per
the owner's 2026-09-08 direction, Issues #81/#84 ship on this explicit,
documented assumption rather than waiting on further real-instance access;
see ADR-0005 D22 and `openwebui.Source`'s own doc comment for what a wrong
assumption would look like (a cosmetic footnote mismatch, never a
`document[]` leak or a failed turn). Also **要実機確認**: whether
`GET /api/v1/chats/{id}` ever carries `sources` for a completed turn — this
adapter assumes not, and never depends on retrieving it a second time.

Separately, **要実機確認**: whether `background_tasks.title_generation:
true` ((g) above) resolves synchronously (the chat's `title` field is
already updated by the time `runTurn`'s own post-completion
`GET /api/v1/chats/{id}` lookup runs) or asynchronously (visible only on
some later call, if ever). This PR does not add a second poll to chase a
title that has not appeared yet; if generation turns out to be
asynchronous, the observable effect is simply that most replies show no
generated title (Issue #84's `"[reply]"` marker, unchanged), not an
incorrect one. See ADR-0005 D23 and `StartChatRequest.
EnableTitleGeneration`'s doc comment.

### `GET /api/v1/chats/{id}` (必要)

Returns the same `ChatResponse` shape. The tree lives in
`chat.history.messages`, keyed by message id
([`chat_after_start.json`](fixtures/openwebui/chat_after_start.json),
[`chat_after_continue.json`](fixtures/openwebui/chat_after_continue.json)):

```json
{"id":"<message id>","parentId":"<id|null>","childrenIds":["<id>", …],
 "role":"user|assistant","content":"…","model":"mock-model","timestamp":1788672375,
 "done":true,
 "output":[{"type":"message","id":"msg_…","status":"completed","role":"assistant",
            "content":[{"type":"output_text","text":"…"}]}],
 "usage":{"prompt_tokens":10,"completion_tokens":12,"total_tokens":22,
          "input_tokens":10,"output_tokens":12}}
```

- `done: true` on the assistant message is the completion flag. A failed turn
  has `done: false`, `content: ""`, and an `error: {content: "<upstream text>"}`
  object ([`error_chat_managed_message_state.json`](fixtures/openwebui/error_chat_managed_message_state.json)).
  A **user** message written by a completions turn never carries `done`; the
  one place a user message does carry it is the `/fork` copy
  ([`fork_response_reference.json`](fixtures/openwebui/fork_response_reference.json)),
  which this integration never calls.
- `chat.history.currentId` and the top-level `current_message_id` both point at
  the newest assistant message and stay in sync in every capture. They advance
  to a **failed** message as well: in
  [`duplicate_delivery_chat.json`](fixtures/openwebui/duplicate_delivery_chat.json)
  both fields name the upstream-429 turn's assistant node, which carries
  `done: false` and an `error`. So `currentId` is "the newest turn", never "the
  newest *successful* turn" — it may be adopted as `remote_current_id`, and
  used as the next turn's `parent_id`, only after the `done: true` check
  below.
- `chat.messages` (the flat array) is **not** maintained by the server — it
  stays `[]` for a bridge-created chat. Only `chat.history` is authoritative.
- This is the endpoint that turns the chat-managed path's ambiguous HTTP 200
  into a decision: `error` present → `failed`; `done: false` without `error` →
  `ambiguous`; `done: true` → success.
- The response's top-level `title` (Issue #84) is what
  `internal/provider/openwebui/client.go`'s `chatResponseBody.resolvedTitle`
  reads, filtered against `createChat`'s own `"bridge-precreated"`
  placeholder — see (i) above and ADR-0005 D23 for the synchronous/
  asynchronous timing this adapter does not assume either way about.

### `GET /api/models` (必要) and the model-discovery endpoints (運用のみ)

- `GET /api/models` → `{"data":[{id, name, owned_by, connection_type, info, …}]}`,
  everything the calling account can see. The configured `external_model_id` is
  one of these `data[].id` values. A built-in `arena-model` entry is present
  alongside real models, distinguished by both its literal id and its own
  `arena: true` field (`fixtures/openwebui/models_response.json`'s second
  entry) — `Registry.eligibleRemoteModels` (Issue #75) checks both, the id as
  the primary signal and the flag as defense in depth against a future rename
  of that literal.
- Issue #75's catalog sync (`Registry.SyncCatalog`, `Client.ListModels`) is
  this call's only production caller: once per sync round, it reconciles
  every eligible entry into `openwebui_models` and caches each one's own
  `info.meta.toolIds`/`defaultFeatureIds` for per-model tool resolution — the
  same fields Issue #74's now-retired `GetModelTools` read for a single
  resolved-at-boot model.
- `GET /api/models/base` narrows this to connection-provided base models;
  `GET /api/v1/models` lists workspace custom models (presets created with
  `POST /api/v1/models/create`), whose ids are equally valid as
  `external_model_id`.
- `GET /api/v1/configs/models` returns UI defaults
  (`DEFAULT_MODELS`, `DEFAULT_PINNED_MODELS`, `MODEL_ORDER_LIST`, …) as
  comma-separated strings. The bridge names the model on every request, so it
  does not read this.
- **Visibility is access-controlled.** A non-admin account sees an empty
  `/api/models` until it is granted access, and every generating call then
  fails with 400 `Model not found`. Granting that access is **要実機確認**.
  Catalog sync treats a successful empty response as a genuine "nothing
  visible" snapshot (deactivating every previously active model), never as
  evidence of an access grant being revoked versus an outage — those two
  cases are indistinguishable from this endpoint alone, and a transport/HTTP-
  level failure is handled as an outage instead (existing registry left
  untouched; see `internal/openwebui/catalog.go`'s own doc comment).
- **Hidden/visibility fields beyond access control are still 要実機確認.**
  The one real capture this document pins
  (`fixtures/openwebui/models_response.json`) shows one real model and the
  built-in arena entry; it does not exercise a model an admin has hidden or
  disabled by some other means. `eligibleRemoteModels` therefore does not
  filter on any field beyond blank/duplicate ids and the arena signal above —
  a model this filter is unsure about is kept rather than silently dropped,
  pending real-instance evidence one way or the other.

### `GET /api/v1/auths/` (必要) and the credential endpoints (運用のみ)

`GET /api/v1/auths/` is a whoami that works with the API key and is the
cheapest credential-liveness probe. The `api_key` endpoints and the two admin
settings that enable them are described under "Authentication and credential
lifecycle" above.

### `GET /api/tasks/chat/{chat_id}` (不要, reference)

While a background (streaming) generation runs it returns
`{"task_ids":["…"]}`, and `[]` once finished. `POST /api/tasks/chat/{chat_id}/stop`
exists next to it. Both are only meaningful for streaming, and cancellation
safety was not verified, so neither is used (ADR-0005 D1, D8).

### `POST /api/v1/chats/{id}/fork` (不要, reference)

`{message_id}` produces a new chat titled `"<original> (fork)"` containing the
`parentId` chain up to that message
([`fork_response_reference.json`](fixtures/openwebui/fork_response_reference.json)).
Because it copies only along `parentId`, a chain broken by a dangling parent
copies just the reachable part — the capture forked a chat whose assistant had
`parentId: null` and got a 2-node result. This is a plausible-looking
implementation of "reply to an earlier node" that this integration
deliberately rejects: it would mean reading and duplicating remote history
(ADR-0005 D5).

## Opaque ID semantics

Every remote identifier is an opaque string. None of them is a local identity,
an ordering key, or an authorization input (ADR-0005 D2).

| Remote value | Where it comes from | Stored as | Notes |
| --- | --- | --- | --- |
| `ChatResponse.id` | server-assigned at `POST /api/v1/chats/new` | `remote_chat_id` | UUID; known before the first generation |
| completions `user_message.id` / `id` | **client-generated UUIDs** in the request | `remote_message_id` | The server stores them verbatim as the user/assistant message ids |
| `message.parentId` | client-supplied | `remote_parent_id` | Not validated by the server |
| `history.currentId` = `current_message_id` | server-maintained | `remote_current_id` | Points at the newest assistant message, including a failed one — only adopt it after confirming `done: true` |
| completion `id` (`chatcmpl-…`) | the upstream model provider | *not stored as an id* | Keep at most as provenance; it is not an Open WebUI identifier |

Because message ids are client-generated, a turn's correlation keys exist
before the request is sent, and a lost response never leaves the local side
without an id to reconcile against.

## Nullable and unknown fields

Fields observed to be absent or `null` in at least one capture, which the
adapter must therefore tolerate:

- on a message: `done` (absent entirely on user messages), `output`, `usage`,
  `error` (present only on a failed turn), `model`, and `parentId` (`null` at
  a root — and, per (d) above, possibly `null` where a parent was expected).
  `childrenIds` is the opposite case: always present, never `null`, but an
  empty array on a leaf, so it must be read as "possibly empty" rather than
  "possibly missing". `modelName` and `modelIdx` appear only when a client
  wrote them into the history itself; the server does not add them;
- on a chat response: `share_id`, `folder_id`, `tasks`, `summary`,
  `current_message_id`, and `context_usage`;
- inside the free-form `chat` object: `params`, which a completions-created
  chat omits while carrying a `files` key the pre-created ones do not have
  ([`completions_only_newchat_chat.json`](fixtures/openwebui/completions_only_newchat_chat.json)
  against [`chat_after_start.json`](fixtures/openwebui/chat_after_start.json));
- on a model entry: `info`, which the recorded `/api/models` shape gives as an
  object or `null` — `fixtures/openwebui/models_response.json` (Issue #74
  Phase 0) captured one real model with `info` present and the built-in
  arena entry with a much sparser one, but not a model an admin has hidden
  or disabled by some other means, so that specific corner remains this
  endpoint's weakest-evidenced part (see "GET /api/models" above);
- whole-response `null`: both `POST /api/v1/chats/new` and
  `GET /api/v1/chats/{id}` declare `ChatResponse | null` as their 200 schema,
  and the chat-managed completions path returns a literal `null` on failure.

Three more `ChatResponse` fields must be tolerated on the schema's authority
rather than on a capture's: `pinned` is declared `boolean | null` (every
capture has `false`), and `meta` and `variables` are optional with a `{}`
default (every capture has `{}`). `models` is **not** a `ChatResponse` field
at all — it lives inside the free-form `chat` object and was present in every
capture.

Fields named in the roadmap that this capture never produced — `followUps` and
`selectedModelId` — are deliberately **not** listed above. They may exist in
other configurations, but nothing here observed them, and an unobserved field
is handled by the general rule below rather than by a contract claim.

Unknown fields are ignored. The adapter decodes only the fields named in this
document and must not fail on additions, because the completions request/response
surface is not covered by the target's published OpenAPI schema at all.

## Rate limits, sizes, and timeouts

**TBD (see #50):** no numbers are recorded here, and none should be invented.

- No default API rate limit was observed on any endpoint, and Open WebUI does
  not document one for these paths. "Not observed" is not "absent" — a
  production deployment may sit behind a reverse proxy that adds one.
- Upstream timeout settings (`AIOHTTP_CLIENT_TIMEOUT` and relatives) exist on
  the target but were not exercised.
- No request, response, or stream size bound was probed.
- The adapter must therefore impose its **own** client-side timeout and
  response-size bound rather than relying on the server's, exactly as
  `internal/ingest/safehttp` already does for RSS (ADR-0005 D11). The concrete
  values are a configuration decision for #52/#53.

## Fixtures

All fixtures live in [`fixtures/openwebui/`](fixtures/openwebui/).

### Redaction rules

- The capture used a throwaway instance and a mock model backend, so no real
  credential, JWT, API key, email address, or personal message ever entered the
  fixtures.
- The account UUID is replaced with the literal `"user-redacted"` in every
  `user_id` field. It is the only value that was rewritten.
- Chat and message UUIDs are **kept as captured**. They are identifiers from a
  destroyed throwaway instance, not secrets, and keeping them is what lets the
  fixtures correlate with each other (the same `chat_id` runs through
  `chats_new_*`, `completions_*`, and `chat_after_*`).
- The pseudo-secret `sk-mock-upstream-secret` is **kept deliberately**. The
  mock backend embedded it in its upstream error text specifically so that
  #53's redaction tests have a string to assert on: an adapter that leaks
  provider error text will leak *this* token into a log or an error body, and
  the test can catch it. It is not a credential and never was one.
- Synthetic fixtures are marked as such in the table below; the SSE ones carry
  a leading `:` comment line naming what was changed and why.

### Index

| Fixture | Source | What it pins |
| --- | --- | --- |
| `chats_new_empty_request.json` | reconstructed from the capture script, with the captured ids | The empty-chat creation body |
| `chats_new_empty_response.json` | observed | `ChatResponse` for a fresh chat; server-assigned `id`, `current_message_id: null` |
| `completions_start_request.json` | reconstructed from the capture script, with the captured ids | The recommended `StartChat` request |
| `completions_start_response.json` | observed | Buffered OpenAI-shaped response; `chatcmpl-…` id is the upstream's |
| `chat_after_start.json` | observed | Persisted U1→A1 pair, `done`/`output`/`usage`, `currentId` |
| `completions_continue_request.json` | reconstructed from the capture script, with the captured ids | The recommended `ContinueTurn` request; also shows the caller-supplied `"(prev reply)"` context placeholder |
| `completions_continue_response.json` | observed | Buffered follow-up response |
| `chat_after_continue.json` | observed | The 4-node linear tree with both `parentId` links wired |
| `chat_branch_same_chat_reference.json` | observed | Two children under one parent in a single remote chat, and `currentId` moving to the newest — the shape this integration does **not** use |
| `completions_only_newchat_response.json` | observed | Completions-only creation returns no `chat_id` |
| `completions_only_newchat_chat.json` | observed | …yet a chat was created and persisted |
| `chat_after_completion_without_parent_id.json` | observed | A `chat_id`+`id` request with no `parent_id` key writes the content **and** clears the assistant's `parentId` |
| `error_legacy_upstream_500.json` | observed | Upstream 500 → HTTP 400, upstream text verbatim (contains the pseudo-secret) |
| `error_legacy_upstream_401.json` | observed | Upstream 401 → HTTP 400 as well |
| `error_legacy_upstream_429.json` | observed | Upstream 429 → HTTP 400; no `Retry-After` on the response |
| `error_legacy_upstream_malformed.json` | observed | Non-JSON upstream → HTTP 200 whose body is a JSON string |
| `error_chat_managed_upstream_500_response.json` | observed | Chat-managed failure → HTTP 200 `null` |
| `error_chat_managed_message_state.json` | observed (single message extracted from the chat) | The failed message: `done:false`, `content:""`, `error.content` |
| `duplicate_delivery_response.json` | observed | The re-sent turn's response |
| `duplicate_delivery_chat.json` | observed | The re-run's text landed on the pre-existing assistant node `e8676c52…` (still one child of the same user message); no node was added for it |
| `sse_passthrough_finish.sse.txt` | observed | Real SSE: role delta, content deltas, `finish_reason: "stop"`, `data: [DONE]`; no per-chunk sequence |
| `sse_duplicate_chunk.sse.txt` | **synthetic**, derived from the above | A repeated chunk, indistinguishable on the wire |
| `sse_out_of_order_chunk.sse.txt` | **synthetic**, derived from the above | Transposed chunks, silently corrupting naive concatenation |
| `sse_truncated_no_done.sse.txt` | **synthetic**, derived from the above | Response loss: no finish event, no `[DONE]` |
| `streaming_task_response.json` | observed | `{"status":true,"task_ids":[…],"chat_id":…}` for `stream:true` + `session_id`, here creating a new chat, whose id therefore does come back in-band |
| `streaming_no_session_response.json` | observed | Literal `null` for `stream:true` without `session_id` |
| `fork_response_reference.json` | observed | What `/fork` returns, and how a broken chain truncates it |
| `error_401_not_authenticated.json` | **synthetic**, from the recorded response text | Missing-credential shape |
| `error_401_invalid_token.json` | **synthetic**, from the recorded response text | Invalid/rotated-out key shape |
| `error_401_chat_not_found.json` | observed | Another account's chat is 401, not 404 |
| `error_400_model_not_found.json` | observed | Unknown/invisible model |
| `error_422_validation.json` | **synthetic**, from the recorded response shape | Pydantic validation array, shown for a typed body (`POST /api/v1/chats/new` without `chat`) — completions never returns one |
| `models_response.json` | observed (Issue #74 Phase 0, real pinned instance + a workspace model configured with `toolIds`/`defaultFeatureIds`) | `GET /api/models`'s `data[].{id,name,info.meta.toolIds,info.meta.defaultFeatureIds}` shape `Client.ListModels` reads for catalog sync (Issue #75), including the built-in arena entry's `arena: true` field |
| `tools_response.json` | observed (Issue #74 Phase 0, the real calculator Tool registered through the admin "Tools" feature) | `GET /api/v1/tools/`'s per-tool `id` shape `Client.ListAccessibleTools` reads |
| `openapi-0.11.3.excerpt.json` | observed, filtered | 13 paths and 21 schemas out of 485/314: the schemas those paths reference, plus the request bodies of the declined and unverified endpoints whose paths were left out (`ForkForm`, `EventForm`, `MessageForm`, `ModelForm`). `info.version` is FastAPI's default `0.1.0`, **not** the Open WebUI version |

## Non-goals and implementation boundary

This contract covers an **outbound** bridge only. Reading, listing, searching,
importing, forking, or reconciling existing Open WebUI chats is excluded, as is
converting a remote message tree into a local reply tree. The single read this
integration performs, `GET /api/v1/chats/{id}`, is a turn-outcome check on a
chat this service itself created — never a history source.

Also excluded, with the endpoints that would implement them listed in the
allowlist table above so a later reader does not re-derive them: remote-side
edit and delete, remote branch management, regeneration, socket.io streaming,
cancellation, tool/function/MCP execution, and any form of federation. Open
WebUI credentials never authenticate Aria, mint a local API token, or widen a
local scope.

The version boundary is a digest, not a version range. `parent_id`-driven chat
management is not part of the target's published API schema, so an upgrade must
be re-verified against this document's "Observation record" before the feature
is re-enabled (ADR-0005 D14).
