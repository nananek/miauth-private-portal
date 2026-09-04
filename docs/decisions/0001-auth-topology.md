# ADR-0001: Separate local and upstream MiAuth boundaries

- Status: Superseded by ADR-0002 (see `0002-ssh-cli-auth.md`)
- Date: 2026-09-03
- Scope: single-owner MVP

## Context

The service presents a small Misskey-compatible surface to Aria, while the
owner identity and learning data may be backed by an upstream Misskey
instance. Aria's MiAuth session is therefore not the same credential as the
upstream owner's MiAuth session. Treating them as one session would allow a
callback, token, or user ID from one trust boundary to be replayed in the
other boundary.

This ADR fixes the boundaries and the owner-binding rules before endpoint,
database, or authentication implementation begins. The wire details observed
from Aria are recorded separately in
[`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md).

## Decisions

### 1. Use configured local and identity origins

The deployment has two distinct canonical origins:

- `LOCAL_ORIGIN` is the configured public origin of this service (HTTPS in
  production). Aria uses it for `/miauth` and `/api/*`; it is also the base for
  the internal server-side callback endpoint.
- `IDENTITY_ORIGIN` is the fixed upstream Misskey origin used for owner
  verification and upstream Misskey provider requests. It is an HTTPS origin
  in production. Any later external-provider adapter, such as Open WebUI,
  uses its own explicitly allowlisted origin and never reuses this value.

An origin includes scheme, host, and an explicit port when one is configured;
paths are not accepted as either origin. The server uses only
`IDENTITY_ORIGIN` for upstream Misskey requests and issuer/instance
validation, and only `LOCAL_ORIGIN` for the local compatibility surface and
its internal callback endpoint. The two values may differ and are never
selected by a client. A client return callback is checked against its separate
exact-match allowlist.

The server MUST NOT accept an arbitrary upstream host or redirect destination
from a client request. The internal callback is fixed under `LOCAL_ORIGIN`.
The client return callback is a separate exact-match deployment allowlist; it
may contain Aria's `aria://aria/miauth` callback as an explicitly configured
non-HTTPS entry. That client callback is never forwarded to the upstream
provider, and all server-side callbacks remain HTTPS in production.

### 2. Bind one owner, with an explicit bootstrap gate

`ALLOWED_MISSKEY_USER_ID` is the preferred and default production control. A
successful upstream verification is accepted only when its opaque user ID
matches this value and its instance matches the configured identity origin.
The pair `(upstream instance origin, upstream user ID)` is stored as the
owner's upstream identity; the ID is never parsed or ordered by the service.

When `ALLOWED_MISSKEY_USER_ID` is unset, the service remains unbound until an
operator presents an explicit bootstrap gate. The gate is:

- generated with `crypto/rand` and shown only through the operator channel;
- single-use, bound to the configured identity origin, and valid for 15
  minutes;
- consumed with an atomic compare-and-set transaction; and
- invalid after a successful binding, expiry, revocation, or a failed binding
  attempt.

There is no public first-login-wins path. A normal Aria MiAuth flow cannot
create or replace the owner binding, and a later upstream account cannot
silently take ownership.

### 3. Keep four credential records distinct

The implementation must use distinct records and types for these values:

| Credential or session | Issuer / consumer | Lifetime and storage rule |
| --- | --- | --- |
| Aria-facing local MiAuth session | Local portal; Aria polls the local check endpoint | Aria supplies an opaque route ID; the server binds it to a crypto/rand state, a one-time 10-minute record, the internal callback under `LOCAL_ORIGIN`, an optional exact-match client return callback, requested scopes, and browser/session context |
| Upstream owner-verification MiAuth session | Upstream Misskey; local portal checks it | Opaque, one-time, 10-minute TTL; bind to the configured upstream origin and bootstrap gate when used |
| Local API token | Local portal; Aria sends it as `i` | Return only after successful local check; persist only a one-way hash, support revocation and rotation, never log the raw token |
| Upstream Misskey token | Upstream Misskey; local provider adapter | Never return to Aria; if persistence is required, encrypt at rest with the deployment key, minimize retention, revoke and destroy on unbind or rotation |

If a raw upstream token must survive a short MiAuth polling window, it is
encrypted before persistence, exposed only to the checking transaction, and
deleted immediately after the compatibility-required response. Raw local and
upstream tokens, authorization headers, cookies, and MiAuth state are never
written to logs.

The public `{session}` route value in Aria's MiAuth URL is the same route
session ID used by `/api/miauth/{session}/check`. It is a high-entropy bearer
capability/correlation secret for accessing that one local auth attempt and
must not be logged or exposed in diagnostics. Possession permits polling or
checking the attempt only; it is not owner-binding or token-minting
authentication. The local server generates one unguessable `state` with
`crypto/rand`, binds it to the route record and fixed callback, validates it
in constant time, and consumes it once. No additional random internal record
ID is needed; the route ID is the record's unique opaque lookup key.

### 4. Scope tokens to the implemented surface

The `permission` query sent by Aria is a requested set, not evidence that the
service implements every requested endpoint. The local token receives only
the effective scopes approved by the service. Implemented endpoints enforce
their exact required scope; unsupported endpoints return a stable explicit
error and never fabricated success.

The broad Aria permission list is never passed through to the upstream
identity provider. Owner verification and provider access use a separate,
minimal server-side scope allowlist appropriate to the adapter; the local
compatibility scopes and upstream scopes are not interchangeable.

The Aria request contains permissions outside the MVP (for example drive,
pages, gallery, and chat permissions). Issue #5 must preserve login
compatibility while ensuring those permissions do not create capabilities that
the service does not implement.

### 5. Use explicit state and one-time transitions

Every local or upstream authentication attempt has an unguessable state value
generated with `crypto/rand`, a 10-minute TTL, a bound origin and callback,
and a one-time terminal transition. State comparisons are constant-time. A
check that races another check can have only one winner; all other checks see
an expired or consumed session and cannot mint another token. The public Aria
route session ID is the bearer correlation secret described above, not the
server-generated state and not owner authentication.

The minimum state machines are:

```text
local MiAuth:    created -> authorized -> consumed
                                  \-> expired / denied

upstream MiAuth: created -> authorized -> consumed
                                      \-> expired / denied

bootstrap gate: issued -> consumed
                         \-> expired / revoked / failed
```

The `check` operation atomically attempts the `authorized -> consumed`
transition; it is not a separate durable state. The names describe
server-side state, not a promise about an upstream Misskey's UI. Unknown,
malformed, replayed, mismatched, or late callbacks are terminal failures for
that attempt.

## Identity mapping and wire actors

The local owner identity and upstream identity are deliberately different:

| Concept | Stable value | Meaning |
| --- | --- | --- |
| Local owner | `owner_local_user_id` generated by this service | The only login-capable local actor; returned in local `UserDetailedNotMe` and `Note.user` projections |
| Upstream owner | `(IDENTITY_ORIGIN, upstream_user_id)` | The verified external identity used by adapters; both components are required |
| Assistant actor | Distinct reserved `assistant_local_user_id` with `actor_type=assistant` | A presentation actor for generated replies; never accepted by MiAuth and never a login principal |
| System actor | Distinct reserved `system_local_user_id` with `actor_type=system` | A presentation actor for ingestion/status entries; never accepted by MiAuth and never a login principal |

The three local actor IDs are distinct opaque strings, and the upstream user
ID, note IDs, session IDs, and provider IDs are opaque strings as well. The
owner, generic assistant, and system wire projections use `host: null`; a
provider-specific VirtualActor may use only an explicitly fixed presentation
host defined by its feature contract, never a copied upstream host. Such a
VirtualActor is a separate local actor ID and does not imply federation or
remote discovery. Actor type is domain metadata and must not be inferred from
a username or note text.

## Authentication sequence

The normal, already-bound flow is:

```mermaid
sequenceDiagram
    participant A as Aria
    participant L as Local portal
    participant B as Local MiAuth session
    participant O as Owner's browser
    participant U as Upstream Misskey

    A->>L: POST /api/meta (capability check)
    A->>L: GET /miauth/{session}?permission=...
    L->>B: Store route ID, generate state, 10-minute TTL, fixed callback binding
    O->>L: Open local approval flow
    L->>U: Redirect to fixed IDENTITY_ORIGIN MiAuth with state and fixed callback
    O->>U: Authenticate and approve as the owner
    U-->>L: Redirect to fixed LOCAL_ORIGIN callback with state/result
    L->>L: Validate issuer/internal callback, constant-time state, TTL, replay
    L->>L: Verify (IDENTITY_ORIGIN, user ID) matches ALLOWED/bound owner
    L->>B: Mark authorized for the bound local owner
    L-->>A: Redirect to the separately allowlisted Aria callback with session
    A->>L: POST /api/miauth/{session}/check (no i)
    L->>B: Atomic check-and-consume
    L-->>A: {ok:true, token: local-token, user: local owner projection}
    A->>L: POST /api/notes/timeline with i=local-token
    L->>U: Read through the upstream adapter using upstream-token
    U-->>L: Upstream data
    L-->>A: Local Note projections
```

Opening `GET /miauth/{route-session}` only creates the pending local record
and starts the redirect; it cannot transition the record to `authorized`. A
repeat GET must not replace the state, extend the TTL, or create a second
upstream attempt; it either resumes the same compatible pending attempt or
rejects an incompatible request without mutating the pending attempt. The
upstream step must require explicit
authentication/consent by the owner at the fixed `IDENTITY_ORIGIN` (an
already-present local browser cookie alone is not approval). A third party who
can cause the GET, or who possesses the route ID and calls `check`, can only
observe/poll that attempt and cannot approve it or mint a local token. Only the
validated, one-time callback above (or the explicit operator approval in the
bootstrap flow) can authorize the record. If Aria supplied a client callback,
the local service uses the exact stored value only for the final return to
Aria; it is not an upstream redirect target.

The unbound bootstrap flow is a separate operator-controlled sequence:

1. An operator opens the short-lived bootstrap gate.
2. The local portal creates an upstream MiAuth session bound to the configured
   `IDENTITY_ORIGIN`, a server-generated state, a fixed callback under
   `LOCAL_ORIGIN`, and the gate; it does not accept an origin or callback
   supplied by the browser.
3. The operator completes upstream authorization. The local portal validates
   the fixed issuer/callback allowlist, state in constant time, TTL, and
   one-time replay before verifying the exact configured user ID when present.
   It then atomically stores the owner mapping and encrypted upstream token.
4. The gate is consumed. Only then may a local Aria session be authorized.

The upstream token is used only by the local provider adapter. It is never
placed in a local MiAuth response, local cookie, Aria deep link, or local API
response.

For an already-bound deployment, every new local Aria MiAuth session requires
fresh owner verification through the fixed `IDENTITY_ORIGIN` redirect/callback
above. The stored owner binding or an existing upstream token alone cannot
approve a local route session or mint a local API token. The verified upstream
identity must match both the persisted binding and `ALLOWED_MISSKEY_USER_ID`
when that setting is configured. A pending, expired, replayed, mismatched, or
unavailable upstream verification leaves the local session unauthorized and
does not affect already-saved posts.

## Threat model

| Threat | Required control | Failure behavior |
| --- | --- | --- |
| Public first-login attacker claims the portal | Required `ALLOWED_MISSKEY_USER_ID`, or explicit bootstrap gate only | Deny binding and do not create a local owner |
| Callback or state replay | Random state, origin/callback binding, constant-time comparison, TTL, atomic one-time consume | Return a generic auth failure; do not mint a token |
| Concurrent bootstrap race | Single-use gate and database compare-and-set under a uniqueness constraint | Exactly one binding succeeds; all other attempts are denied |
| Login or instance mix-up | Fixed `LOCAL_ORIGIN` for the local surface and `IDENTITY_ORIGIN` for upstream; session records carry the applicable canonical origin | Reject mismatched host, scheme, port, or user ID |
| Open redirect / callback exfiltration | Exact callback allowlist; never echo a request URL | Reject the session or use the configured callback only |
| Local token theft from storage or logs | Hash local tokens; structured redacted logging; no authorization headers or cookies in logs | Revoke and rotate the token; do not reveal its value |
| Upstream token theft | Encrypt only when persistence is unavoidable; adapter-only access; revoke and destroy on unbind | Mark upstream integration unavailable without invalidating saved posts |
| Scope escalation through Aria's broad request | Effective-scope intersection and endpoint-level scope checks | Explicit permission error for unsupported capabilities |
| Session fixation or confused session IDs | Aria's route ID is only a bearer capability/correlation secret; server-generated random state, session-to-origin/callback binding, and no client-selected state | Reject mismatched session and expire it |
| Sensitive content exposure during diagnostics | Redact bodies, prompts, tokens, mail, cookies, and upstream errors | Keep only request/job IDs and categorized errors |

## Consequences and follow-up

- Issue #5 implements the state machines, bootstrap gate, owner binding, and
  token lifecycle.
- Issue #7 exposes only the minimal local Aria/Misskey surface described in
  the compatibility document.
- The local service can keep accepting user posts when upstream or LLM work
  is unavailable; owner authentication is not coupled to background jobs.
- Multi-user tenancy, general user administration, federation, and arbitrary
  upstream instances remain outside this ADR and the MVP.
