# ADR-0002: Approve local MiAuth sessions through SSH and CLI

- Status: Accepted for Issue #28
- Date: 2026-09-05
- Scope: single-owner MVP
- Supersedes: ADR-0001's upstream owner-verification and bootstrap decisions

## Context

ADR-0001 assumed the owner had an upstream Misskey account at a configured
`IDENTITY_ORIGIN`. That account does not exist in the intended deployment,
so upstream MiAuth cannot be the owner's trust anchor. The deployment is
already administered through authenticated SSH access to its host.

Aria's observed contract does not require upstream verification. It opens
`GET /miauth/{session}` and later polls
`POST /api/miauth/{session}/check`; reaching a client callback is not proof
that the local session is authorized.

## Decision

The server creates a `LocalMiAuthSession` in `created` state and never
authorizes it from a public HTTP route. An operator with host access uses
`miauthctl list` to inspect pending requests and then runs
`miauthctl approve <session-id>` or `miauthctl reject <session-id>`.
Approval atomically transitions an unexpired session from `created` to
`authorized`. Aria's existing `/check` call atomically consumes that session
and mints the local API token exactly once.

The first successful approval creates the sole Owner actor. Later approvals
reuse it. `actors.UNIQUE(actor_type)` is the compare-and-set boundary for
concurrent first approvals; there is no public first-login-wins path.

If Aria supplies an exact-match-allowlisted client callback, the start route
redirects there immediately with the route session ID. Without a callback it
shows a page explaining that operator approval is pending. Neither response
authorizes the session.

ADR-0001's local-session/API-token separation and exact effective-scope
rules remain in force. Its upstream session, upstream token, owner binding,
bootstrap gate, identity-origin, and allowlisted-upstream-user decisions are
superseded.

## Threat model

Authenticated host access is the trust point. Anyone able to run
`miauthctl` with write access to `DB_PATH` can approve sign-ins and revoke
tokens; SSH access, OS accounts, file permissions, and audit policy are
therefore operational security boundaries outside this service.

An attacker can create arbitrary pending sessions through the public start
route. The operator must verify that the session ID and displayed request
details correspond to the Aria attempt they initiated. To reduce accidental
approval, `miauthctl approve` displays the session ID, creation time and age,
requested permissions, and callback, then requires typing `yes`. `--yes`
exists only for trusted automation. All untrusted fields are sanitized before
terminal output to prevent control-sequence injection.

Route session IDs remain bearer correlation secrets: they are not logged,
but possessing one still does not authorize it. Sessions expire after ten
minutes, approval/rejection is guarded by atomic state-and-expiry checks, and
`/check` has a single winner. Local API tokens remain stored only as hashes.

## Consequences

- The service no longer performs outbound Misskey requests for login.
- `IDENTITY_ORIGIN`, `ALLOWED_MISSKEY_USER_ID`, and
  `UPSTREAM_HTTP_TIMEOUT` are removed.
- Upstream sessions/tokens, owner bindings, bootstrap gates,
  `internal/provider/misskey`, and `bootstrapctl` are removed.
- Existing Owner actors, local API tokens, posts, and related user data are
  preserved by the forward migration.
