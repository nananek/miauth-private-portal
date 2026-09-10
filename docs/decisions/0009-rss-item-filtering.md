# ADR-0009: RSS item filtering via a single global Starlark script

- Status: Accepted for Issue #135
- Date: 2026-09-10
- Scope: `internal/ingest/rss`'s item-level exclusion filter. Does not
  cover IMAP or any other future `ingest.Adapter` kind (RSS-only,
  matching ADR-0008's own RSS-only precedent for a different feature).

## Context

Issue #135 asks for per-article exclusion filtering (for example, drop
items whose title/body contains a banned string) before an item ever
reaches the timeline. `RSS_FEED_URLS` today only controls ingestion at
whole-feed granularity — there is no way to drop an individual article.
The issue proposes scripting the exclusion rule in
[Starlark](https://github.com/google/starlark-go) (pure-Go, sandboxed,
Python-subset syntax, the language Bazel `BUILD`/`.bzl` files use) so it
fits this repo's `CGO_ENABLED=0` single static binary policy, and leaves
three things open: rule granularity (global vs. per-feed), how the
script is configured (inline env var vs. file path), and whether
filtering is ingest-time-only or also retroactive.

## Decision

- **One global `RSS_FILTER_SCRIPT_PATH`-named script, not a per-feed
  mechanism.** A script receives each candidate item's `source_host`/
  `source_uri` (`internal/ingest/rss.HostFromFeedURL`, already
  implemented for ADR-0008) as call arguments, so per-feed rules are
  expressed as ordinary conditionals inside the one script rather than
  needing a second per-feed config primitive incompatible with
  `RSS_FEED_URLS`' single-line comma-separated shape.
- **A file path, not inline script text in an env var.** This matches
  existing path-shaped config precedent (`DB_PATH`,
  `IMAP_MAILFETCH_SOCKET`); a multi-line script cannot reasonably live in
  a single `.env` line.
- **Bootstrap-only — no live DB-overlay reload for v1.** Unlike
  `RSS_FEED_URLS`/`RSS_POLL_INTERVAL` (ADR-0006 Tier A),
  `RSS_FILTER_SCRIPT_PATH` is not added to `internal/config/keyclass.go`'s
  `dbEligibleKeys`. It is loaded and compiled exactly once, at
  `cmd/server` startup; editing the script requires a restart. A script
  is executable logic, not a scalar — live-reloading it would require
  either re-parsing the file on every `Fetch` call with a defined
  fallback for a script broken mid-edit (keep serving the last-known-good
  compiled program, discard the broken parse, log a warning) or a
  `configstore`-routed reload hook. Both are legitimate follow-up work,
  but add real complexity (partial-failure/fallback state) this
  already-sizable change (new dependency, new config key, new adapter
  behavior) does not need to ship with. This is a deliberate, named
  non-goal, not an oversight.
- **Ingest-time only — no retroactive re-filtering.** Filtering only ever
  prevents promotion of a not-yet-fetched item; it never suppresses an
  already-created `domain.Entry`. This codebase's append-only-history
  convention (ADR-0004's note-hide-not-delete precedent for the one place
  a note *can* be suppressed post-hoc) has no existing general-purpose
  "retroactively suppress an already-ingested external entry" mechanism —
  building one would be a materially larger, separate feature
  (auto-moderation of already-published content), not a natural
  extension of "don't ingest this." Dedupe (`external_items.dedupe_key`)
  already means a filtered-and-later-unfiltered item is not automatically
  recovered either, once its feed's ETag/cursor moves past it. An owner
  wanting to remove specific already-ingested articles uses the existing
  per-entry `notes/delete` (ADR-0004's hide mechanism) — no new
  bulk-suppression tool is introduced.
- **Hook point: `internal/ingest/rss.Adapter.Fetch`, immediately after
  `parseFeed`, not the generic `internal/ingest.Service`/`Adapter`
  interface.** `parseFeed` already produces `[]ingest.FetchedItem` with
  `Title`/`Body` HTML-stripped and length-bounded — the same plain text
  that ends up in `Entry.Body` — so filtering in place there, before
  `FetchResult{Items: items, ...}` is returned, is the single natural
  insertion point. `NextCursor` (ETag/Last-Modified, derived from HTTP
  response headers) is entirely independent of item content, so
  filtering some items out never disturbs cursor advancement: a filtered
  item is simply never seen again once the feed's ETag moves past it,
  the same "silently skip, not replay later" shape `RSS_ENABLED=false`
  already has. Scoping this to `internal/ingest/rss` keeps the new
  Starlark dependency contained to the one adapter package that needs it
  (`internal/ingest/imap` is untouched — Issue #135's own scope is
  RSS-only), and requires zero changes to `ingest.Adapter`'s interface,
  `ingest.Service.Handle`, or `internal/timeline`.
- **Contract**: the script must define
  `def matches(title, body, source_host, source_uri, provenance_url): ...`
  returning a `bool` — `True` excludes the item. A script that fails to
  load (missing file, syntax error, missing/mis-shaped `matches()`) fails
  server startup closed, before the server ever binds a port — the same
  posture every other startup-time config problem in `cmd/server`
  already gets. A per-item runtime evaluation error (an execution-step
  bound exceeded, an unanticipated type error triggered by untrusted feed
  content, a non-bool return, ...) is logged at `Warn` and **fails
  open** — the item is kept, never silently dropped by a filter
  *infrastructure* failure as opposed to a deliberate filter *decision*.
  This mirrors AGENTS.md's "an integration failure must never make a
  user post disappear" principle applied to this filter's own failure
  mode: a transient/pathological evaluation failure is recoverable (the
  owner can hide the article afterward via `notes/delete` if the script
  really would have excluded it), while wrongly dropping a legitimate
  article on a script hiccup is not (the feed's cursor moves on and the
  item is never re-offered).
- **Sandboxing.** `LoadFilter` configures no `Load` callback on the
  `starlark.Thread` it uses, so a script containing a `load(...)`
  statement fails outright ("load not implemented by this application")
  — a script can never pull in code from another file or module. Only
  the language's own predeclared builtins (`len`, `range`, `str`, list/
  string methods, `fail`, ...) are available; there is no `open`, `exec`,
  `os`, or `http` builtin, so referencing one is an undefined name,
  caught by the static resolver before the script ever runs. Each
  `matches()` call bounds Starlark execution via
  `starlark.Thread.SetMaxExecutionSteps` (`filterMaxSteps`,
  `internal/ingest/rss/filter.go`), on a **fresh `*starlark.Thread`
  constructed for that one call**: go.starlark.net's `Thread.Steps`
  counter accumulates across every `Call` on the same `Thread` rather
  than resetting per call, and a `Thread` is not safe for concurrent use.
  `internal/jobs.Manager` can run several `external_source_poll` jobs
  concurrently (`JOBS_MAX_CONCURRENT`), each potentially calling
  `Adapter.Fetch` — and therefore `Filter.shouldExclude` — on the same
  shared `*Filter`; reusing one `Thread` would both race and, after
  enough cumulative execution across every item ever evaluated, start
  failing every subsequent item permanently once the running total
  crossed `filterMaxSteps`, regardless of any single item's actual cost.
  The compiled `matches` function value itself is immutable and safe to
  call from many `Thread`s concurrently. go.starlark.net's default
  dialect (used unmodified) also disallows `while` statements and
  recursive function calls, so a script has no way to loop forever on
  its own — the step bound exists only to catch a bounded-but-huge loop
  (for example `for i in range(10**18):`) before it runs for an
  unreasonable amount of wall-clock time.

## Consequences

- New dependency: `go.starlark.net` (pure Go, no cgo — verified with
  `CGO_ENABLED=0 GOOS=linux go build ./...`, matching `Dockerfile`'s
  static-binary build; BSD-3-Clause licensed; the reference
  implementation the issue itself names, Bazel's own interpreter; makes
  no network calls of its own, so it needs no SSRF-allowlist review).
- `internal/config` gains `RSS_FILTER_SCRIPT_PATH` as a plain optional
  string with no deep validation — real validation (parse + shape check)
  happens in `cmd/server` via `internal/ingest/rss.LoadFilter` at
  startup, fail-closed.
- `internal/ingest/rss.Adapter` gains a `*Filter` field/constructor
  parameter (`NewAdapter`'s signature changes to take `filter *Filter,
  logger *slog.Logger`); `nil` preserves exactly today's unfiltered
  behavior, and every pre-#135 `NewAdapter` call site is updated to pass
  `nil, nil`.

## Revisit if

Per-feed filter rules ever need to be independently toggled/edited
without touching a shared script (for example a UI for managing filters
per feed) — that would need a real per-feed config primitive this
decision deliberately avoided building. Also revisit the "no live
reload" choice if editing the script without a restart becomes a
frequent operational need.
