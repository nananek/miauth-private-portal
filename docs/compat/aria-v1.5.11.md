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

The contract distinguishes two origins. `LOCAL_ORIGIN` is the configured,
public origin of this Misskey-compatible service and is the origin Aria calls.
`IDENTITY_ORIGIN` is the fixed upstream Misskey origin used for owner
verification and upstream Misskey provider requests. Neither value is
supplied by an Aria request, and neither may contain a path. The two origins
may be different and must not be substituted for one another. An external
provider such as Open WebUI has a separate workspace-scoped origin policy and
is outside this Aria compatibility contract.

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
| `GET /miauth/{session}` | **必要** | Starts the Aria-facing account-add flow | Browser session; no API token. The local session is bound to the internal callback under `LOCAL_ORIGIN`, an optional exact-match client return callback, and requested permissions; any owner verification uses `IDENTITY_ORIGIN` |
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
| WebSocket `/streaming` timeline channel | **不要** for MVP | Provides live insertion, but HTTP load/reload/pagination are sufficient for MVP | A failed optional stream must not make HTTP timeline or post operations fail |

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
token and never accepts, exchanges, or persists an upstream Misskey token.
Any endpoint outside this allowlist returns an explicit,
consistently classified unsupported-endpoint error at the wire boundary; its
exact status and code remain an implementation contract-test decision.

## Shared request, authentication, and error rules

### Request transport

- Misskey API calls are `POST` with JSON content type.
- The API base is `LOCAL_ORIGIN` plus `/api/`; upstream Misskey requests use
  `IDENTITY_ORIGIN` only inside the provider boundary. Other external
  providers use their own feature-specific origin policy.
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
token, user content, or upstream details. It should expose a stable local
error category and preserve the Misskey-compatible shape only at the wire
boundary.

## Observed endpoint contracts

### `POST /api/meta`

Aria first sends an anonymous empty object:

```json
{}
```

The login path reads only these fields. The local service returns its
configured `LOCAL_ORIGIN` (or omits `uri`); it must never return
`IDENTITY_ORIGIN` or an arbitrary upstream authority for Aria to save.

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
the value is a client return destination, not the server's upstream callback:
the local service validates and stores it, completes owner verification at its
fixed internal HTTPS callback, and only then redirects to the exact stored
Aria callback with the original route session. The exact permission values,
in source order, are:

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
It must not be forwarded unchanged to the upstream identity provider; any
upstream owner-verification/provider scopes come from a separate minimal
server-side allowlist.

The response is an interactive HTML/browser flow and is not parsed by Aria.
The exact page status, cookie attributes, consent behavior, redirect timing,
and whether the upstream implementation accepts every requested permission
are **要実機確認**. The local server must use its fixed internal callback
under `LOCAL_ORIGIN` and its exact client-return callback allowlist; it must
reject any client attempt to select an upstream origin or an unconfigured
callback.

The `{session}` route value is the same opaque Aria route session ID in the
`GET` URL and the `/api/miauth/{session}/check` path. It is a high-entropy
bearer capability/correlation secret for accessing the state of that one local
auth attempt, so it must not be logged or exposed in diagnostics. Possession
permits polling/checking that attempt only; it is not proof of owner identity,
owner binding, or authorization to mint a local API token. Those decisions
require the server-side state and owner verification described by ADR-0001.

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
returned token to this local session and the configured origins; the response
must never contain the upstream token.

The `token` in this response is a secret. It is shown in this example only as
the literal word `REDACTED_…`; real fixtures and logs must never contain it.
The check endpoint's exact pending body, status code, replay response, and
whether a consumed session remains readable are **要実機確認**; the local
service decision is one-time atomic consume as specified by ADR-0001.

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
   an upstream token is not a supported login path.
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
the target instance. The local wire projection must not expose upstream
tokens or administrative secrets.

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

## Requirement traceability

The table maps Issue #1 requirements to roadmap children. Issue #2 freezes the
contracts; the later issue owns implementation and evidence.

| Issue #1 requirement | MVP owner issues | Future owner / note |
| --- | --- | --- |
| Only the allowed Misskey user can add an account through MiAuth | #5, #7, #13 | #2 ADR and contract are prerequisites |
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
