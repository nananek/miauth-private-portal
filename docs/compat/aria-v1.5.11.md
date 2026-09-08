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
6. (Issue #23 PR1) Edit the owner's display name from Settings → Profile.
7. (Issue #23 PR2) Open the server-info page for this instance, which
   loads server-wide stats.

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
| `POST /api/i/update` (`name` field only) | **必要** for Issue #23 | Owner profile display-name self-edit from Settings → Profile | `i` token; local `write:account` equivalent |
| `POST /api/i/update` (`username` field) | **不要** | No traced Aria/misskey_dart source ever sends or exposes a `username` field on this endpoint | N/A — never implement without a new observed source |
| `POST /api/notes/update` | **不要** for Issue #2 | Only the edit path uses it; editing is not an Issue #2 acceptance journey | Do not advertise it until a later issue adds a contract |
| WebSocket `/streaming` timeline channel | **不要** for MVP compatibility scope (HTTP load/reload/pagination remain the correctness source of truth); **minimal stub since Issue #41, real `homeTimeline` note-create push since Issue #95** | Provides live insertion as a UX enhancement, not a requirement — see "Streaming decision" below | A failed optional stream must not make HTTP timeline or post operations fail |
| `POST /api/stats` | **必要** for Issue #23 PR2 (implemented) | Server-info page, always reachable via `/{acct}/servers/{host}` | Anonymous; the traced call site always builds a tokenless guest account, so no `i` field is ever sent |
| `POST /api/notes/delete` | **必要** for Issue #23 PR3 (implemented) | Note footer/sheet delete action, and the post-edit dialog's delete option | `i` token; `write:notes` (already granted — no new scope) |
| `POST /api/notes/renote` | **不要** | No traced Aria/misskey_dart source ever sends a dedicated renote-creation request to this path; renoting is `notes/create` with `renoteId` set (already rejected as `UNSUPPORTED_FEATURE`) | N/A — never implement without a new observed source |
| `POST /api/notes/reactions/create` | **必要** for Issue #23 PR4 (implemented) | Note footer's reaction button/picker | `i` token; `write:reactions` scope (newly granted by this PR) |
| `POST /api/notes/reactions/delete` | **必要** for Issue #23 PR4 (implemented) | Note footer's un-react / change-reaction actions | `i` token; `write:reactions` scope |
| `POST /api/notes/reactions` | **必要** for Issue #23 PR4 (implemented) | "Who reacted" sheet's paginated reaction list | `i` token; `read:reactions` scope. **Not** `/api/notes/reactions/list` — see this document's "POST /api/notes/reactions" section for why plan-issue-23's assumed path is corrected here |
| `POST /api/notes/mentions` | **必要** for Issue #23 (not yet implemented — PR5) | Optional user-added "Mention"/"Direct" home-timeline tabs | `i` token; `read:notes` (already granted — no new scope, per trace; see below) |
| `POST /api/i/notifications` | **必要** for Issue #23 PR6 (implemented) | The Notifications tab, always present in Aria's navigation | `i` token; new `read:notifications` scope |
| `POST /api/notifications/mark-all-as-read` | **不要** | No traced Aria/misskey_dart source ever calls this; the pinned `misskey_dart` client does not even define a wrapper method for it (see below) | N/A — never implement without a new observed source |
| `POST /api/users/search` | **必要** for Issue #65 (implemented) | User-selection dialog and mention/search autocomplete's query-based lookup | `i` token; `read:account` (already granted — no new scope) |
| `POST /api/users/search-by-username-and-host` | **必要** for Issue #65 (implemented) | Same call sites' exact username(+host) lookup | `i` token; `read:account` (already granted — no new scope) |
| `POST /api/drive` | **必要** for Issue #77 (implemented — PR3) | Drive capacity/usage display (drive screen header, account settings) | `i` token; new `read:drive` scope |
| `POST /api/drive/files` | **必要** for Issue #77 (implemented — PR3) | Drive screen's per-folder file listing, paginated with `untilId`/`limit` | `i` token; `read:drive` |
| `POST /api/drive/files/create` | **必要** for Issue #77 (implemented — PR3) | Upload from local file (post composer, profile avatar picker, drive screen); both the multipart-file and raw-binary request forms are used | `i` token; new `write:drive` scope |
| `POST /api/drive/files/show` | **必要** for Issue #77 (implemented — PR3) | Opening one drive file's detail page | `i` token; `read:drive` |
| `POST /api/drive/files/update` | **必要** for Issue #77 (implemented — PR3) | Rename, toggle sensitive, edit comment, and move-to-folder — all four via the same endpoint as a partial-field POST | `i` token; `write:drive` |
| `POST /api/drive/files/delete` | **必要** for Issue #77 (implemented — PR3) | Drive file delete action | `i` token; `write:drive` |
| `POST /api/drive/files/upload-from-url` | **必要** for Issue #77 (implemented — PR3) | Drive screen's "upload from URL" action | `i` token; `write:drive` |
| `POST /api/drive/files/attached-notes` | **必要** for Issue #77 (implemented — PR3 shipped the route returning `[]`; PR6 wires it to real `entry_files` data) | Drive file detail page's "notes this file is attached to" list | `i` token; `read:drive` + `read:notes` |
| `POST /api/drive/files/move-bulk` | **要実機確認**; optional | Drive multi-select bulk-move action — Aria probes `POST /api/endpoints` first and transparently falls back to per-file `drive/files/update` moves when the name is absent | Omitting this name from this service's `/api/endpoints` response is sufficient to make Aria always use the per-file fallback instead of implementing this endpoint |
| `POST /api/drive/folders`, `/create`, `/delete`, `/update`, `/show` | **必要** for Issue #77 (implemented — PR3) | Drive screen's folder browsing, creation, rename, and move UI — used extensively, not an edge feature | `i` token; `read:drive`/`write:drive` |
| `POST /api/drive/stream` | **不要** | No traced Aria source ever calls this (`MisskeyDrive.stream` has no Aria call site) | N/A — never implement without a new observed source |
| `POST /api/drive/files/find` | **不要** | No traced Aria source ever calls this | N/A — never implement without a new observed source |
| `POST /api/drive/files/check-existence` | **不要** | No traced Aria source ever calls this | N/A — never implement without a new observed source |
| `POST /api/drive/files/find-by-hash` | **不要** | No traced Aria source ever calls this | N/A — never implement without a new observed source |
| `POST /api/drive/folders/find` | **不要** | No traced Aria source ever calls this | N/A — never implement without a new observed source |
| `POST /api/notes/create` (`fileIds` field) | **必要** for Issue #77 (implemented — PR6) | Post composer's attachment picker (new local upload or existing drive file) | Already-granted `write:notes` — no new scope |
| `POST /api/i/update` (`avatarId` field) | **必要** for Issue #77 (implemented — PR5) | Profile avatar upload/removal flow | `write:account` (already granted). Every other non-`name`/`avatarId` `IUpdateRequest` field stays rejected as `UNSUPPORTED_FEATURE`, per the Issue #23 scope decision above |

`/api/endpoints` is deliberately **要実機確認** rather than part of the
minimal release gate: the call is present in Aria's edit capability probe,
but create/reply/reload/thread journeys do not depend on it. The local server
must not claim edit support until the later endpoint decision is made.

For this contract, the exact effective local API scope set is
`read:account`, `read:notes`, `write:notes`, (since Issue #23 PR1)
`write:account`, (since Issue #23 PR4) `read:reactions`/
`write:reactions`, (since Issue #23 PR6) `read:notifications`, and
(since Issue #77 PR3) `read:drive`/`write:drive`. Aria's MiAuth
`permission` query already included `read:drive,write:drive` before PR3
shipped (it requests the union of every feature it supports, independent
of what any given instance implements), so an **existing** local API
token issued before PR3 added these two names to `grantableScopes` does
not carry them — the same re-authorization-required situation this
document already records for `read:notifications`/PR6 and for
plan-issue-23's other newly-granted scopes; re-approving through
`miauthctl` is what adds them retroactively. The broad `permission` query from Aria is recorded for
compatibility but does not grant any additional scope. `meta`, `endpoints`,
the MiAuth page, and the MiAuth check use their documented browser or
anonymous/session capability and do not consume a local API token. `/api/i`,
`/api/i/update`, and every notes endpoint in the allowlist require a locally
issued API token; the token-login fallback accepts only such a local token.
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

### `POST /api/i/update` (Issue #23 profile self-edit)

Traced from [`lib/view/page/settings/profile_page.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/view/page/settings/profile_page.dart) and
[`lib/provider/api/i_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/i_notifier_provider.dart),
plus the pinned `misskey_dart` request model
[`lib/src/data/i/i_update_request.dart`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/data/i/i_update_request.dart).

Every `INotifier` setter (`setName`, `setDescription`, `setBirthday`, ...) sends
only the single field it changed, with every other field omitted, and decodes
the response as `MeDetailed`:

```json
{
  "name": "New Display Name",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

```json
{ "...": "MeDetailed" }
```

`profile_page.dart`'s "Name" field is what real Misskey/Aria call the display
name; it maps to Aria's `setName`, which calls
`_misskey.i.update(IUpdateRequest(name: value))` (or the raw
`apiService.post('i/update', {'name': value})` form other setters use). This is
the only field this issue's PR1 scope implements
(`docs/decisions`/Issue #23 Non-goals: avatar, description, and the rest of
`IUpdateRequest`'s 40+ fields are out of scope and must be rejected the same
way `/api/notes/create` rejects unsupported fields — `UNSUPPORTED_FEATURE`, not
silently ignored). **Issue #77 PR5 narrows this**: `avatarId` moves from
rejected to implemented (see this document's "Drive API and note
attachments" section below for the traced `files/create` → `i/update
{avatarId: ...}` sequence); every other non-`name` field stays rejected.

**`username` is not part of this contract.** The pinned `IUpdateRequest` model
has no `username` field at all — Misskey's real `/api/i/update` does not
support self-service handle renaming, and neither `misskey_dart` nor Aria
model it. Confirmed independently in the client UI: `profile_page.dart` has no
username input (username is not one of its editable fields), and
[`account_settings_page.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/view/page/settings/account_settings_page.dart)
renders the username through a read-only `UsernameWidget`. Aria therefore never
sends, and has no code path that could ever send, a `username` field to this
or any endpoint. Per the same standard applied to `/api/notes/update` above,
this service must not accept-and-apply a `username` field it has never
observed any real client send — doing so would be inventing a protocol rather
than implementing an observed one. **This directly narrows Issue #23's
`/api/i/update` acceptance criteria, which asks for username **and**
display-name self-edit; see the Issue #23 implementation notes below for the
resulting scope decision.**

The exact `write:account`-scope enforcement status code, and whether a real
instance's `MeDetailed` response after `i/update` differs from `/api/i`'s, are
**要実機確認**.

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

### `POST /api/stats` (Issue #23 PR2)

Traced from [`lib/src/misskey_dart_base.dart`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/misskey_dart_base.dart)'s
`stats()` method and Aria's only call site,
[`lib/provider/api/stats_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/stats_provider.dart),
invoked from
[`lib/view/page/server/server_overview.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/view/page/server/server_overview.dart)
(mounted at the `/{acct}/servers/{host}` route,
[`lib/router/router.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/router/router.dart)).

```json
{}
```

This call is anonymous by construction, not merely by observed habit:
`server_overview.dart` builds `Account(host: host)` — a guest account
(`username: null`) — regardless of whether the viewer is actually logged
into that host, `token_provider.dart` resolves a guest account's token
as always `null`, and the pinned `ApiService.post` (`lib/src/services/
api_service.dart`) unconditionally adds `i: token` to every request body
and then strips any null-valued field before sending. So Aria never
attaches an API token to this call, matching real Misskey's own
unauthenticated `/api/stats`. This handler must not require a scope or
reject a request with no `i` field.

The pinned `StatsResponse` parser treats every field as optional:

| Field | Type | This service's value |
| --- | --- | --- |
| `notesCount` | int | Total entries ever stored, every author, including archived/hidden (`EntryRepository.CountAll`) |
| `originalNotesCount` | int | Same as `notesCount` — no federation, so every note is "original" |
| `usersCount` | int | Always `1` — the single owner is this deployment's only registered user |
| `originalUsersCount` | int | Always `1`, for the same reason |
| `reactionsCount` | int | Total reactions ever stored, across every entry (`ReactionRepository.CountAll`, added by Issue #23 PR4; a fixed `0` before that PR) |
| `instances` | int | Always `0` — no federation |
| `driveUsageLocal` / `driveUsageRemote` | int | Still always `0`, deliberately, even after Issue #77 PR3 added Drive: `POST /api/stats` is anonymous (no local API token — see this handler's own doc comment), and Drive usage is a per-owner figure now available through the authenticated `POST /api/drive` (`read:drive`) instead — summing `files.byte_size` into this anonymous, tokenless response would leak the owner's private storage usage to any caller. "remote" stays `0` regardless, same as before: no federation |

### `POST /api/notes/delete` (Issue #23 PR3, implemented)

Traced from
[`lib/provider/notes_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/notes_notifier_provider.dart)'s
`delete(noteId)`, reachable from the note footer, the note action sheet,
and the post-edit dialog's delete option, plus the pinned `misskey_dart`
request model
[`lib/src/data/notes/notes_delete_request.dart`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/data/notes/notes_delete_request.dart).

```json
{
  "noteId": "note-id",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

`noteId` is a required opaque string; there is no other field. The pinned
client sends this through `_apiService.post<void>`, so any 2xx JSON body
(including `{}`) is accepted — Aria never decodes a typed response here
and instead removes the note from its local cache client-side after the
call succeeds. This reuses the already-granted `write:notes` scope; no
new scope is needed. `plan-issue-23`'s confirmed direction (mapping
delete onto `timeline.Service.SetHidden`, restricted to the owner's own
`user_post` entries) is unaffected by this trace and remains PR3's basis.

**Implemented as** `Server.handleNotesDelete`
(`internal/httpserver/noteapi_handlers.go`), registered with the existing
`write:notes` scope. It resolves `noteId` through `timeline.GetEntry`,
then collapses every non-deletable case onto the uniform `NO_SUCH_NOTE`
response `writeNoSuchNote` already gives every other note-reading
endpoint for an unknown/hidden/archived ID: an unknown note, an
already-hidden/archived note (`entryVisible`), and a note that exists but
is not the caller's own `user_post` (`entry.Kind !=
domain.EntryUserPost || entry.AuthorActorID !=
LocalActorIDFromContext(ctx)`) are all indistinguishable to the caller. A
permitted delete calls `timeline.Service.SetHidden(id, true)` and
responds `200 {}`. See `docs/decisions/0004-note-delete-as-hide.md` for
why hide (not a new hard delete) was chosen, and why `hidden` rather than
`archived` is the primitive this maps onto. `EntryRepository.CountByAuthor`
(hence `/api/i`'s and `/api/miauth/{session}/check`'s `notesCount`) now
excludes archived/hidden entries, so a successful delete visibly
decrements it; `EntryRepository.CountAll` (PR2's `/api/stats`) is
unaffected and keeps counting every entry.

### `POST /api/notes/renote` (不要 — confirmed no dedicated wire path)

Exhaustively searched: the pinned `misskey_dart`'s `MisskeyNotes` class
([`lib/src/misskey_note.dart`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/misskey_note.dart))
has no `renote()` method and no `NotesRenoteCreateRequest`-shaped type
exists anywhere in the pinned dependency. Every renote-creation call site
in the pinned Aria commit (`lib/view/widget/renote_sheet.dart`,
`lib/provider/post_notifier_provider.dart`) constructs a
`NotesCreateRequest(renoteId: note.id)` and posts it through the already-
implemented `notes/create` path — which already rejects a non-null
`renoteId` with `UNSUPPORTED_FEATURE` (see this document's `POST
/api/notes/create` section). Per Issue #23's own Non-goals ("`/api/notes/
renote` は、ソーストレースの結果 Aria が実際に呼ばないと判明した場合、
この issue のスコープから除外する"), this item is dropped from scope
entirely, the same way `/api/notes/update` was: no `/api/notes/renote`
route will ever be added.

Two related-but-distinct endpoints do exist and are genuinely called by
Aria — `notes/renotes` (list who renoted a note,
`lib/provider/api/renotes_notifier_provider.dart`) and `notes/unrenote`
(undo a renote, `lib/view/widget/note_footer.dart:312`) — but neither was
part of Issue #23's explicit scope list, so this trace records their
existence without adding them to scope. Both remain **不要** for this
issue; a future issue would need to add them explicitly, including the
question of what "renote" even means in a single-owner, no-federation
deployment (Issue #23's own text floats a "resurface my own past post"
use case as the only plausible one).

### `POST /api/notes/reactions/create`, `/delete`, and `POST /api/notes/reactions` (Issue #23 PR4, implemented)

Traced from
[`lib/provider/notes_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/notes_notifier_provider.dart)'s
`react`/`unreact`/`changeReaction` (reachable from the note footer's
reaction button and long-press picker, with no client-side restriction
on reacting to your own note or an assistant/system-authored note — see
[`lib/view/widget/note_footer.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/view/widget/note_footer.dart)),
[`lib/provider/api/reactions_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/reactions_notifier_provider.dart)
(the paginated "who reacted" sheet), and the pinned `misskey_dart`
[`MisskeyNotesReactions`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/misskey_note.dart)
class.

**Correction to `plan-issue-23`**: the list endpoint's wire path is
`notes/reactions`, not `notes/reactions/list` as the plan assumed. This
is a path correction only — the endpoint's purpose, request shape, and
PR4 scope are otherwise unaffected.

Create:

```json
{
  "noteId": "note-id",
  "reaction": "👍",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

Delete (no `reaction` field — a Misskey account has at most one reaction
per note, so the note ID alone identifies which one to remove; `notes/
reactions/delete` is also how `changeReaction` clears the old reaction
before creating the new one):

```json
{
  "noteId": "note-id",
  "i": "REDACTED_LOCAL_TOKEN"
}
```

Both are sent through `_apiService.post<void>`, so any 2xx body is
accepted; Aria updates its note cache optimistically and then re-fetches
via `notes/show`.

List (`notes/reactions`):

```json
{
  "noteId": "note-id",
  "type": "👍",
  "limit": 20,
  "i": "REDACTED_LOCAL_TOKEN"
}
```

`type`, `limit`, `offset`, `sinceId`, `untilId`, `sinceDate`, and
`untilDate` are all optional in the generated model; the traced call
site always sends `noteId`, `type` (the specific reaction being
paginated — Aria's "who reacted" sheet is per-reaction-emoji, not a
single combined list), and `limit: 20`, paginating with `untilId`. The
response is a JSON array of:

| Field | Type | Null / omission behavior |
| --- | --- | --- |
| `id` | string | Required and opaque |
| `createdAt` | ISO-8601 string | Required |
| `user` | object | Required; decoded as `UserLite` |
| `type` | string | Nullable |

Reaction emoji scope: Aria's picker can produce either a plain Unicode
emoji or a `:name:`/`:name@host:` custom-emoji shortcode. Per
`plan-issue-23`'s already-confirmed direction (and this issue's Non-
goals excluding custom emoji/drive), PR4 accepts only a plain Unicode
emoji in `reaction` and rejects a `:`-delimited shortcode with
`UNSUPPORTED_FEATURE` (`isCustomEmojiShortcode`,
`internal/httpserver/reactions_handlers.go`), the same way
`/api/notes/create` rejects fields it does not support.

Scopes: Aria's fixed permission list already includes
`read:reactions`/`write:reactions` (see this document's `GET /miauth/
{session}` section); PR4 adds both to
`internal/miauth/scope.go`'s `grantableScopes` — but every API token
issued before this PR shipped does not carry them (see Issue #23 §3
"既存 API token への新規 scope 反映", still open; re-approving through
`miauthctl` is the operational workaround).

**Implementation (2026-09-06)**: `domain.Reaction`/`ReactionRepository`
(`internal/domain/reaction.go`) and migration `0013_reactions.sql` back a
new `reactions` table (`entry_id`, `reactor_actor_id`, `emoji`,
`created_at`, `UNIQUE(entry_id, reactor_actor_id)`).
`ReactionRepository.Create` is an upsert — a second call for the same
(entry, actor) pair overwrites the emoji rather than conflicting — so
either Aria's own delete-then-create `changeReaction` sequence or a
direct repeat `create` call lands on the same single-row-per-pair state;
`Delete` is idempotent for the same reason removing an absent reaction is
not observed to be an error. `timeline.Service` exposes
`SetReaction`/`RemoveReaction`/`ReactionCounts`/`MyReaction`/
`ListReactions`/`GetReaction`/`CountAllReactions` as thin wrappers, mirroring
`SetHidden`/`SetArchived`'s existing style rather than needing a
`UnitOfWork` transaction (each is a single-table write). The target note
is not restricted to the owner's own `user_post` entries (unlike
`/api/notes/delete`): only `entryVisible` gates it, matching the
recommended policy above and Aria's own lack of an `isMe`-style guard.
`note.Reactions`/`note.myReaction` (`internal/httpserver/noteapi_wire.go`)
are now populated with real data on every note-returning endpoint (not
only the three reactions endpoints themselves) via a new
`(*Server).projectNote` wrapper around `newNote`, since Aria's note
footer needs this on every rendered note, not just the ones a "who
reacted" call happens to touch. `POST /api/stats`' `reactionsCount` is
likewise no longer a fixed `0` (see this document's `POST /api/stats`
section).

### `POST /api/notes/mentions` (Issue #23 PR5, implemented)

**Finding that revises `plan-issue-23`'s premise**: the plan and Issue
#23 body both treat this as a "縁辺のユースケース" that might warrant
only a minimal empty-array implementation. The trace shows it is a real,
reachable Aria call, not dead code — Aria's home-timeline tab system
calls it whenever the owner adds a "Mention" or "Direct" tab:

```dart
// lib/provider/api/timeline_notes_notifier_provider.dart
TabType.mention => _misskey.notes.mentions(
  NotesMentionsRequest(untilId: untilId, sinceDate: sinceDate, untilDate: untilDate, limit: limit),
),
TabType.direct => _misskey.notes.mentions(
  NotesMentionsRequest(untilId: untilId, sinceDate: sinceDate, untilDate: untilDate, limit: limit,
    visibility: NoteVisibility.specified),
),
```

(also duplicated in
[`lib/provider/api/timeline_notes_after_note_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/timeline_notes_after_note_notifier_provider.dart)
for the "load around a note" path). Both tab types are optional — the
owner must explicitly add one to their timeline configuration — so this
is reachable-but-not-default, the same tier as `/api/endpoints`'s edit
probe, not one of this document's core always-exercised journeys.

The pinned `misskey_dart` request model
([`lib/src/data/notes/notes_mentions_request.dart`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/data/notes/notes_mentions_request.dart))
also declares `following` and `sinceId`, but Aria's call sites never set
them. The `direct` tab adds `visibility: NoteVisibility.specified` —
since this service has no visibility concept beyond "public" (every note
this service ever creates is `"public"`, see the Minimum Note contract),
that tab variant will always resolve to an empty result here; only the
plain `mention` tab variant can ever return anything. The response is a
JSON array of `Note` — the existing wire `note` type, no new shape.

This does not change PR5's scope decision by itself (recorded as
"要検討" between real `@username` extraction and a minimal empty
response in `plan-issue-23`), but it did mean option (B) (minimal empty
array) would have been a deliberate simplification of a real,
owner-reachable feature, not a no-op for dead code. Weighed on exactly
that basis, option (A) (real `@username` detection) was adopted
(owner-confirmed, 2026-09-06). No new scope is needed either way:
`read:notes` already covers it.

**Implemented as** `Server.handleNotesMentions`
(`internal/httpserver/mentions_handlers.go`), registered with the
existing `read:notes` scope. `visibility: "specified"` (the "Direct" tab)
short-circuits to an empty page without running any query, matching the
trace above; otherwise pagination mirrors `handleNotesTimeline`
(`untilId` resolved through `timeline.GetEntry`, `limit`
default/clamp, newest-first). Detection itself lives in
`timeline.Service.recordSelfMentionIfAny`
(`internal/timeline/service.go`): a `user_post`'s `Body` is matched
against `(^|[^A-Za-z0-9_])@` + the configured `OWNER_USERNAME` +
`([^A-Za-z0-9_]|$)` at creation time, word-bounded so a longer
`@`-handle merely starting with the owner's username (or an
email-like `name@owner.example` token) never false-positives. Only
`user_post` is ever scanned — `llm_reply`/`llm_follow_up`/`news`/`mail`
entries are not, since Issue #23's own text already treats an LLM reply
deliberately mentioning the owner as overlapping with the existing
`EntryLLMFollowUp` concept, and this deployment's single login-capable
actor means the mentioned actor is always the post's own author, so no
lookup beyond the entry itself is needed. Detection is forward-only: it
runs once, at creation time, for every entry-creation path
(`CreateRoot`/`CreateReply`/`CreateGeneratedReply`/
`CreateExternalEntry`, the latter two always no-op on the `Kind` check),
and pre-existing entries from before this feature shipped are never
scanned or backfilled (owner-confirmed, 2026-09-06). The persisted
`mentions` table (migration `0014_mentions.sql`) is a thin
`(entry_id, mentioned_actor_id, created_at)` index joined against
`entries` for the actual read (`MentionRepository
.ListEntriesByMentionedActor`), so no new `Note` field beyond the
already-static `mentions` array's own JSON shape is needed.

### `POST /api/i/notifications` (Issue #23 PR6, implemented)

Traced from
[`lib/provider/api/notifications_notifier_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/api/notifications_notifier_provider.dart)
(backing the Notifications tab, a permanent part of Aria's navigation —
unlike `notes/mentions`'s optional tabs, this one is always reachable)
and
[`lib/view/widget/notification_widget.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/view/widget/notification_widget.dart)'s
per-`type` rendering, plus the pinned `misskey_dart`
[`INotificationsRequest`/`INotificationsResponse`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/data/i/)
models.

Aria first probes `POST /api/endpoints` for `"i/notifications-grouped"`
to decide whether to call the grouped variant instead; since this
service's `implementedEndpoints` does not (and will not) advertise it,
Aria's grouped-notifications setting has no effect here and it always
falls back to the plain call:

```json
{
  "untilId": "older-notification-id",
  "limit": 20,
  "i": "REDACTED_LOCAL_TOKEN"
}
```

`sinceId`, `sinceDate`, `untilDate`, `following`, `unreadOnly`,
`markAsRead`, `includeTypes`, and `excludeTypes` all exist in the
generated request model, but this call site never sets any of them.

**Type-decode risk from `plan-issue-23` §3.5 is resolved**: the response
model's `type` field is annotated
`@JsonKey(unknownEnumValue: JsonKey.nullForUndefinedEnumValue)` — an
unrecognized `type` string decodes to `null` rather than throwing. This
means a locally-chosen `type` value the pinned `NotificationType` enum
does not declare would not break decoding; it would just render as an
untyped/unhandled notification. In practice this is moot: `NotificationType`
already declares `reply` and `app` (`lib/src/enums/notification_type.dart`),
so `plan-issue-23`'s recommended mapping (llm_reply/llm_follow_up →
`"reply"`, news/mail → `"app"`) uses only pre-existing enum values.
`notification_widget.dart` confirms the exact fields each rendering path
needs:

- `reply` (and `quote`) requires a non-null `note` and renders the full
  note via `NoteWidget(noteId: note.id)` — matching `plan-issue-23`'s
  plan to reuse `newNote()` for this type.
- `app` uses only optional `icon`/`header`/`body` (all nullable — a
  header-only or even fully-empty `app` notification still renders
  without error), confirming the "free-text third-party notification"
  shape `plan-issue-23` guessed for news/mail is correct and low-risk.

**Implemented as** `Server.handleAPINotifications`
(`internal/httpserver/notifications_handlers.go`), registered with the
new `read:notifications` scope. Pagination mirrors `handleNotesReactions`:
`limit` defaults to 20 and clamps to `maxTimelineLimit` (100), and
`untilId` resolves through the notification's own `(created_at, id)` via
`timeline.Service.GetNotification` (an unknown/stale `untilId` yields an
empty page, the same pagination-loop-safe treatment every other
`untilId`-paginated endpoint in this document gives it) rather than
through the related note's — a notification and its related entry are
different rows with independent, only-coincidentally-correlated
timestamps once more than one notification type exists.

Delivery itself lives in `timeline.Service`
(`internal/timeline/service.go`): `CreateGeneratedReply` records a
`"reply"` notification for the llm_reply/llm_follow_up entry it just
created, and `CreateExternalEntry` records an `"app"` notification for a
news/mail entry, but only inside the `created == true` branch — a
redelivered duplicate (same `DedupeKey`) never notifies twice. Both calls
run in the same transaction as the entry create, via the new
`domain.NotificationRepository` (`notifications` table, migration
`0015_notifications.sql`), so a notification can never be missing for, or
outlive, the entry it names. A plain `user_post` creation (`CreateRoot`/
`CreateReply`) never notifies.

Only the fields each type actually needs are sent — `id`/`createdAt`/
`type` always, `note` for `"reply"` (the full wire `Note` for the
generated reply/follow-up entry itself, reusing `newNote()`/
`(*Server).projectNote` exactly as `plan-issue-23` planned), `body` for
`"app"` (the related news/mail entry's `Body` verbatim, which already
carries `internal/ingest.Service.composeExternalBody`'s own
kind/title/provenance-URL header — see this document's "Note.text
provenance markers" section — so no separate `header` value is
synthesized). Every other `INotificationsResponse` field
(`reaction`, `achievement`, `role`, `users`, ...) belongs to notification
types this deployment never emits and is omitted rather than sent as
always-null padding, matching how real Misskey's own notification wire
format already varies fields by type. There is no read/unread field on
the wire response and no persisted read state either: PR2's trace found
no Aria/misskey_dart wire path for `mark-all-as-read` or any other
read-management call (see below), so this PR implements listing only —
`plan-issue-23`'s original `domain.Notification` sketch's `ReadAt` field
and `NotificationRepository.MarkAllRead` method were dropped accordingly
rather than added as unreachable code.

### `POST /api/notifications/mark-all-as-read` (不要 — confirmed no wire path exists at all)

**Finding that revises `plan-issue-23`'s and Issue #23's premise**: this
sub-endpoint is not merely unused by Aria — it does not exist anywhere
in the pinned `misskey_dart` dependency. `lib/src/misskey_i.dart` (the
class wrapping every `i/*`-family call, including `i/notifications`)
declares no method for it, and an exhaustive search of both the pinned
Aria commit and the pinned `misskey_dart` commit for `mark-all-as-read`/
`markAllAsRead` returns zero results. `INotificationsRequest` does
declare a `markAsRead: bool?` field — real Misskey's actual alternate
mechanism for this, folded into `i/notifications` itself rather than a
separate endpoint — but Aria's own call site never sets it either.

Per Issue #23's own common acceptance criteria ("トレースの結果 Aria が
実際には呼ばない...と判明した項目は...「不要」として明示的に記録し、
実装しない"), this is exactly the documented fallback, not a scope
conflict requiring a new owner decision the way PR1's `username` finding
was — but it is a large enough surprise relative to Issue #23's explicit
Scope/Acceptance-criteria text (which lists this sub-endpoint by name)
that PR6 should not assume it needs to add this route at all.

### `POST /api/users/search` and `POST /api/users/search-by-username-and-host` (Issue #65, implemented)

Traced from the pinned Aria commit's `lib/view/dialog/user_select_dialog.dart`,
`lib/provider/api/search_users_notifier_provider.dart`, and
`lib/provider/api/search_users_by_username_provider.dart`, plus the pinned
`misskey_dart`'s `UsersSearchRequest`/`UsersSearchByUsernameAndHostRequest`
models and the polymorphic `User.fromJson` decoder in
`lib/src/data/base/user.dart`. The observed operator action that surfaces
these calls is Aria's own "Chat" feature's user picker (`Start Chat` →
`Individual Chat`); **this PR implements only the search lookup itself, not
Chat** — `POST /api/chat/*` remains unimplemented, matching README.md's
"Known limitations." Selecting a user from the resulting dialog still ends
in an unsupported-endpoint error the first time Aria tries to actually start
or send a chat message.

Both endpoints omit any null-valued optional field before sending, per this
document's general rule:

```json
// POST /api/users/search
{"query": "some text", "offset": 0, "limit": 10, "origin": "combined", "detail": true}
// POST /api/users/search-by-username-and-host
{"username": "someuser", "host": null, "limit": 10, "detail": true}
```

`query` is the only field `UsersSearchRequest` requires; every other field on
both requests — `offset`/`limit`/`origin`/`detail` on the first,
`username`/`host`/`limit`/`detail` on the second — may be entirely absent.

**The response is a bare JSON array, never wrapped in an object**, and each
element is decoded through `User.fromJson`'s discriminator, not directly as
`UserDetailedNotMe`:

```dart
factory User.fromJson(json) {
  if (json.containsKey("url")) return UserDetailed.fromJson(json);
  else return UserLite.fromJson(json);
}
factory UserDetailed.fromJson(json) {
  if (json.containsKey("avatarId")) return MeDetailed.fromJson(json);
  else if (json.containsKey("isFollowing")) return UserDetailedNotMeWithRelations.fromJson(json);
  else return UserDetailedNotMe.fromJson(json);
}
```

All three Aria call sites filter their result list with
`.whereType<UserDetailed>()`. **A search result object missing the `"url"`
key (any value, including `null`, is enough) decodes as `UserLite` instead
and is silently dropped from what Aria ever shows** — the endpoint still
returns 200 with otherwise-correct-looking JSON, so this failure mode is
easy to miss without a dedicated raw-JSON key check. Conversely, an
`"avatarId"` or `"isFollowing"` key must never be present, or the object
misdecodes as `MeDetailed`/`UserDetailedNotMeWithRelations` and fails to
parse against their own additional required fields. Every other required
field matches this document's already-frozen "`UserDetailedNotMe` minimum"
table above.

**Implemented as** `Server.handleUsersSearch`/
`Server.handleUsersSearchByUsernameAndHost`
(`internal/httpserver/users_handlers.go`/`users_wire.go`), registered with
the existing `read:account` scope (no new scope; `users/search` is not
gated behind any distinct permission in Aria's fixed MiAuth permission list
either). Both search over this deployment's known local-actor set — the
owner, the reserved assistant/system presentation actors, and (Issue #52)
every currently resolvable Open WebUI model actor — rather than a general
directory: `users/search` does a case-insensitive substring match on
username or display name plus an optional `origin` (`local`/`remote`/
`combined`) scope; `users/search-by-username-and-host` does an exact,
case-insensitive username match plus a host match (an omitted or empty host
means local-only, matching this document's "host: null means local"
convention). `userDetailedNotMe` gained two fields for this: `Host *string`
(nil for every local actor, the Open WebUI model's fixed presentation host
otherwise — mirroring `userLite`'s own convention) and `Url *string`
(always nil; existing solely to keep the `"url"` discriminator key present
on every projection built through this struct, including `/api/i` and the
MiAuth check response, which were already unaffected since Aria decodes
their `user` field directly as `UserDetailedNotMe`, never through the
polymorphic `User`/`UserDetailed` discriminator).

### Drive API and note attachments (Issue #77 investigation — PR0)

**This section originally documented a contract for functionality this
service did not implement yet; Drive (PR3), profile avatars (PR5), and
post attachments (PR6) are all now implemented.** This PR0 records
what the pinned Aria snapshot and its pinned `misskey_dart` dependency
actually do, so those later PRs implement an observed protocol rather
than a remembered one
(AGENTS.md: "Do not silently invent a protocol"), matching the method
Issue #72 used for Open WebUI tool-call feasibility. Traced from the
pinned Aria commit
[`a66c9303`](https://github.com/poppingmoon/aria/tree/a66c9303995e7c964765cf382de6a9b0e3f4a3b6)'s
`lib/provider/api/drive_files_notifier_provider.dart`,
`drive_file_notifier_provider.dart`, `drive_folders_notifier_provider.dart`,
`drive_folder_provider.dart`, `drive_stats_provider.dart`,
`attached_notes_notifier_provider.dart`, `attaches_notifier_provider.dart`,
`endpoints_notifier_provider.dart`, `post_notifier_provider.dart`,
`lib/extension/notes_create_request_extension.dart` and
`note_draft_extension.dart`, `lib/model/post_file.dart`, and
`lib/view/page/settings/profile_page.dart`/`i_notifier_provider.dart`, plus
the pinned `misskey_dart` commit
[`14176c515`](https://github.com/poppingmoon/misskey_dart/tree/14176c515a005a9fb01d3e6365a49b5a5d387a92)'s
`lib/src/misskey_drive.dart` (the full Drive API surface the client
exposes) and `lib/src/data/base/drive_file.dart`/`note.dart`.

**Every Drive/folder endpoint the allowlist table above marks 必要 is
genuinely called by Aria's drive screen** (`drive_page.dart`,
`drive_file_page.dart`, and the widgets under `lib/view/widget/drive_*.dart`
and `file_picker_sheet.dart`); this is not a generic client capability Aria
merely links in. In particular, **the folder API is not a corner case**:
`DriveFoldersNotifier`/`driveFolderProvider` implement full folder listing,
creation, deletion, rename, and move, and the drive screen surfaces all of
them. A Drive implementation that hard-codes every file to a single root
folder (`folderId: null`) and treats `drive/folders/*` as unsupported will
make those UI actions fail outright rather than degrade gracefully —
narrower than plan-issue-77 v2 §2.1's "minimal or unsupported" framing for
folders assumed before this trace.

**`DriveFile`'s required fields** (`misskey_dart`'s
`lib/src/data/base/drive_file.dart`): `id`, `createdAt`, `name`, `type`
(MIME string), `md5`, `size` (bytes), `isSensitive`, `properties`, `url`.
Nullable: `blurhash`, `thumbnailUrl`, `comment`, `folderId`, `folder`,
`userId`, `user`. Nested `properties` (`DriveFileProperties`) is itself
required but every one of its own fields — `width`, `height`,
`orientation`, `avgColor` — is nullable, so a non-image attachment can
project `properties: {}`. **There is no discriminator key on `DriveFile`**
(unlike `User`/`UserDetailed`'s `"url"`/`"avatarId"` keys traced above) —
every field decodes structurally, so there is no equivalent trap to guard
against here. `url` (the full-resolution asset) is required; `thumbnailUrl`
is optional, so a backend that does not generate thumbnails may omit it
(Aria's `media_card.dart`/`post_file_thumbnail.dart` fall back to `url`
when `thumbnailUrl` is null).

**`url`/`thumbnailUrl` are consumed as plain HTTP(S) URLs through Aria's
image cache manager** (`cacheManagerProvider.getSingleFile(file.url)` in
`profile_page.dart`, and equivalently in the media widgets) — an ordinary
GET with no special headers traced. This is consistent with either serving
the byte stream directly or issuing a redirect to it, but redirect-based
serving specifically (a `localdisk`/`s3compat` backend issuing a 302 to a
presigned URL) has not been traced against a real client HTTP stack and is
**要実機確認** per this document's existing convention, not a blocker to
implementing it.

**Endpoints traced as genuinely unused by Aria** (**不要**, also listed in
the allowlist table above): `drive/stream`, `drive/files/find`,
`drive/files/check-existence`, `drive/files/find-by-hash`,
`drive/folders/find`. None has any Aria call site; `misskey_dart` defines
wrapper methods for all of them regardless (the client library's surface is
broader than what Aria itself uses, same pattern already noted for
`notifications/mark-all-as-read`).

**`drive/files/move-bulk` degrades gracefully when absent.**
`DriveFilesNotifier.moveBulkFrom` first calls this service's already-traced
`POST /api/endpoints` (see this document's dedicated section above) and
checks whether `"drive/files/move-bulk"` is in the returned list; only if
so does it call the bulk endpoint, otherwise it issues one
`drive/files/update` (`folderId` change) per file instead. **This service
can skip implementing `drive/files/move-bulk` entirely by simply never
including that name in its `/api/endpoints` response** — Aria's fallback
path is exercised automatically, not an error state.

**`notes/create`'s `fileIds`**: `NoteDraft.setFiles` sets `fileIds: null`
(and `files: null`) when the attachment list is empty, and
`toNotesCreateRequest()` passes `fileIds` straight through — so an
attachment-less post omits the `fileIds` key entirely (same
null-omission convention as every other optional field this document
records), never an explicit `fileIds: []`. When files are attached, the
array is the drive-file IDs in the composer's own (user-reorderable via
`AttachesNotifier.reorder`) display order — this service must preserve
that order as the attachment order, which is exactly plan-issue-77 v2
§2.3's `entry_files.position` design.

**A single drive file can be attached to more than one note — this
corrects plan-issue-77 v2 §2.3's tentative "1:1 might be enough"
framing.** `PostFile` (`lib/model/post_file.dart`) has two variants:
`LocalPostFile` (a not-yet-uploaded local file) and `DrivePostFile` (an
existing `DriveFile` selected from the drive, not freshly uploaded).
`AttachesNotifier.add`/`addAll` de-duplicate a `DrivePostFile` only within
the *same* compose session by drive-file ID; nothing stops the same
already-uploaded file from being selected into a second, independent post
later. This is corroborated by `drive/files/attached-notes`
(`AttachedNotesNotifier`, traced above) existing specifically to answer
"which notes is this drive file attached to" as a one-to-many query, and by
`profile_page.dart`'s `_getFile` reusing a `DrivePostFile.file` directly
(no re-upload) when the user picks an existing drive file without
cropping. **`entry_files` must therefore support one `files` row
referenced by many `entries` rows** (a genuine many-to-many join table,
not a 1:1 pointer plan-issue-77 v2 tentatively allowed) — deleting one
`files` row while `entry_files` rows still reference it, or vice versa, is
the ownership/GC edge case PR6/PR7 must define, not something PR0 resolves.

#### PR6 implementation notes (Issue #77, `entry_files` + `internal/httpserver/noteapi_{handlers,wire}.go` + `internal/drive.Service`)

- **The files-row-still-referenced half of the edge case above is
  resolved: rejected explicitly, never cascaded.**
  `internal/drive.Service.DeleteFile` now checks
  `EntryFileRepository.CountByFile` first and returns `ErrFileAttached`
  (wire: `FILE_ATTACHED`) if the file is still attached to any entry —
  the same stance `ErrFolderNotEmpty` already takes for a non-empty
  folder (AGENTS.md: unsupported/unsafe operations must fail explicitly,
  not silently cascade or surface as a raw SQLite foreign-key
  constraint error). The entries-row half never arises in practice:
  `notes/delete` maps onto `hidden_at` rather than a hard delete
  (`docs/decisions/0004-note-delete-as-hide.md`), so an entry_files row's
  `entry_id` never dangles.
- **fileIds is validated (ownership + `purpose = 'attachment'`) before
  any entry or entry_files row is written**, via the new
  `drive.Service.ValidateAttachmentFiles` — a bad ID fails the whole
  `notes/create` call with `NO_SUCH_FILE`, never a partially-created
  note.
- **The attachment link itself is written inside the same transaction
  that creates the entry**, through `internal/timeline.Service`'s
  existing `EntryHook` extension point (Issue #53's own mechanism for
  running the Open WebUI bridge post-creation) — `httpserver`'s new
  `combineEntryHooks` composes both hooks into one, since
  `CreateRootWithHook`/`CreateReplyWithHook` accept only a single hook.

**Avatar upload sequence confirms plan-issue-77 v2 §2.5's hypothesis.**
`profile_page.dart`'s `_getFile` calls
`drive.files.create`/`createAsBinary` (optionally after a client-side
crop, which re-uploads the cropped bytes as a *new* drive file rather than
mutating the original), then the caller passes the resulting `DriveFile.id`
to `INotifier.setAvatarId`, which does
`apiService.post('i/update', {'avatarId': avatarId}, excludeRemoveNullPredicate: (_, _) => true)`
— i.e. `POST /api/drive/files/create` (or `createAsBinary`) followed by
`POST /api/i/update {"avatarId": "<fileId>"}`, and `avatarId: null` is sent
explicitly (not omitted) to clear the avatar, which is why the call site
opts out of this document's usual null-field-omission rule for this one
field. PR5 accepts an explicit-null `avatarId` as "remove avatar,"
per this finding, rather than rejecting it as a validation error.

**A null `avatarUrl`'s own default/placeholder rendering is Aria's
responsibility, not this service's** (Issue #77 AC: "新規インストール
直後でもfavicon・アプリアイコン・デフォルトアバターが表示される"). This
service's obligation is exactly what `userLite`/`userDetailedNotMe`
already do — the `avatarUrl` key is always present and correctly `null`
until an owner/model actor's `avatar_file_id` is set (Issue #77 PR5) —
never to synthesize or host a fallback image of its own. Every
Misskey-compatible client, Aria included, is expected to render its own
placeholder (an initial, a generic silhouette, ...) for a `null`
`avatarUrl`, the same baseline behavior any Misskey server's client
already needs regardless of this deployment; no traced Aria source
confirms this specifically (**要実機確認**, this document's existing
convention for an unverified real-client-rendering claim), but there is
no `User`/`UserLite` wire field this service could populate instead —
misskey_dart's schema has no separate "default avatar URL" concept to
project. The app's *own* favicon/OGP/PWA icons (Issue #77 PR2) are a
different, already-fully-implemented case: those are this service's own
static assets with no per-user null state to consider.

**`POST /api/notes/timeline`'s `withFiles` flag is still accepted and
ignored even after PR6** (`internal/httpserver/noteapi_handlers.go`'s
`WithFiles *bool` field, per its own comment) — PR6 makes
`note.files`/`fileIds` real, but never wires `withFiles` itself into
`GetTimelineDesc`'s query. Aria sends it as a user-configurable "only
show notes with attachments" timeline filter reflecting an Aria setting,
not a fixed value; whether to start honoring it remains a PR7
implementation decision, not something this PR needed to resolve.

**`Note.files`/`Note.fileIds` default to `[]` when absent** (already noted
in this document's "Minimum Note contract" section below via
`misskey_dart`'s `@Default([])`), so returning an explicit empty array
(this service's behavior for every note with no attachments,
`internal/httpserver/noteapi_wire.go`) is safe and correct, and is now
joined by real data for a note that does have attachments.

#### PR3 implementation notes (Issue #77, `internal/drive.Service` + `internal/httpserver/drive_{handlers,wire}.go`)

Every endpoint this section's allowlist table marks 必要 for PR3 is now
implemented, with the following decisions this document's PR0 findings
did not already settle:

- **Raster-only, every upload.** Issue #77 v2's "SVG/ベクター画像は許容
  しない" scope decision applies to every `drive/files/create`/
  `upload-from-url` call, not only avatar uploads: every accepted file
  passes `internal/drive.ValidateImage` (PR1). This service is not a
  general-purpose file-storage Drive; it stores raster images only.
- **`force` is accepted and ignored.** Aria's two upload call sites
  (`DriveFilesNotifier.upload`/`.uploadBinary`) always send
  `force: true`; this service performs no content-hash deduplication at
  all, so there is no behavior for `force` to toggle.
- **`md5` is a real MD5 digest**, computed and stored alongside the
  `files` table's pre-existing `sha256` identity hash (migration 0024) —
  not SHA256 projected under the wrong wire key. No traced Aria call
  site reads this field's value, but the key itself is required on the
  wire.
- **`GET /files/{id}` is the anonymous serving route** `DriveFile.url`/
  `thumbnailUrl` resolve to (PR0: "an ordinary GET with no special
  headers"). It performs no ownership check by design: the id itself
  (`domain.NewID()`, 128 bits of `crypto/rand`) is the access control,
  the same unauthenticated-but-unguessable-URL design every Misskey/
  Mastodon-shaped service already uses for media. `thumbnailUrl` is
  always null — this service generates no separate thumbnail.
- **`upload-from-url` fetches synchronously**, through
  `internal/ingest/safehttp` (the same SSRF-protected client RSS/favicon
  fetching uses), before responding — unlike real Misskey's async
  behavior. Aria's only call site declares the method `Future<void>` and
  never inspects the response, so this is wire-compatible.
- **`drive/files/attached-notes` returned `[]` unconditionally until
  Issue #77 PR6 added `entry_files`** — see this section's earlier PR0
  note on why a file can be attached to more than one note, and PR6's own
  implementation notes above. It still verifies the given `fileId` is
  owned by the caller first (`NO_SUCH_FILE` otherwise), rather than
  querying attachments for any id unconditionally.
- **Folders are real** (migration 0025): create/list/show/update/delete,
  with a "cannot delete a non-empty folder" check
  (`FOLDER_NOT_EMPTY`, this service's own invented-but-Misskey-flavored
  error code — see the next point) and a "cannot move a folder inside
  its own descendant" cycle check (`INVALID_PARAM`).
- **Drive error ids/codes** (`NO_SUCH_FILE`, `NO_SUCH_FOLDER`,
  `FOLDER_NOT_EMPTY`, `FILE_TOO_LARGE`, ...) follow this document's
  already-established local convention (kebab-case `id`, SCREAMING_SNAKE
  `code` — see `NO_SUCH_NOTE`/`INVALID_PARAM` above): this service's own
  contract, not literal real-Misskey error UUIDs, since no traced Aria
  call site branches on a Drive error's specific id/code.
- **`POST /api/drive`'s `capacity` is cosmetic.** This deployment
  enforces no real storage quota beyond `DRIVE_MAX_FILE_BYTES` per
  upload; `capacity` is a fixed constant (`cmd/server/main.go`'s
  `driveCapacityBytes`, 10 GiB) purely so Aria's used/total bar renders
  something reasonable.

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

**`host: null` means local user, with exactly one exception (Issue #52).**
Every actor this service can author an entry with projects `host: null`
(the owner, and the reserved `assistant`/`system` presentation actors) —
except an Open WebUI model's VirtualActor, which projects a non-null,
deployment-configured presentation host
(`internal/httpserver.resolveUserLite`; see
`docs/operations/configuration.md`'s "Open WebUI bridge" section for the
seeding and eligibility rules behind it). This is a fixed presentation
value, not federation: the host is never discovered, resolved, or
delivered to. No other route in this service's API surface (there is no
`users/show`, so a client cannot look the host up) treats it as
anything but a label on that one note's author.

The redacted fixtures are [`fixtures/note.json`](fixtures/note.json) (an
ordinary local author, `host: null`) and
[`fixtures/note-virtual-actor.json`](fixtures/note-virtual-actor.json) (a
VirtualActor-authored reply to it, `host` set to the synthetic
presentation host `openwebui.example.net`). Both intentionally contain no
access token, real user content, real instance host, or personal
identifier.

**A VirtualActor's reply arrives asynchronously (Issue #53).** With the
Open WebUI outbound bridge enabled
(`docs/operations/configuration.md`'s "Outbound turn bridge" section),
`POST /api/notes/create`'s response never itself carries the model's
reply: the created note comes back immediately, exactly as it would with
the bridge off, and the VirtualActor-authored `llm_reply` child (the same
`note-virtual-actor.json` shape) appears later as an ordinary new entry
under the owner's post — visible the same way any other async reply is,
through a subsequent `notes/children`/`notes/conversation`/timeline read,
or through the `reply`-type `POST /api/i/notifications` entry this
service records once the turn actually succeeds (never for a turn that
fails, is left ambiguous, or is still pending). No existing endpoint's
request or response shape changes for this; only the timing of when the
child note exists differs from a synchronous reply.

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

**Since Issue #77 PR4 (ADR-0008), a `news` entry's provenance is no
longer text-only.** Before PR4, every `news`/`mail` entry projected
`user: {username: "system", host: null}` regardless of which RSS feed
or mailbox it came from — the table above's markers were the *only*
place provenance appeared. PR4 adds two real, structured signals on top
of that unchanged text-marker behavior:

- **`user.host` is the feed's own real origin domain** for a `news`
  entry ingested from an RSS-kind source registered after PR4 shipped
  (`ActorExternalSource`, ADR-0008) — `@<username>@<feed's real host>`,
  not `system`. `mail` entries are unaffected and still project as
  `system`: ADR-0008 excludes IMAP from this treatment. A `news` entry
  from a source that predates PR4 (no `ActorID` on its
  `domain.ExternalSource` row) also still projects as `system`, exactly
  as before — nothing is backfilled.
- **`note.url` is the source item's own article URL** (nullable, per the
  "Minimum Note contract" section below — always `null` before PR4,
  since no field carried it): `domain.Entry.ProvenanceURL`, denormalized
  from `domain.ExternalItem.ProvenanceURL` at ingestion time
  (`timeline.Service.CreateExternalEntry`) and projected verbatim.

The text markers table above is unchanged by PR4 — the `"[news: ...]
..."` body marker and the new `user.host`/`note.url` fields are
independent, complementary signals a client can use together, not a
replacement of one by the other.

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

Streaming was **not an MVP requirement** (Issue #2). The observation showed
that Aria's timeline is initially populated and paginated through HTTP
`/api/notes/timeline`; its WebSocket timeline channel only inserts newly
arriving notes and provides a live UX enhancement. Post success, reload,
restart persistence, reply creation, and thread viewing could all be
verified without a stream.

MVP therefore used poll/reload semantics as the release gate, and that
remains true today: HTTP load/reload/pagination is still the correctness
source of truth for every journey this contract covers, not the stream. A
stream outage must never make the HTTP timeline, post, or thread endpoints
unavailable.

**Updated by Issue #95 PR2**: the "future streaming adapter" this section
originally deferred has now shipped, narrowly. `GET /streaming` pushes a
real live-insertion event — but only a `homeTimeline` `"note"` create event,
nothing else — see "Traced server→client `channel`/`note` push event shape"
below for the confirmed wire shape and internal/streamhub's role, and
"Issue #41: minimal stub, not a capability" just below for how this sits
alongside the earlier handshake-only stub. This is still not "full Misskey
streaming compatibility": reaction, notification, mention, renote, and every
other `ChannelStreamEvent` case remain unimplemented and un-pushed, a
deliberate, permanent non-goal (README.md's "Known limitations"), not
deferred work. Push delivery is additive UX only — its failure in any form
must never affect post success or the HTTP timeline's own correctness, the
same rule this section has always stated for a stream outage.

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
for `connect`, tracks `disconnect`/`subNote`/`unsubNote` state, and pings
every `StreamPingInterval` (default 30s) to stay alive through an
idle-timeout intermediary. At the time, none of that tracked state was ever
read back — the "not an MVP requirement" decision above was unchanged in
substance, a wire-availability fix for the symptom above, not a promotion
of streaming to a supported capability. **This is no longer the full
picture as of Issue #95 PR2**: `connect`'s `channel`/`id` state (only —
`subNote`/`unsubNote`'s note-ID tracking is still never read back by
anything) is now read to drive a real `homeTimeline` `"note"` push — Issue
#41's own doc comment already noted this connection state existed only so
"a future real-event-push feature has an established place to look up ...
against". See the "Streaming decision" section's Issue #95 update above and
"Traced server→client `channel`/`note` push event shape" below for the
confirmed wire shape. A successful handshake is still not evidence that
every kind of live timeline update works — only home-timeline note creation
is ever pushed.

As with Issues #7 and #13, **no real Aria/Misskey end-to-end verification
has been performed for this issue**: `streaming_handlers_test.go` proves
the handshake, auth gate, keepalive, and message handling against a real
WebSocket connection, but the client on the other end is
`gorilla/websocket`'s dialer, not a pinned `misskey_dart`/Aria build — it
does not exercise `StreamingService`'s actual reconnect/error-surfacing
path this issue exists to stop. The message shapes below were confirmed by
reading the pinned client source, not by running it, so whether opening a
real Aria timeline tab against this endpoint is actually silent (this
issue's acceptance criterion) remains 要実機確認 until that run happens.

#### Traced `/streaming` message shapes

Source-traced against the pinned `misskey_dart` streaming request enum
([`streaming_request_type.dart`](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/enums/streaming_request_type.dart)),
its [`StreamingService` implementation](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/services/streaming_service_impl.dart),
and Aria's
[`timeline_stream_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/streaming/timeline_stream_provider.dart) —
not guessed from the sakurasato precedent alone (`plan-issue-41` §0 required
this before implementation).

Every client-to-server frame uses the envelope `{"type": ..., "body": ...}`.

| `type` | `body` fields observed | Server reply |
| --- | --- | --- |
| `connect` | `channel`, `id`, `params` (Aria's home-timeline provider sends these three; `params` carries filters such as `withRenotes`/`withReplies`/`withFiles` that this handler reads nothing from — Issue #95 PR2's push delivery sends every homeTimeline note unfiltered, see "Traced server→client `channel`/`note` push event shape" below) | `{"type":"connected","body":{"id":...}}` |
| `disconnect` | `id` | none |
| `subNote` | `id`, `params` | none |
| `unsubNote` | `id`, `params` | none |

`misskey_dart`'s enum additionally declares abbreviated wire values `sn` and
`un` as aliases for `subNote`/`unsubNote`; the pinned Aria commit's timeline
provider does not call them (it only sends `connect`/`disconnect` for the
home timeline), but the server implementation accepts all four spellings
defensively.

Aria never actually waits for or requires the `connected` ack:
`StreamingService`'s incoming-message handler broadcasts every frame to its
listeners without matching it against a pending request. Its response model
(`StreamingResponse`, `@Freezed(unionKey: "type", fallbackUnion:
"fallback")`) falls back to an opaque `StreamingChannelUnknownResponse` for
any `type` it does not statically declare (`connected` is not one of its
declared cases) rather than throwing — confirmed against the pinned
`misskey_dart` commit, not assumed. So a malformed or absent `connected` ack
would not have reproduced the class of error this issue fixes; it is sent
because the sakurasato precedent and real Misskey servers do, not because
Aria's pinned version depends on it.

#### Traced server→client `channel`/`note` push event shape (Issue #95 PR2)

Before Issue #95, `GET /streaming` never sent a server-initiated frame
beyond the `connected` ack above. `plan-issue-95` §3.4 required this shape
be confirmed from the pinned client source before implementation, the same
method Issue #41 used for the client-to-server frames above — not guessed
from the sakurasato precedent or from real Misskey server behavior alone.

Source-traced against the pinned `misskey_dart`'s
[`StreamingResponse`/`ChannelStreamEvent` union
definitions](https://github.com/poppingmoon/misskey_dart/blob/14176c515a005a9fb01d3e6365a49b5a5d387a92/lib/src/data/streaming/streaming_response.dart)
and Aria's own
[`timeline_stream_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/streaming/timeline_stream_provider.dart),
[`incoming_message_provider.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/provider/streaming/incoming_message_provider.dart),
and
[`model/streaming/incoming_message.dart`](https://github.com/poppingmoon/aria/blob/a66c9303995e7c964765cf382de6a9b0e3f4a3b6/lib/model/streaming/incoming_message.dart).

Every server-to-client frame Aria's `incomingMessageProvider` decodes uses
the same envelope as the client-to-server side, `{"type": ..., "body":
...}` (`IncomingMessage.fromJson`): the outer `type` decodes to an
`IncomingMessageType` enum (`channel`, `noteUpdated`, `emojiAdded`,
`emojiUpdated`, `emojiDeleted`, `announcementCreated` — an unrecognized
value decodes to `null` rather than throwing, same
`unknownEnumValue: JsonKey.nullForUndefinedEnumValue` pattern as
`UserDetailedNotMe`'s permissive fields elsewhere in this document), and
`body` decodes as an **untyped** `Map<String, dynamic>` — Aria does not
require `misskey_dart`'s own generated `StreamingResponse` union to parse
an incoming frame at all; only this thin `IncomingMessage` envelope.

For the home timeline tab specifically, `timeline_stream_provider.dart`'s
`timelineStream` only reacts to a frame when the outer `type` is
`IncomingMessageType.channel` **and** `body['id']` equals the `id` this
same connection sent on its own `connect` call (this document's existing
`connect` row — home timeline connects with `channel: "homeTimeline"`).
When both match, it pattern-matches the inner `body` map itself as
`{'type': 'note', 'body': final Map<String, dynamic> body}` and decodes
that inner `body` with `Note.fromJson` — the same wire `Note` shape as
every other note-returning endpoint in this document, decoded through
`(*Server).projectNote` server-side. This inner shape is exactly
`misskey_dart`'s own `ChannelStreamEvent.note` case
(`@FreezedUnionValue("note")`, fields `id: String`, `body: Note`, plus an
unused `type: ChannelEventType?` — a redundant re-decode of the same
`"note"` discriminator string into `misskey_dart`'s own enum that Aria's
hand-written pattern match above never reads).

So the confirmed wire shape for a new home-timeline note push is:

```json
{
  "type": "channel",
  "body": {
    "id": "<the id the client sent on its homeTimeline connect>",
    "type": "note",
    "body": { "...": "Note, identical shape to every other note-returning endpoint" }
  }
}
```

No other `ChannelStreamEvent` case (`reply`, `mention`, `renote`,
`notification`, `reacted`/`unreacted` via `noteUpdated`, ...) is sent —
this document's "WebSocket `/streaming` timeline channel" row's
reaction/notification non-goal is unchanged by Issue #95; only
`homeTimeline`-subscribed `connect` ids ever receive a `"note"` push, and
only for that entry's own creation.

**Implemented as** `internal/streamhub.Hub` (a neutral in-process
publish/subscribe broker `internal/timeline` and `internal/httpserver`
both depend on, satisfying AGENTS.md's "Domain/use-case code must not
depend on HTTP handlers" without a construction-order cycle in
`cmd/server/main.go`) and `serveStreamConn`'s new subscription select loop
(`internal/httpserver/streaming_handlers.go`). Delivery failure of any
kind (no subscriber, a full per-connection buffer, a WebSocket write
error) never affects `POST /api/notes/create`'s own success — see
`internal/timeline`'s `EntryBroadcaster` doc comment for why, mirroring
this document's existing "a failed optional stream must not make HTTP
timeline or post operations fail" rule.

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
  entry (including archived/hidden ones) authored by the actor. **Updated
  by Issue #23 PR3**: since `/api/notes/delete` shipped, this count
  excludes archived/hidden entries instead — see the "Issue #23 PR3
  implementation notes" section below.
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

## Issue #23 PR1 implementation notes

Issue #23's PR1 (`/api/i/update`, owner profile self-edit) traced Aria's
Settings → Profile screen and the pinned `misskey_dart` request model before
writing any handler code, per this document's 必要/不要 method and AGENTS.md's
"do not silently invent a protocol" rule. The trace surfaced a scope
conflict against Issue #23's acceptance criteria, which is recorded here
rather than resolved unilaterally:

- **Finding**: neither Aria (`profile_page.dart`, `account_settings_page.dart`)
  nor the pinned `misskey_dart` `IUpdateRequest` model has any way to send a
  `username` field to `/api/i/update` (see this document's `/api/i/update`
  section above for the full source trace). Real Misskey does not expose
  self-service username renaming through this endpoint either. Only `name`
  (display name) is a traced, observed, implementable field.
- **Conflict**: Issue #23's `/api/i/update` acceptance criteria ask for both
  username and display-name self-edit ("owner actor の username/表示名を更新
  できる"), and `plan-issue-23`'s PR1 section calls for a mutable `username`
  column alongside `display_name`. Neither source anticipated that the trace
  itself would come back with no observed `username` wire path at all — the
  plan's own precedent for this situation (drop a sub-scope to 不要 when the
  trace shows Aria never exercises it, as applied to `/api/notes/renote`) has
  not yet been applied here because it changes what the parent issue's
  acceptance criteria promise, which is a call for the issue owner rather
  than an implementation detail.
- **Status**: implementation paused at this point pending a decision on how
  to reconcile the acceptance criteria with the trace (e.g., narrow
  `/api/i/update`'s AC to display-name-only and record `username` as 不要 the
  same way `/api/notes/update` is; or pick a different mechanism entirely
  for username changes, such as remaining `OWNER_USERNAME`-config-only via
  SSH/CLI). No `actors` schema change, repository method, or HTTP handler for
  `/api/i/update` has been written yet.
- **Resolution (2026-09-06, owner confirmed)**: Issue #23's Scope and
  Acceptance criteria were narrowed to display_name-only, and `username`
  recorded as a permanent Non-goal (`OWNER_USERNAME` config remains its
  source of truth). PR1 is implemented on that basis: migration
  `0012_actor_profile.sql` adds `actors.display_name` only (no `username`
  column), and `POST /api/i/update` accepts only the `name` field.

## Issue #23 PR2 implementation notes

Issue #23's PR2 is a source-trace-and-record pass over the remaining five
endpoints from Issue #23's scope expansion (`/api/notes/delete`,
`/api/notes/renote`, `/api/notes/reactions/*`, `/api/notes/mentions`,
`/api/i/notifications` + `/api/notifications/mark-all-as-read`), plus
`/api/stats`, which this PR both traces and implements (see this
document's per-endpoint sections above for the full trace of each). No
endpoint from the first group is implemented in this PR: PR3/PR4/PR5/PR6
own that work and can build directly on the contracts recorded here
without re-tracing.

Three findings revise `plan-issue-23`'s assumptions and are called out
here so they are not missed by whichever PR consumes them:

- **`/api/notes/reactions/list` does not exist** — the pinned
  `misskey_dart`'s list-who-reacted call is wire path `notes/reactions`,
  not `notes/reactions/list`. A path correction only; PR4's scope is
  unaffected.
- **`/api/notes/mentions` is real, reachable Aria traffic**, not the
  edge case `plan-issue-23`/Issue #23 treat it as — it fires whenever the
  owner adds an optional "Mention" or "Direct" home-timeline tab. This
  does not by itself force PR5 to pick the full-implementation option
  over the minimal-empty-array option, but that choice is now known to
  affect a real, owner-reachable tab rather than dead code.
- **`/api/notifications/mark-all-as-read` has no wire path at all** —
  not merely unused by Aria, but structurally absent from the pinned
  `misskey_dart` dependency (`lib/src/misskey_i.dart` defines no method
  for it). Issue #23's own common acceptance criteria already cover this
  outcome (record as 不要, do not implement), so this needed no owner
  escalation the way PR1's `username` finding did — but PR6 should not
  budget for adding this route, contrary to `plan-issue-23`'s and Issue
  #23's explicit mention of it in scope text.

`/api/notes/renote`'s absence (confirmed: no dedicated wire path; renote
is `notes/create` + `renoteId`) matches what `plan-issue-23` and Issue
#23's Non-goals already anticipated as the likely outcome, so it is a
confirmation rather than a new finding.

`/api/stats` is implemented in this PR: `EntryRepository.CountAll` (a
new, narrow addition alongside the existing `CountByAuthor`) backs
`notesCount`/`originalNotesCount`; every other field this service has no
concept for (federation, drive, reactions not yet persisted) is a fixed
default, matching `note.go`'s existing always-present-default
convention for the same kind of field. The handler is registered
alongside `/api/meta`/`/api/endpoints` — anonymous, no `RequireScope`
wrapper — because the trace showed Aria's only call site never attaches
an API token.

## Issue #23 PR3 implementation notes

PR3 implements `POST /api/notes/delete`, mapping it onto
`timeline.Service.SetHidden(id, true)` per
`docs/decisions/0004-note-delete-as-hide.md` rather than adding a new
hard-delete primitive. No migration was needed: the existing
`entries.hidden_at` column (from
`internal/storage/sqlite/migrations/0004_threads_entries.sql`) is reused,
and this PR is what gives it its first concrete product meaning.

- `Server.handleNotesDelete` restricts deletion to the owner's own
  `user_post` entries (`entry.Kind == domain.EntryUserPost &&
  entry.AuthorActorID == LocalActorIDFromContext(ctx)`), mirroring
  `timeline.Service.EditPost`'s existing author/kind restriction. Every
  disallowed case — unknown note ID, already hidden/archived note, and a
  note that exists but is not the owner's own `user_post` — returns the
  same `NO_SUCH_NOTE` this document's other note-reading endpoints already
  give for an unknown/hidden/archived ID, so the endpoint cannot be used
  to probe which case applies.
- A deleted note's children are unaffected (their own `HiddenAt` stays
  `nil`), so they remain visible in the home timeline and by direct ID —
  matching real Misskey's "delete removes the parent, orphans the
  children" behavior, and requiring no new code since `entryVisible`
  already evaluates each entry independently.
- `EntryRepository.CountByAuthor`'s SQL now adds `AND archived_at IS NULL
  AND hidden_at IS NULL`, so `/api/i`'s and `/api/miauth/{session}/check`'s
  `notesCount` decrements on a successful delete, matching what Aria's UI
  expects (see the updated note under this document's Issue #7
  implementation notes above). This reuses the existing partial index
  `idx_entries_timeline_default` (`WHERE archived_at IS NULL AND
  hidden_at IS NULL`), so no new index was added.
  `EntryRepository.CountAll` (PR2's `/api/stats` server-wide count) is a
  separate method and keeps counting every entry regardless of
  archived/hidden state — this PR does not touch it.
- No new scope: `write:notes` was already granted for `/api/notes/create`.

## Issue #23 PR4 implementation notes

PR4 implements `POST /api/notes/reactions/create`, `/delete`, and the
"who reacted" list `POST /api/notes/reactions` (see this document's
per-endpoint section above for the full trace and design). Summary of
what shipped:

- New `reactions` table (migration `0013_reactions.sql`) and
  `domain.ReactionRepository` (`internal/storage/sqlite/reaction_repository.go`).
  `Create` is an upsert (`INSERT ... ON CONFLICT (entry_id,
  reactor_actor_id) DO UPDATE`), so it never conflicts on the table's
  `UNIQUE(entry_id, reactor_actor_id)` constraint regardless of whether a
  caller reaches it via Aria's own delete-then-create `changeReaction`
  flow or a direct repeat call; `Delete` is unconditional (no
  `requireRowAffected`), so removing an absent reaction is not an error.
- New `internal/miauth` scopes `read:reactions`/`write:reactions`, added
  to `grantableScopes` so Aria's already-requested permission list
  (`docs/compat/aria-v1.5.11.md`'s `GET /miauth/{session}` section) grants
  them on a fresh MiAuth approval. A token issued before this PR does not
  carry them; re-approving through `miauthctl` is the only way to add
  them retroactively (Issue #23 §3 "既存 API token への新規 scope 反映",
  still open as an operational question, not a blocker for this PR).
- Reaction target is not restricted to the owner's own `user_post`
  entries the way `/api/notes/delete` is: only `entryVisible` gates a
  reaction target, so an assistant-authored `llm_reply` or a
  system-authored `news`/`mail` entry can be reacted to, matching
  plan-issue-23 §1 PR4's recommendation and the PR2 trace finding that
  Aria's `note_footer.dart` places no `isMe`-style guard on this.
- `note.Reactions`/`note.myReaction` are populated with real data (via
  `ReactionRepository.CountsByEmoji`/`GetByActor`, wrapped by
  `timeline.Service.ReactionCounts`/`MyReaction`) on every note-returning
  endpoint — `/api/notes/create`, `/timeline`, `/show`, `/conversation`,
  and `/children` — not only within the three reactions endpoints
  themselves, since Aria's note footer needs this data whenever it
  renders any note. This is done through a new `(*Server).projectNote`
  wrapper (`internal/httpserver/noteapi_wire.go`) rather than folding it
  into `newNote` itself, keeping `newNote` a pure, repository-free
  conversion function.
- `reaction` accepts only a plain Unicode emoji;
  `isCustomEmojiShortcode` rejects a `:name:`/`:name@host:` shortcode
  with `UNSUPPORTED_FEATURE` (custom emoji/drive remain this issue's
  Non-goals).
- `POST /api/stats`' `reactionsCount` is no longer a fixed `0`: a new
  `ReactionRepository.CountAll` backs it (see this document's `POST
  /api/stats` section).

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
