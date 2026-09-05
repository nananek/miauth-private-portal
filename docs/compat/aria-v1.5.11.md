# Aria v1.5.11 compatibility contract

## Pinned target and observation record

- Roadmap issue: [Issue #2](https://github.com/nananek/miauth-private-portal/issues/2)
- Release target: [Aria v1.5.11](https://github.com/poppingmoon/aria/releases/tag/v1.5.11)
- Compatibility source snapshot: [`a66c9303995e7c964765cf382de6a9b0e3f4a3b6`](https://github.com/poppingmoon/aria/commit/a66c9303995e7c964765cf382de6a9b0e3f4a3b6)
- Pinned API client dependency: [`misskey_dart` 14176c515a005a9fb01d3e6365a49b5a5d387a92](https://github.com/poppingmoon/misskey_dart/commit/14176c515a005a9fb01d3e6365a49b5a5d387a92)
- Observation date: 2026-09-03 (Asia/Tokyo)
- Method: static source trace of the pinned Aria snapshot and its pinned
  `misskey_dart` request/response models; no credentials, personal data, or
  live Misskey account were used.

`LOCAL_ORIGIN` is the configured public origin of this Misskey-compatible
service and is the origin Aria calls. It is not supplied by an Aria request
and may not contain a path. Authentication approval is host-local (ADR-0002),
not delegated to another Misskey origin. External providers such as Open
WebUI have separate feature-specific origin policies outside this contract.

The release page identifies the v1.5.11 tag as `0f957e9`, while the roadmap
explicitly pins the source inspection snapshot to `a66c930…`, which is 11
commits and 491 files ahead of the tag. Both values are recorded instead of
silently substituting one for the other. The pinned `misskey_dart` dependency
is identical at both commits, so the wire-model surface this contract relies
on is unaffected by the drift. Compatibility regression tests must state
which snapshot they exercise.

The following labels are used throughout this document:

- **必要**: required for the Issue #2 user journeys or for Aria's first
  authenticated timeline load.
- **不要**: not required for the MVP contract; an implementation must not
  advertise it as supported merely because Aria contains a generic client
  feature.
- **要実機確認**: the call path is observed, but status codes, server-version
  behavior, or a response detail cannot be established without a real
  instance. It must be verified before the endpoint is treated as a release
  gate.

## User journeys traced

The contract covers these concrete Aria paths:

1. Add an account through MiAuth, then complete the check from the browser or
   the `aria://aria/miauth` deep link.
2. Load the home timeline, reload it, and paginate older notes.
3. Create a note or a reply and receive the created note.
4. Open a note, load its ancestor conversation, and load direct children.
5. Optionally use the access-token login fallback exposed by the login page.

The source locations used for the trace are:

- [`lib/repository/miauth_repository.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/repository/miauth_repository.dart)
- [`lib/provider/accounts_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/accounts_notifier_provider.dart)
- [`lib/provider/api/timeline_notes_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/timeline_notes_notifier_provider.dart)
- [`lib/provider/api/conversation_notes_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/conversation_notes_provider.dart)
- [`lib/provider/api/children_notes_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/children_notes_notifier_provider.dart)
- [`lib/provider/post_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/post_notifier_provider.dart)
- [`lib/provider/streaming/timeline_stream_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/streaming/timeline_stream_provider.dart)

Aria's client-side HTTP implementation is in the pinned dependency's
[`ApiService`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/services/api_service.dart):
it sends JSON `POST` requests, adds the access token as the `i` body field,
and removes null-valued fields before sending. Therefore, examples below show
`i` only where the account is authenticated; the token value is always
redacted.

## Endpoint classification and allowlist

| Endpoint | Classification | Why it is called | Authentication / scope boundary |
| --- | --- | --- | --- |
| `GET /miauth/{session}` | **必要** | Starts the Aria-facing account-add flow | Browser session; no API token. Creates a pending local session with an optional exact-match client return callback and requested permissions; only host-local CLI approval authorizes it |
| `POST /api/miauth/{session}/check` | **必要** | Completes the MiAuth flow | `{session}` is a bearer capability/correlation secret for this auth attempt; no `i` body field. It is not owner-binding or token-minting authentication |
| `POST /api/meta` | **必要** | Detects `features.miauth` and optionally canonicalizes the instance URI | Anonymous; no `i` body field |
| `POST /api/i` | **必要** | Access-token login fallback and authenticated account bootstrap | `i` token; local `read:account` equivalent |
| `POST /api/endpoints` | **要実機確認** | Aria probes endpoint availability before its edit path | The observed provider sends no token; exact anonymous behavior and response compatibility must be verified |
| `POST /api/notes/timeline` | **必要** | Home timeline initial load, reload, and older-page pagination | `i` token; local `read:notes` equivalent |
| `POST /api/notes/create` | **必要** | New note and reply creation | `i` token; local `write:notes` equivalent |
| `POST /api/notes/show` | **必要** | Note reload and opening a note not already cached | `i` token required; local `read:notes` equivalent |
| `POST /api/notes/conversation` | **必要** | Loads the ancestor chain for a thread | `i` token required; local `read:notes` equivalent |
| `POST /api/notes/children` | **必要** | Loads direct replies / quote-renotes for a thread | `i` token required; local `read:notes` equivalent |
| `POST /api/notes/update` | **不要** for Issue #2 | Only the edit path uses it; editing is not an Issue #2 acceptance journey | Do not advertise it until a later issue adds a contract |
| WebSocket `/streaming` timeline channel | **不要** for MVP; **minimal stub since Issue #41** | Provides live insertion, but HTTP load/reload/pagination are sufficient for MVP | A failed optional stream must not make HTTP timeline or post operations fail |

`/api/endpoints` is deliberately **要実機確認** rather than part of the
minimal release gate: the call is present in Aria's edit capability probe,
but create/reply/reload/thread journeys do not depend on it. The local server
must not claim edit support until the later endpoint decision is made.

For this contract, the exact effective local API scope set is
`read:account`, `read:notes`, and `write:notes`. The broad `permission` query
from Aria is recorded for compatibility but does not grant any additional
scope. `meta`, `endpoints`, the MiAuth page, and the MiAuth check use their
documented browser or anonymous/session capability and do not consume a local
API token. `/api/i` and every notes endpoint in the allowlist require a
locally issued API token; the token-login fallback accepts only such a local
token.
Any endpoint outside this allowlist returns an explicit,
consistently classified unsupported-endpoint error at the wire boundary; its
exact status and code remain an implementation contract-test decision.

## Shared request, authentication, and error rules

### Request transport

- Misskey API calls are `POST` with JSON content type.
- The API base is `LOCAL_ORIGIN` plus `/api/`. External providers use their
  own feature-specific origin policy.
- Authenticated calls carry `i: <local API token>` in the JSON body. Aria does
  not use an `Authorization` header for this client path.
- Null-valued optional request fields are omitted by the pinned client. A
  server must accept omission and must not require an optional field merely
  because the generated Dart model declares it.
- IDs are JSON strings. Do not infer ordering from the textual ID; the local
  service owns stable cursor semantics.

### Error shape

For typed `misskey_dart` calls, the pinned `ApiService` attempts to decode a
non-2xx response as:

```json
{
  "error": {
    "id": "synthetic-error-id",
    "code": "SOME_ERROR_CODE",
    "message": "Human-readable message",
    "kind": "client",
    "info": {}
  }
}
```

`id`, `code`, and `message` are strings. `kind` is optional and, when known,
is one of `client`, `server`, or `permission`; `info` is optional JSON object
data. If this shape is absent or malformed, Aria falls back to a transport
exception. The direct Dio calls used by `/api/meta` and MiAuth check do not
decode this wrapper. Exact status-code mapping, error codes, and pending
responses are **要実機確認**.

The local implementation must not log this error body when it could contain a
token, user content, or external-provider details. It should expose a stable local
error category and preserve the Misskey-compatible shape only at the wire
boundary.

## Observed endpoint contracts

### `POST /api/meta`

Aria first sends an anonymous empty object:

```json
{}
```

The login path reads only these fields. The local service returns its
configured `LOCAL_ORIGIN` (or omits `uri`); it must never return an arbitrary
external authority for Aria to save.

| Field | Type | Required for this path | Null / omission behavior |
| --- | --- | --- | --- |
| `features.miauth` | boolean | Yes to choose MiAuth; missing or false selects token-login fallback | Missing `features` or `miauth` is treated as not supported |
| `uri` | string | No | If present and parseable, its authority is used to canonicalize the saved server URL; missing, null, or invalid is ignored |

Other `meta` fields are not part of the Issue #2 login contract. A 2xx JSON
map is expected; exact error status and whether a target instance returns a
feature map or a legacy shape are **要実機確認**.

### `GET /miauth/{session}`

Aria generates an opaque UUID-like session ID and constructs:

```text
<LOCAL_ORIGIN>/miauth/<session>
  ?name=Aria
  &permission=<comma-separated-permission-values>
```

On Android it additionally sends `callback=aria://aria/miauth`. Other
platforms do not add a callback parameter in this source path. When present,
the value is a client return destination: the local service validates and
stores it, then redirects immediately to the exact callback with the original
route session. That redirect is not authorization; the host operator must
still approve the pending session. The exact permission values, in source
order, are:

```text
read:account,write:account,read:blocks,write:blocks,
read:drive,write:drive,read:favorites,write:favorites,
read:following,write:following,read:mutes,write:mutes,
write:notes,read:notes-schedule,write:notes-schedule,
read:notifications,write:notifications,read:reactions,write:reactions,
write:votes,read:pages,write:pages,write:page-likes,read:page-likes,
read:channels,write:channels,read:gallery,write:gallery,
read:gallery-likes,write:gallery-likes,read:flash,write:flash,
read:flash-likes,write:flash-likes,write:clip-favorite,read:clip-favorite,
write:report-abuse,read:chat,write:chat
```

The line breaks above are presentation only; the query value is one comma
separated string. The local service records requested permissions but grants
only its effective implemented scope set. Aria's broad request is not proof
that blocks, drive, pages, gallery, chat, or any other non-MVP API exists.
It is not authorization by itself and must not cause unsupported local
capabilities to be granted.

The response is an interactive HTML/browser flow and is not parsed by Aria.
The exact page status, consent behavior, and redirect timing are
**要実機確認**. The local server creates a pending session and may return
immediately to an exact-match-allowlisted client callback; authorization is
performed separately by the host operator. It rejects an unconfigured
callback.

The `{session}` route value is the same opaque Aria route session ID in the
`GET` URL and the `/api/miauth/{session}/check` path. It is a high-entropy
bearer capability/correlation secret for accessing the state of that one local
auth attempt, so it must not be logged or exposed in diagnostics. Possession
permits polling/checking that attempt only; it is not proof of owner identity,
owner binding, or authorization to mint a local API token. Those decisions
require the explicit SSH+CLI approval described by ADR-0002.

### `POST /api/miauth/{session}/check`

Aria sends an empty body and no `i` token; the route ID in the URL is the only
client-supplied handle:

```json
{}
```

The success shape that the source explicitly accepts is:

```json
{
  "ok": true,
  "token": "REDACTED_LOCAL_OR_UPSTREAM_TOKEN",
  "user": { "...": "UserDetailedNotMe" }
}
```

The `token` must be a JSON string and `user` must be a JSON object. The
success user object is decoded directly as `UserDetailedNotMe`; its minimum
fields are listed below. Any response not matching `ok: true` plus those two
types is a non-success response for this contract. Aria does not distinguish
pending from denial in this method; malformed JSON or a decode failure may be
surfaced as a transport/decode failure instead. The local server must bind the
returned token to this local session and local Owner actor.

The `token` in this response is a secret. It is shown in this example only as
the literal word `REDACTED_…`; real fixtures and logs must never contain it.
The check endpoint's exact pending body, status code, replay response, and
whether a consumed session remains readable are **要実機確認**; the local
service decision is one-time atomic consume as specified by ADR-0002.

#### `UserDetailedNotMe` minimum

The pinned generated parser requires these fields and types:

| Field | Type | Null / omission behavior |
| --- | --- | --- |
| `id` | string | Required and opaque |
| `username` | string | Required |
| `createdAt` | ISO-8601 string | Required and parseable as a date-time |
| `isBot` | boolean | Required |
| `isCat` | boolean | Required |
| `isLocked` | boolean | Required |
| `isSilenced` | boolean | Required |
| `isSuspended` | boolean | Required |
| `followersCount` | JSON integer for reliable decoding | Required by the model; retain as a non-negative count |
| `followingCount` | JSON integer for reliable decoding | Required by the model; retain as a non-negative count |
| `notesCount` | JSON integer | Required and decoded as an integer |

`name`, `host`, URLs, description, relationship fields, lists, and maps are
nullable or optional. `host: null` represents a local user; omission is also
accepted by the generated parser for nullable fields. Optional lists/maps
default to empty in the pinned parser. A fixture deliberately exercises both
omitted optional fields and explicit nulls:
[`user-detailed-not-me.json`](fixtures/user-detailed-not-me.json).
The pinned count converter is permissive at runtime but reliably preserves
JSON integers; a server must not use a string count as a substitute for a
missing or hidden value without a live compatibility test.

### `POST /api/i`

Aria has two observed uses:

1. The explicit token-login fallback sends `{"i":"<token>"}` and the account
   store reads `id` (string) and `username` (string) to save the account. For
   this local service, `<token>` is only a previously issued local API token;
   any other token is not a supported login path.
2. After login, the timeline loads the current account and decodes the full
   response as `MeDetailed`.

The first path therefore has this minimum response:

```json
{
  "id": "local-owner-id",
  "username": "owner"
}
```

For the second path, the pinned `MeDetailed` parser additionally requires
`createdAt` (ISO-8601 string), `isBot`, `isCat`, `isLocked`, `isSilenced`,
`isSuspended`, `notesCount`, `isModerator`, `isAdmin`, `alwaysMarkNsfw`,
`carefulBot`, and `autoAcceptFollowed` with their declared boolean/number
types. `followersCount` and `followingCount` use the pinned count converter.
Most other fields are nullable or have defaults, but the exact minimum needed
by the current timeline UI (including `policies`) is **要実機確認** against
the target instance. The local wire projection must not expose token hashes
or administrative secrets.

### `POST /api/endpoints`

The pinned dependency sends an empty request after removing the null token:

```json
{}
```

The expected typed response is a JSON array of strings, for example:

```json
["notes/create", "notes/show"]
```

Aria currently uses this only to decide whether its edit path may call
`notes/update`; edit support is outside Issue #2. Whether a target instance
allows this anonymous probe and its exact status/error behavior are **要実機確認**.

### `POST /api/notes/timeline` (home timeline)

For the initial authenticated home timeline, Aria sends the non-null subset of
the following request (the token is redacted):

```json
{
  "limit": 30,
  "withRenotes": true,
  "withFiles": false,
  "allowPartial": true,
  "i": "REDACTED_LOCAL_TOKEN"
}
```

`withRenotes` and `withFiles` reflect the user's Aria settings and can be
either boolean. For older-page pagination, Aria adds `untilId` with the last
loaded note ID. The generated request model also supports `sinceId`,
`sinceDate`, and `untilDate`; dates are epoch milliseconds, but the home path
does not send them during the normal initial load. Null fields are omitted.

The response is a JSON array of `Note` objects. An empty array is a valid
end-of-list response. Each note must satisfy the minimum Note contract below;
the server owns deterministic cursor ordering and must not rely on Aria's
lexical ID comparison.

Aria may also request the same endpoint with `sinceId` when filling the
timeline around a viewed note. The exact behavior of `allowPartial`, server
limit bounds, ordering, and same-timestamp cursor behavior are **要実機確認**.

### `POST /api/notes/create` (note and reply)

Aria sends an authenticated request with null fields removed. The relevant
normal-note/reply shape is:

```json
{
  "visibility": "public",
  "text": "synthetic fixture text",
  "localOnly": false,
  "fileIds": [],
  "replyId": "parent-note-id",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

For a top-level note, `replyId` is omitted. For a reply it is a string
containing the parent note ID. Aria also supports `visibleUserIds`,
`reactionAcceptance`, `renoteId`, `channelId`, `poll`, and `scheduledAt`; they
are outside the minimum Issue #2 create/reply contract and must be treated as
optional until separately verified. `scheduledAt` is epoch milliseconds in
the request model.

The success response must be an object containing a `createdNote` object:

```json
{
  "createdNote": { "...": "Note" }
}
```

If `createdNote` is absent, Aria treats the result as a null response and
falls back to its local draft representation; malformed `createdNote` data can
still cause a decode failure. The exact validation/error code for empty text,
oversize content, invalid reply IDs, and unsupported visibility is **要実機確認**.

### `POST /api/notes/show`

Request:

```json
{
  "noteId": "note-id",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

`noteId` is a required opaque string. The response is one `Note` object, not
an envelope. A missing/hidden note and its exact status/error body are
**要実機確認**; do not turn a hidden note into fabricated success.

### `POST /api/notes/conversation`

Request used by the thread view:

```json
{
  "noteId": "note-id",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

`limit` and `offset` exist in the generated model but Aria does not send them
in this path. The response is a JSON array of `Note` objects representing the
ancestor chain. Ordering, whether the subject note is included, and hidden
ancestor behavior are **要実機確認**; the local domain contract must define
one deterministic ordering before implementing the endpoint.

### `POST /api/notes/children`

Request used by the thread view:

```json
{
  "noteId": "note-id",
  "depth": 1,
  "untilId": "older-child-id",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

`untilId` is omitted on the first request and added for pagination. The
generated model also supports `limit`, `sinceId`, `sinceDate`, and `untilDate`,
but Aria's direct-children provider sends only `noteId`, `depth: 1`, and the
optional `untilId`. The response is a JSON array of `Note` objects. Aria
requests another page when the result is short and stops on an empty result;
server default limits, ordering, and visibility errors are **要実機確認**.

## Minimum Note contract

The pinned generated `Note` parser requires only the following top-level
fields:

| Field | Type | Null / omission behavior |
| --- | --- | --- |
| `id` | string | Required and opaque |
| `createdAt` | ISO-8601 string | Required and parseable as a date-time |
| `user` | object | Required; decoded as `UserLite` |
| `userId` | string | Required and opaque |

The nested `UserLite` requires `id` and `username` strings. Its `host` is
nullable. `text`, `cw`, `replyId`, `renoteId`, `channelId`, `uri`, `url`, and
`myReaction` are nullable; `visibility` and `reactionAcceptance` are nullable
enums. Counts default to zero, maps/lists (`reactions`, `reactionEmojis`,
`emojis`, `fileIds`, `files`, `mentions`, `visibleUserIds`, and
`reactionAndUserPairCache`) default to empty, and `localOnly` defaults to
false. Nested `reply`, `renote`, `channel`, and `poll` are optional but must
be fully valid when present.

The redacted fixture is
[`fixtures/note.json`](fixtures/note.json). It intentionally contains no
access token, real user content, real instance host, or personal identifier.

### Note.text provenance markers

Misskey's `Note` carries no field of its own to say how a note originated, so
Issue #13 (AC5) distinguishes the four non-`user_post` entry kinds Aria can
see by folding a fixed marker into the wire-visible `text` itself:

| Entry kind | `text` shape | Where the marker is added |
| --- | --- | --- |
| `user_post` | Verbatim user text, never altered | n/a |
| `llm_reply` | `"[reply]\n\n" + body` | `internal/httpserver`'s `wireText`, at wire-projection time only |
| `llm_follow_up` | `"[follow-up question]\n\n" + body` | `internal/httpserver`'s `wireText`, at wire-projection time only |
| `news` (RSS/Atom) | `"[news[: <source display name>]] <title>\n[<provenance URL>]\n\n" + body` | `internal/ingest`'s `composeExternalBody`, folded into the stored `Body` itself when the adapter sets a `Title` |
| `mail` (IMAP) | `"From: ...\nSubject: ...\nDate: ...\n\n" + snippet` | `internal/mailfetch`, folded into the stored `Body` itself; IMAP items never set `Title`, so `composeExternalBody` is a no-op for them |

The `llm_reply`/`llm_follow_up` markers are presentation-only: the underlying
`domain.Entry.Body` and the `LLMGeneration.Body` audit record both keep the
provider's unmarked output, so the generation log always reflects what the
provider actually produced. The `news`/`mail` markers, by contrast, are part
of the persisted `Body` (there is no separate wire/domain split for ingested
content), matching `internal/mailfetch`'s pre-existing header-block
convention that this issue extends to RSS/Atom rather than replacing.

Aria's own classification results (`internal/domain.LLMClassificationRepository`)
are never exposed through any Note field — no marker is needed for them, since
no Aria/Misskey-compatible HTTP endpoint exposes them at all (see
`docs/operations/configuration.md`'s "Review/notebook/unresolved queries").

## Pagination and reload semantics

| Journey | Aria request cursor | Client behavior | Local contract decision |
| --- | --- | --- | --- |
| Home initial load | No cursor; `limit: 30` | Adds returned notes to cache | Return newest-to-oldest page with deterministic ordering |
| Home older page | `untilId` = last loaded note ID | Requests again; filters duplicate/older items client-side | Resolve the opaque note ID through the stored ordering/cursor; use an explicit timestamp/opaque-ID tie-breaker, never lexical ID order or offset-only pagination |
| Home around a viewed note | `sinceId` or `untilId` in auxiliary provider | Fills before/after viewed note | Preserve cursor inclusivity/exclusivity in contract tests |
| Conversation | No cursor in Aria path | Loads one ancestor list | Define ordering and subject inclusion before implementation |
| Children | Optional `untilId`; `depth: 1` | Loads until an empty page | Define deterministic ordering and hidden-note behavior |
| Note reload | `noteId` only | Replaces cached note | Return one note or a typed not-found/hidden error |

The client source uses lexical comparisons in a few UI pagination helpers, but
that is an Aria implementation detail, not a license for the service to infer
chronology from an ID. Stable cursor behavior is a local requirement.

## Streaming decision

Streaming is **not an MVP requirement**. The observation shows that Aria's
timeline is initially populated and paginated through HTTP
`/api/notes/timeline`; its WebSocket timeline channel only inserts newly
arriving notes and provides a live UX enhancement. Post success, reload,
restart persistence, reply creation, and thread viewing can all be verified
without a stream.

MVP therefore uses poll/reload semantics as the release gate. A future
streaming adapter may be added behind a capability check, but a stream outage
must not make the HTTP timeline, post, or thread endpoints unavailable.

### Issue #41: minimal stub, not a capability

Before Issue #41, `GET /streaming` was unregistered, so Aria's WebSocket
upgrade request received net/http's default 404, which fails the handshake
outright. Because this endpoint requires no `read:account`-scoped token
check to *reach* that failure, Aria attempted this on every timeline tab
open; `misskey_dart`'s `StreamingService._connect` retries once after five
seconds and then surfaces the resulting exception as a Riverpod `AsyncError`
to whatever UI is watching the stream — a real, observed error report, not
merely a theoretical gap in this contract's Non-goals.

Issue #41 adds a `read:account`-authenticated `GET /streaming` that
completes the WebSocket handshake, sends a `{"type":"connected", ...}` ack
for `connect`, tracks (but never reads back) `disconnect`/`subNote`/
`unsubNote` state, and pings every `StreamPingInterval` (default 30s) to
stay alive through an idle-timeout intermediary. It still pushes no real
note/notification event — the "not an MVP requirement" decision above is
unchanged in substance. This is a wire-availability fix for the symptom
above, not a promotion of streaming to a supported capability; do not treat
a successful handshake as evidence that live timeline updates work.

## Requirement traceability

The table maps Issue #1 requirements to roadmap children. Issue #2 freezes the
contracts; the later issue owns implementation and evidence.

| Issue #1 requirement | MVP owner issues | Future owner / note |
| --- | --- | --- |
| Only the allowed Misskey user can add an account through MiAuth | #5, #7, #28, #13 | #2 ADR and contract are prerequisites; #28 replaced the upstream-Misskey-owner check with host-local SSH+CLI approval (ADR-0002) |
| Aria post, reload, reply, thread view, and restart persistence | #3, #4, #6, #7, #13 | Requires storage, domain semantics, transport, and E2E evidence |
| Posts succeed while LLM is stopped; recovery reprocesses reply/classification | #4, #8, #9, #10, #13 | Durable job intent is part of the same post transaction |
| Distinguish LLM reply and follow-up; answer stays in one thread | #6, #9, #10, #13 | Generated records remain separate from source text |
| Persist subject/field/keywords/tags/summary/questions/open items/relations/learning target/priority/notebook/review metadata separately from source | #4, #9, #10, #13 | No LLM output may overwrite user-authored text |
| Show RSS/Atom news and read-only IMAP mail with provenance | #6, #11, #12, #13 | Fetching remains isolated from user-post durability |
| Backup/restore, secret/token operations, security regression, and Aria v1.5.11 E2E | #3, #4, #5, #13 | #13 is the release gate |
| No full Misskey compatibility or federation | #7, #13 | Explicit non-goal; no Future issue promotes it |
| No custom web UI | #7, #13 | Aria remains the client; no separate UI is planned |
| No general user management or multi-user behavior | #5, #7, #13 | Multi-user tenancy is Future #16 |
| No PostgreSQL | #4, #13 | PostgreSQL backend/migration path is Future #15 |
| No AppFlowy/notebook export in MVP | #4, #13 | Notebook/AppFlowy export is Future #14 |
| No arbitrary crawling, SMTP, mail mutation, or autonomous unlimited tools | #11, #12, #13 | Additional source adapters are Future #17; mail stays read-only |

## Issue #7 implementation notes

Issue #7 implements the endpoint handlers this document specifies. The
decisions below fix behavior this document left 要実機確認 (needs
real-instance verification); **no real Aria/Misskey end-to-end
verification has been performed for this issue** — the 要実機確認 labels
above remain accurate and unchanged. These decisions must be revisited
against a real Aria client before any release gate treats them as
verified.

- **Error shape**: implemented as `{"error":{"id","code","message","kind","info"}}`
  with locally-chosen `code` strings (`INVALID_PARAM`, `NO_SUCH_NOTE`,
  `UNSUPPORTED_FEATURE`, `AUTHENTICATION_FAILED`, `INTERNAL_ERROR`), all
  returned with a 400 status except authentication failures (401) and
  internal errors (500). These are this implementation's contract, not a
  confirmed match to a real Misskey instance's codes/statuses.
- **Missing, archived, and hidden notes** are never distinguished:
  `/api/notes/show`, the conversation ancestor chain, the children list,
  and `/api/notes/create`'s `replyId` all treat an unknown, archived, or
  hidden note ID identically (`NO_SUCH_NOTE` for the requested/reply-target
  ID itself; silently excluded when it appears inside a list). Replying to
  a hidden/archived note is rejected rather than silently succeeding, even
  though `timeline.CreateReply`'s own parent lookup does not filter by
  visibility — the httpserver handler checks this itself, the same way it
  does for every note-reading endpoint.
- **Home timeline pagination** is newest-first via a dedicated
  `EntryRepository.ListTimelineDesc` (see internal/domain/entry.go), never
  the oldest-first `ListTimeline` cursor. `limit` defaults to 30 and
  clamps to 100; `untilId` resolves through the referenced entry's
  `(created_at, id)` and pages strictly older. `sinceId`/`sinceDate`/
  `untilDate` are not implemented (accepted-and-ignored is not applicable
  since they are simply never read).
- **Conversation ordering**: the ancestor chain is oldest-first (root,
  then its child, ..., then the subject's direct parent), excluding the
  subject note itself.
- **Children pagination**: continues `ListChildren`'s existing
  oldest-first order (no separate newest-first children query exists);
  `untilId` resumes after the matching child. The lookup searches
  `ListChildren`'s full result, including archived/hidden children, so an
  anchor that was visible on an earlier page but has since been
  hidden/archived still resolves to its correct position and pagination
  keeps surfacing later visible children — mirroring `/api/notes/timeline`'s
  `GetEntry`-based `untilId` lookup, which likewise ignores visibility when
  resolving the cursor. Only an `untilId` that matches no child at all
  (a stale/unknown ID, never part of this note's children) yields an empty
  page rather than restarting from the first page, matching the home
  timeline's unknown-`untilId` handling.
- **`/api/i`** always returns the `MeDetailed` superset regardless of
  which of Aria's two call sites is asking (its extra required fields
  parse successfully as the token-login fallback's minimal `{id,
  username}` shape too). `isModerator`/`isAdmin` are `true` (the single
  owner is this deployment's only login-capable, administrator-equivalent
  actor); `alwaysMarkNsfw`/`carefulBot`/`autoAcceptFollowed` are `false`.
- **`notesCount`** is now real on both `/api/i` and
  `/api/miauth/{session}/check`'s `UserDetailedNotMe`, counting every
  entry (including archived/hidden ones) authored by the actor.
- **`/api/notes/create`** rejects `visibleUserIds`, `reactionAcceptance`,
  `renoteId`, `channelId`, `poll`, `scheduledAt`, a non-empty `fileIds`,
  and any `visibility` other than `"public"` with an explicit
  `UNSUPPORTED_FEATURE` error rather than silently ignoring them.
- **`/api/notes/children`**'s `depth` request field is accepted but not
  otherwise enforced: `ListChildren` already returns only direct children
  regardless of the requested depth.
- **Real Aria end-to-end verification substitute**: this issue's
  acceptance criteria call for a real Aria v1.5.11 login → post → reload
  → reply → conversation run. That has not been performed. In its place,
  `contract/aria_client` (a Dart package depending on the pinned
  `misskey_dart` commit above, run via `make contract-test` /
  `scripts/run-contract-tests.sh`) decodes real responses from a running
  `bin/server` with the actual generated parser Aria itself uses, and
  asserts on the decoded fields (created-note round trip, timeline
  ordering and `untilId` paging, `notesCount` accounting,
  `NO_SUCH_NOTE`/`MisskeyException` decoding). This is judged sufficient
  to close Issue #7 without a real device run: the specific residual risk
  the wire-shape 要実機確認 labels above exist for — Aria's real decoder
  rejecting a response this service considers valid — is exactly what
  this suite exercises. What it cannot cover is anything about the real
  Aria *application* beyond its HTTP client library: UI rendering, its
  MiAuth browser/deep-link and host-operator approval UX end to end, and
  any real Misskey server's actual behavior where this document still
  says 要実機確認. Those remain open until a real device run happens.

## Issue #13 implementation notes

Issue #13 is the MVP release gate: it closes the remaining acceptance
criteria across restart persistence, LLM outage recovery, provenance
distinguishing, backup/restore, security regression coverage, and
operator documentation, without changing the endpoint handlers Issue #7
implemented. As with Issue #7, **no real Aria/Misskey end-to-end
verification has been performed for this issue either** — every 要実機確認
label in this document remains accurate and unchanged, and the
decisions below only extend Issue #7's substitute evidence strategy
rather than replacing it.

- **Real Aria end-to-end verification substitute, extended**: following
  the same precedent as Issue #7's implementation notes above,
  `contract/aria_client` gains a restart-persistence scenario
  (`restart_persistence_test.dart`): `scripts/run-contract-tests.sh` now
  creates a note, kills and relaunches `bin/server` against the same
  `DB_PATH`, and passes the note's id/text to the suite via
  `TEST_PRE_RESTART_NOTE_ID`/`TEST_PRE_RESTART_NOTE_TEXT` so the pinned
  decoder — against a server that was actually restarted as a process,
  not merely reopened in-process — confirms the note (and the local API
  token obtained before the restart) both survive. Reply/conversation
  round-trip coverage already existed
  (`notes_conversation_test.dart`'s root/child/grandchild ancestor-chain
  assertion) and needed no further extension for this issue.
- **Provenance marker evidence, deliberately split across two layers**:
  the "Note.text provenance markers" table above is pinned at the unit
  level by `internal/httpserver/noteapi_wire_test.go` and
  `internal/ingest/service_test.go`. Whether the marker actually reaches
  a real HTTP response is instead proven by
  `internal/integration`'s `TestServerE2E_PostSucceedsWhileLLMDownAndReplyRecoversWithMarker`
  (a real `bin/server` subprocess, a real `/api/notes/create` call, and
  a real `/api/notes/children` call asserting the decoded `text` starts
  with `"[reply]\n\n"`), not by an addition to
  `contract/aria_client`. Driving an `llm_reply`/`llm_follow_up`
  marker to completion needs a controllable fake LLM provider and an
  asynchronous job to actually finish; `internal/provider/openai`'s
  HTTP client (unlike RSS/IMAP's `internal/ingest/safehttp`) has no SSRF
  restriction blocking a loopback `LLM_BASE_URL`, which makes an
  in-process Go `httptest.Server` a much more direct way to get real
  end-to-end evidence than teaching the Dart/bash contract harness to
  also stand up a fake provider and poll an async job to completion. The
  `news`/`mail` markers are lower-risk by comparison — they are folded
  directly into the persisted `Body` at ingestion time
  (`internal/ingest/service_test.go` already covers `composeExternalBody`
  against real `FetchedItem` values) rather than at wire-projection
  time, and driving them through a real RSS/IMAP fetch end to end is
  outside this issue's remaining scope.
- **Clean-environment deploy lifecycle** (AC1): `internal/integration`'s
  `TestServerE2E_MigrateReadyRestartShutdown` builds the real
  `cmd/server` binary, runs it against a fresh SQLite database with no
  pre-existing `.env`, and asserts migrate → `/readyz` 200 → SIGTERM →
  graceful exit, then repeats first boot's readiness/shutdown cycle a
  second time against the same already-migrated `DB_PATH` to prove the
  restart path (an idempotent migration re-run, not just first boot)
  also works. This complements `internal/httpserver/run_test.go`, which
  already covers `Run`'s shutdown behavior in detail in-process but
  never builds `cmd/server`'s own configuration/migration/actor-seeding/
  job-manager wiring around it.
- **LLM outage and recovery** (AC4): the same
  `TestServerE2E_PostSucceedsWhileLLMDownAndReplyRecoversWithMarker`
  above also carries this issue's AC4 evidence: `/api/notes/create`
  returns 200 while the fake LLM provider answers every request with
  503 (internal/provider/openai's `categoryServerError`, retryable), and
  once the provider is flipped to succeed, the pending `llm_generation`
  job's own backoff/retry loop (internal/jobs) picks it up and completes
  it with no `jobsctl retry` call — proving both "posts succeed while
  the LLM is stopped" and "recovery reprocesses the job automatically"
  over the real durable-job path rather than a unit test double.
- **Backup/restore** (AC6): `cmd/backupctl` (`backup`/`verify`, both
  built on `modernc.org/sqlite`'s `VACUUM INTO`, no external `sqlite3`
  binary) and its automated restore drill
  (`cmd/backupctl/main_test.go`'s
  `TestRestoreDrill_BackupSurvivesSourceDestructionAndRestoresRelationships`)
  were added in a prior PR on this same issue; see
  [`docs/operations/backup-restore.md`](../operations/backup-restore.md).
- **Security regression** (AC8): `docs/operations/security-regression.md`
  maps every AC8 bullet to its evidencing test, added in a prior PR on
  this same issue. Request rate/concurrency limiting is confirmed
  intentionally absent from the application layer and delegated to the
  reverse proxy (see
  [`docs/operations/runbook.md`](../operations/runbook.md)'s "Request
  rate and concurrency limits" section); no new middleware was added for
  it.
- **Operator documentation** (AC7, AC11): day-two operations
  (incident response, secret rotation, revoking access, database/file
  permissions, reverse proxy/TLS termination, log retention) live in
  [`docs/operations/runbook.md`](../operations/runbook.md), added in a
  prior PR on this same issue. The README's "Known limitations" section
  and this document's non-goals below cover what remains permanently
  out of scope rather than merely deferred.

## Non-goals and implementation boundary

This document does not implement endpoint handlers, a database, migrations,
owner binding, token storage, an LLM, ingestion, or a web UI. Those are
deliberately assigned to later issues. In particular, the following remain
out of scope for this compatibility target:

- full Misskey compatibility and federation;
- a custom web UI;
- general user registration, deletion, roles, or multi-user tenancy;
- PostgreSQL;
- AppFlowy/notebook export;
- arbitrary web crawling, SMTP, mail mutation, and unlimited autonomous
  LLM/tool execution.

No Aria or `misskey_dart` source is copied into this repository. The documents
record only observed request construction, response parsing, and the security
properties that the local implementation must preserve.
