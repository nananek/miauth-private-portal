# ADR-0008: RSS external-source identity — real host, design A

- Status: Accepted for Issue #77 PR4
- Date: 2026-09-08
- Scope: Issue #77 PR4 (`ActorExternalSource`, `internal/ingest/rss`'s
  host/username derivation, `internal/storage/sqlite`'s actors/
  external_sources schema changes, `resolveUserLite`'s new
  `ExternalSourceResolver` case). Does not cover IMAP-kind sources
  (deliberately excluded — see Decision) or post attachments/favicon
  storage (PR1/PR6, unrelated).

## Context

Issue #77's requirement 2 ("外部ソース/RSSのアイコンと帰属") asks that
RSS-authored notes stop projecting as the single shared `system` actor
and instead carry their own per-feed identity, the way Issue #52 already
gives each Open WebUI model its own `VirtualActor`. During PR4 planning
the owner additionally asked (2026-09-08, plan-77 §2.4's own record of
the request, quoted there): "RSSの実装なんですが、これも配信元のドメイン
を仮想連合先に見せて、ユーザー名を登録時に任意に設定できるようにしたい
ですね" — i.e., that the projected `host` be the feed's own real origin
domain, not a synthetic deployment-wide value, and that the username be
settable per feed.

This raised a design question Issue #52's VirtualActor precedent does
not answer by itself: `OPENWEBUI_PRESENTATION_HOST` is one fixed,
deployment-chosen synthetic hostname every VirtualActor shares (a
Tailscale-style name no one could mistake for a real third-party
service). A per-feed *real* domain is different in kind: `note.com`,
`zenn.dev`, and similar are real services with real user bases, and
`@someusername@note.com` is visually indistinguishable from an actual
federated account on that real service.

## Decision

**Design A: `ActorExternalSource.` host is the feed's own real origin
domain, computed once at registration.**

- `domain.ActorType` gains `ActorExternalSource`, following Issue #52's
  "1 external identity = 1 actor row" pattern: one dedicated actor per
  RSS-kind `domain.ExternalSource`, never a singleton, never accepted by
  MiAuth. `Actor.IsRemote()` includes it.
- `ExternalSource` gains `ActorID`, `Username`, and `Host`, all set
  together exactly once, when the source is first registered
  (`cmd/server`'s `ensureRSSSourcesWithActors`), and never recomputed —
  editing `RSS_FEED_URLS`' host after the fact does not retroactively
  change an already-registered source's projected host; registering
  under a new URL creates a new source instead.
- `Host` = `net/url.Parse(uri).Hostname()` — the feed's real,
  operator-uncontrolled origin.
- `Username` is Misskey-username-charset-validated
  (`internal/config`'s `ownerUsernamePattern`), owner-settable via a
  `RSS_FEED_URLS` entry's optional `|username` suffix, or derived from
  `Host` (`internal/ingest/rss.DefaultUsername`) when left unset, with a
  hash-suffix disambiguation identical in shape to Issue #75's
  `GenerateActorSlug` when two feeds would otherwise collide on the same
  `(host, username)` pair (a `UNIQUE(host, username)` partial index
  enforces this can never actually happen at the database level).
- **Deliberately excluded: IMAP.** `ActorExternalSource`/`Host`/
  `Username` apply only to `Kind == "rss"` sources. An imap-kind source
  keeps projecting as the shared `system` actor, completely unchanged.
  This is not a deferral — it is a considered exclusion (see
  "Considered and accepted" below).

### Why a real domain is safe here specifically

This service does not federate with anything (AGENTS.md: "no full
Misskey compatibility or federation"; README.md's Known limitations).
`host` here is never used to discover, resolve, dial, or deliver to a
remote server, verify an ActivityPub signature, or participate in any
inter-server protocol — it is a display-only string Aria's `UserLite`
renders next to a username, the identical mechanism Issue #52's
`VirtualActor` already uses for a synthetic host. The owner's own
reasoning for accepting this (quoted verbatim, 2026-09-08): 「実際の連合
をする予定がないので(なりすまし等の)問題もない」 — there is no
federation, so there is no peer server for a real domain to be confused
with, and no delivery path a spoofed identity could exploit.

### Considered and accepted, not retracted: the impersonation-adjacent
concerns

Before this decision, the following risks were raised and are recorded
here as accepted trade-offs, not oversights:

1. **A real host visually resembles a federated account.**
   `@someusername@note.com` looks identical to an actual note.com user
   to anyone reading Aria's UI, a screenshot, or a shared link. Accepted
   because no protocol-level consequence follows from that resemblance
   in a non-federating deployment — it is a cosmetic risk, not a
   security boundary this service actually depends on elsewhere.
2. **Two feeds on the same host present as two distinct "accounts."**
   Registering two different note.com RSS feeds under different
   usernames produces `@blogA@note.com` and `@blogB@note.com` — visually
   two different note.com users, though note.com itself has no
   involvement in either. Accepted: this is the intended and only way to
   tell two feeds from the same site apart in Aria's UI, and matches how
   a real federated Misskey instance would in fact show two different
   accounts from the same remote server.
3. **IMAP was considered and rejected for the same treatment**, not
   merely left for later. A spoofed email-sender-domain-as-host would
   reuse a real, well-known phishing technique (a forged `From:` domain)
   in a way RSS's "publicly syndicated content" framing does not share —
   an RSS feed's URL is the operator's own deliberate subscription
   choice, where a mail message's sender domain is attacker-influenced
   content arriving over a channel this service explicitly does not
   trust (`AGENTS.md`: treat mail as untrusted; IMAP is read-only by
   design). This asymmetry is why "no federation, so no harm" does not
   extend to IMAP by the same reasoning, and PR4 does not implement it
   there.

## Consequences

- `internal/storage/sqlite/migrations/0027_actors_external_source_type.sql`
  rebuilds `actors` (SQLite cannot alter a `CHECK` list in place — the
  same shape migration 0016 already used for `openwebui_model`).
  `0028_external_sources_identity.sql` adds `actor_id`/`username`/`host`
  plus a `UNIQUE(host, username)` partial index.
  `0029_entries_provenance_url.sql` denormalizes each ingested entry's
  source-item URL onto `entries` (Misskey's `Note.url`, projected by
  `internal/httpserver`).
- `internal/ingest/rss`'s host/username derivation
  (`HostFromFeedURL`/`DefaultUsername`) deliberately duplicates the
  shape of Issue #75's `internal/openwebui.GenerateActorSlug`
  (normalize → hash fallback → hash-suffix disambiguation) rather than
  importing it: same algorithm shape, kept in a separate package so RSS
  ingestion never depends on the unrelated Open WebUI feature
  (`AGENTS.md`'s narrow use-case package boundaries).
- `resolveUserLite` (`internal/httpserver/noteapi_wire.go`) gains an
  `ExternalSourceResolver` case, structurally satisfied by
  `domain.ExternalSourceRepository` itself (`cmd/server` passes
  `db.Repos.ExternalSources` directly) — no new registry/service
  package, unlike Issue #52's `openwebui.Registry`, since resolving an
  actor to its owning row needs no business logic beyond a lookup.

## Revisit if

**If this service ever adds real ActivityPub federation** (actual
inter-server delivery, signature verification, or follow/discovery with
any other Misskey/Mastodon-shaped server), **this decision must be
revisited before that work ships.** The reasoning above depends entirely
on there being no peer server for a real per-feed domain to be confused
with; federation introduces exactly that peer relationship, and an
`ActorExternalSource` presenting a real third-party domain would then
carry a genuine impersonation/trust risk no longer offset by "nothing
downstream ever treats this host as real." At that point, this decision
should be re-examined against whatever federation design is actually
proposed — likely candidates include reverting to a synthetic host
(Issue #52's own pattern) for `ActorExternalSource` specifically, or
gating real-host display behind an explicit, separately-reviewed opt-in.
