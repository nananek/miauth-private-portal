# ADR-0004: Map `/api/notes/delete` onto SetHidden, not a new hard delete

- Status: Accepted for Issue #23 PR3
- Date: 2026-09-06
- Scope: Issue #23 PR3 (`POST /api/notes/delete`)

## Context

Issue #23 asks for Aria's note delete action (note footer / note action
sheet / post-edit dialog's delete option) to work against this service.
The pinned Aria/`misskey_dart` trace
(`docs/compat/aria-v1.5.11.md`'s `POST /api/notes/delete` section) confirms
Aria calls this endpoint with only `{noteId, i}` and never decodes a typed
response, so the wire contract itself does not constrain the choice below.

The domain layer currently has two independent soft-delete primitives,
`Entry.ArchivedAt` and `Entry.HiddenAt` (`internal/domain/entry.go`,
`internal/timeline.Service.SetArchived`/`SetHidden`), but no true hard
delete. Before this issue, neither primitive was reachable from any HTTP
endpoint — `SetArchived`/`SetHidden` were only ever called from
`internal/timeline/service.go` itself (its own tests), so this PR is the
first to wire either one to a Misskey-compatible wire API, and is
therefore also the first to decide what either state actually means at
the product level.

Two options were considered, as the Issue #23 text itself framed them:

1. **Add a true hard delete** to the domain/storage layer: a new
   `Entries.Delete` primitive that actually removes (or irreversibly
   tombstones) the row, updating `EntryRepository.CountByAuthor`'s doc
   comment (which explicitly said hard delete "this service does not
   support") and its semantics accordingly.
2. **Map delete onto the existing `SetHidden` primitive**: `POST
   /api/notes/delete` becomes sugar for
   `timeline.Service.SetHidden(id, true)`, restricted to the owner's own
   `user_post` entries.

Real Misskey's `notes/delete` is a true hard delete and does reduce
`notesCount`. Mapping onto hide is therefore not perfectly wire-compatible
with real Misskey at the semantic level (a hidden note could, in
principle, be un-hidden later, which real deleted notes cannot), even
though Aria itself cannot observe the difference (it never re-queries a
deleted note's ID and only removes it from its local cache).

## Decision

Map `POST /api/notes/delete` onto `timeline.Service.SetHidden(id, true)`.
No hard delete is added.

- **Product-concept alignment**: Issue #1's goal states user posts should
  be preserved as a learning log ("user post を最優先で保存"). A
  reversible, non-destructive delete is a better fit for that goal than an
  irreversible one, and this service has no separate "trash"/undo
  mechanism that a hard delete would otherwise need to be paired with to
  avoid one honest mis-click permanently destroying a post.
- **`hidden`, not `archived`, is the delete mapping.** Both primitives are
  otherwise still unconnected to any first-class product meaning; this PR
  is what assigns one to `hidden` for the first time. "Hidden" reads
  closer to Misskey's delete semantics (no longer visible to anyone,
  whether or not it is recoverable), while "archived" is left free for a
  future, owner-facing organizational concept (e.g., a "read later" shelf)
  that is unrelated to deletion. Nothing in this PR prevents that future
  use; it only avoids conflating the two now that one of them has been
  given a concrete meaning.
- **Scope of what becomes deletable**: only entries the owner authored as
  `user_post` (`entry.Kind == domain.EntryUserPost`, mirroring
  `EditPost`'s existing author/kind restriction in
  `internal/timeline/service.go`). LLM-generated replies/follow-ups and
  RSS/mail-ingested entries are not deletable through this endpoint, the
  same way they are not editable through the (unwired) `EditPost` — a
  hard requirement of Misskey's own `notes/delete`, which only ever
  operates on the caller's own notes.
- **Children survive.** A deleted note's own children keep their own
  `HiddenAt == nil` and remain reachable from the home timeline and by
  direct ID; only the parent becomes invisible. This mirrors real
  Misskey's "delete the parent, orphan the children" behavior and requires
  no new code: `entryVisible` already governs each entry independently.
- **`CountByAuthor` changes meaning.** Before this issue, `notesCount`
  counted every authored entry unconditionally, since nothing could
  reduce it. This PR adds `AND archived_at IS NULL AND hidden_at IS NULL`
  to `EntryRepository.CountByAuthor`'s query (matching the existing
  partial index `idx_entries_timeline_default`, so no new index is
  needed), so that a delete now visibly decrements `/api/i`'s
  `notesCount`, matching what Aria's UI expects after a successful
  delete. `EntryRepository.CountAll` (Issue #23 PR2's `/api/stats`
  server-wide count) is a separate method and keeps its
  include-everything semantics; this decision does not touch it.
- **Repeat/invalid delete is indistinguishable from not-found.** An
  unknown note ID, an already hidden/archived note, and a note that
  exists but is not the owner's own `user_post` all produce the same
  `NO_SUCH_NOTE` response, matching this codebase's existing uniform
  denial pattern for note-reading endpoints (`writeNoSuchNote`'s doc
  comment on not distinguishing "does not exist" from "you may not see
  it").

## Consequences

- No migration is needed: this PR reuses the existing `hidden_at` column
  from `internal/storage/sqlite/migrations/0004_threads_entries.sql`.
- `hidden` now has a first product-level meaning ("deleted, Misskey-style"),
  so any future feature that wants a different "temporarily hide but not
  delete" concept must not reuse `SetHidden`/`HiddenAt` — it should use
  `archived` (still unassigned) or a new primitive.
- A "deleted" note is not actually gone: it remains in SQLite, still
  counted by `/api/stats`' server-wide `CountAll`, and is recoverable in
  principle (no HTTP endpoint currently exposes an "undelete", but the
  underlying `SetHidden(id, false)` call already exists). This is treated
  as consistent with, not a violation of, Issue #1's "posts are preserved"
  goal, and should be kept in mind if a future issue ever wants a genuine,
  irreversible hard delete (e.g., for handling accidentally posted
  secrets) — that would need a new, separate primitive rather than a
  reinterpretation of `hidden`.
- Real Misskey's `notesCount` is a true post-delete count; this service's
  is a hide-aware count that can, in principle, go back up if a note is
  ever un-hidden through a future feature. Aria cannot observe this
  difference today (it has no un-hide UI pointed at this service), but it
  is a documented semantic gap from real Misskey, not a bug.
