# Repository guidelines for agents

## Product boundary

This repository is a single-owner learning log and interactive timeline service written in Go. Aria is the client, and only the Misskey-compatible API surface proven necessary for the pinned Aria compatibility target is in scope.

The service stores user posts first, then performs LLM work and external-source ingestion asynchronously. Failure of an LLM, RSS source, IMAP server, or other integration must never make a user post disappear.

Do not add a custom web UI, federation, general-purpose Misskey compatibility, user registration/deletion/role management, PostgreSQL support, notebook export, or multi-user behavior unless an issue in the tracker explicitly promotes that work.

## Sources of truth

- The linked GitHub issue and its acceptance criteria define the current change.
- docs/decisions contains accepted architectural decisions.
- docs/compat contains observed Aria/Misskey wire contracts and the pinned client version.
- AGENTS.md contains repository-wide engineering and security rules.
- If these sources conflict or required behavior is ambiguous, stop implementation and record or request an ADR update; do not silently invent a protocol.

Use nananek/sakurasato and Aria as behavioral references. Do not copy third-party source code without first checking license and attribution obligations. Prefer contract fixtures captured from public protocol behavior.

## Work discipline

- Work on one roadmap child issue per pull request. Link the parent tracker and use Closes #child; do not close the tracker from a feature PR.
- Keep changes small and within the issue's stated non-goals.
- Preserve unrelated user changes and never commit secrets, local databases, logs, generated credentials, or real learning/mail content.
- Add or update documentation, configuration examples, migrations, and tests in the same change that introduces behavior.
- New dependencies require a concrete reason. Prefer the Go standard library or small, maintained libraries.
- Do not edit an applied migration. Add a new forward migration.

## Architecture

- Keep transport and Misskey wire models separate from domain models.
- Domain/use-case code must not depend on HTTP handlers, SQLite details, or a specific LLM provider.
- Put persistence and provider boundaries behind narrow interfaces. Keep SQLite-specific SQL inside the SQLite adapter so future PostgreSQL work does not require changing domain behavior.
- Configuration is typed and validated at startup. Environment variables override the config file. Missing required secrets or unsafe production settings fail closed.
- Use UTC internally. Treat external and Misskey IDs as opaque strings; do not infer ordering from their text.
- Schema changes go through embedded, versioned migrations. Enable SQLite foreign keys, a busy timeout, and a documented journal mode.
- Timeline pagination must use a stable cursor with a deterministic tie-breaker, not offset-only pagination.
- Durable jobs must be idempotent, leased, retryable with bounded backoff, and recoverable after process restart. Commit the post and durable job intent atomically.

## MiAuth and API compatibility

- Keep the Aria-facing local MiAuth session, the upstream owner-verification MiAuth session, local API tokens, and upstream Misskey tokens as distinct types and records.
- Generate session IDs, state values, setup tokens, and API tokens with crypto/rand. Compare secrets safely.
- Do not expose a public first-login-wins path. Initial owner binding requires an explicit, single-use bootstrap gate; ALLOWED_MISSKEY_USER_ID remains the preferred production path.
- Validate callback destinations and use a configured HTTPS identity-instance origin. Do not accept arbitrary upstream hosts or redirects.
- Store local API tokens as hashes. If a raw token must survive briefly for MiAuth polling, minimize its lifetime, encrypt it at rest when persisted, and scrub it after the compatibility-required window.
- Enforce exact scopes on implemented endpoints. Aria requesting a scope does not mean an unimplemented feature exists.
- Preserve the documented Misskey JSON field names, null/omission behavior, ID types, timestamp format, status codes, and error shape. Add a contract test before changing wire behavior.
- Unsupported endpoints must fail explicitly and consistently; do not return fabricated success.
- A local owner and any assistant/system presentation actors are wire projections, not additional login-capable users.

## Security and privacy

- Never log access tokens, API keys, cookies, authorization headers, MiAuth state, message bodies, mail bodies, or full LLM prompts. Structured logs use request/job IDs and redacted errors.
- Cookies, if used, must be Secure, HttpOnly, SameSite=Lax or stricter, narrowly scoped, rotated after authentication, and bounded by an explicit lifetime.
- Bound request sizes, timeouts, concurrency, and retry counts. Propagate context cancellation.
- Treat posts, feeds, mail, remote API responses, and LLM output as untrusted data. Escape rendered text and never execute or treat embedded instructions as system instructions.
- External fetchers require fixed schemes, host validation, redirect limits, and SSRF protections. IMAP is read-only by default and must not mark, move, or delete mail.
- User-authored text is authoritative. LLM classifications, summaries, tags, and related-post guesses are separate, versioned data and never overwrite it.
- Record provider, model, prompt version, status, timestamps, error category, and token usage when available. Do not retain hidden chain-of-thought.
- High-risk legal, medical, and financial replies must be qualified and must not claim certainty.

## Tests and verification

For Go changes, format changed files with gofmt and run:

- go test ./...
- go vet ./...
- go test -race ./... for concurrency, job, session, or shutdown changes

When make check exists, use it as the repository-wide check and keep it equivalent to the documented commands.

Tests must cover success and failure paths. Use temporary SQLite databases and fake HTTP/LLM/feed/mail servers; normal tests must not require real credentials or network access. External parity and Aria end-to-end tests stay opt-in behind explicit environment flags.

Each endpoint change needs protocol contract tests. Each migration needs a fresh-database test and an upgrade test. Each durable job needs restart, duplicate-delivery, retry-exhaustion, and cancellation coverage where applicable.

## Definition of done

A change is done only when:

- the child issue acceptance criteria are satisfied and evidenced;
- relevant tests and static checks pass;
- security and privacy failure paths are covered;
- configuration and operations documentation is updated;
- migrations are forward-only and restart-safe;
- no secret, real user content, or unrelated change is included;
- intentionally deferred work is linked to a follow-up issue rather than hidden in a TODO.
