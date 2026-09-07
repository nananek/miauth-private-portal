# Drive-backed media storage, Misskey Drive API, and attachments roadmap

- Status: **every PR0–PR7 complete — Issue #77's full scope has
  shipped.** PR4 (RSS/external-source icons and attribution)'s
  identity/host mechanism (ADR-0008) was done in its own commit; favicon
  fetching/storage — left open at the end of PR4 as an explicit owner
  decision — was folded into PR5 rather than a PR4 follow-up, since both
  need the same Drive-backed image storage infrastructure (owner
  decision, 2026-09-08).
- Tracker issue: [Issue #77](https://github.com/nananek/miauth-private-portal/issues/77)
  — "Add app/source icons, RSS attribution, profile images, and
  Drive-backed media storage" (P1)
- Plan record: ccserver document board key `plan-77` (v2, scope-confirmed
  revision, workerC, 2026-09-08)
- Target contract: [`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md)'s
  "Drive API and note attachments" section (added by PR0)
- Scope: single-owner `miauth-private-portal` service; this is not a
  general-purpose multi-user Misskey Drive implementation

## Goal

Add real avatar/icon/media storage where today there is none — no
`Actor`/`VirtualActor`/wire-projected user carries an avatar field, no
static file route exists, and RSS/mail-authored notes always project a
fixed `ActorSystem` regardless of the originating feed or sender. Four
scope points were confirmed by the owner beyond the original investigation
(v1 → v2 revision):

1. Implement the Misskey-compatible Drive API itself (file list/upload/
   show/update/delete/folders), not just a CLI-managed avatar path — Aria's
   drive screen is a real, actively used surface (confirmed by PR0; see
   the compat doc section above).
2. Include generic post-attachment upload (`notes/create`'s `fileIds`), not
   only avatars/icons.
3. Reject SVG/vector images — raster only.
4. Implement an S3-compatible storage backend in addition to local disk;
   the storage abstraction is not local-disk-only.

## Roadmap placement

This is a standalone P1 feature track under Issue #77, independent of the
Issue #75 Open WebUI multi-model work and the Issue #83 timeline-ordering
fix. It does not block or get blocked by either.

Dependency graph:

```text
PR0 (Aria/misskey_dart Drive contract investigation, done)
  -> PR3 (Misskey-compatible Drive API, done)
  -> PR6 (post attachments)

PR1 (Drive foundation: Storage abstraction, local disk + S3-compatible,
     files table, image validation)
  -> PR3, PR4, PR5, PR6

PR2 (static app/source icons, done) — independent, no dependency on PR1/PR0

PR7 (operational hardening: orphan GC, backup/restore docs, storage
     failure tests, image-validation tests) — last, depends on the whole
     stack above
```

PR1 and PR2 do not depend on PR0 and could in principle proceed in
parallel with it; this repository's implementation order ran them
sequentially (PR0 then PR1) since Issue #77's assigned worker handled both.

## PR0: Aria/misskey_dart Drive and attachment contract investigation — done

**Status: complete.** No `internal/` Go code changed; this was a
documentation-only investigation PR, the same shape as Issue #72's Open
WebUI tool-call feasibility investigation.

Traced the pinned Aria snapshot
([`a66c9303`](https://github.com/poppingmoon/aria/tree/a66c9303995e7c964765cf382de6a9b0e3f4a3b6))
and its pinned `misskey_dart` dependency
([`14176c515`](https://github.com/poppingmoon/misskey_dart/tree/14176c515a005a9fb01d3e6365a49b5a5d387a92))
— the same versions `docs/compat/aria-v1.5.11.md` already pins — to confirm
or correct plan-77 v2's §2.1–§2.5 hypotheses before PR3/PR5/PR6 implement
against them. Full findings are recorded in
[`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md)'s "Drive API and
note attachments" section and its updated allowlist table; summary of what
changed from the pre-investigation plan:

- **Folder support is not optional-to-skip.** Aria's drive screen actively
  uses folder listing, creation, deletion, rename, and move. Plan-77 v2
  §2.1's "minimal implementation or unsupported" framing for folders is
  narrower than what a smooth Aria drive-screen experience needs; PR3 must
  decide explicitly whether to implement `drive/folders/*` for real or
  accept that folder actions will visibly fail in Aria's UI.
- **A drive file can be attached to more than one note.** This corrects
  plan-77 v2 §2.3's tentative "1:1 might be enough" framing for
  `entry_files`: `drive/files/attached-notes` exists specifically to answer
  a one-to-many "which notes reference this file" query, and Aria's post
  composer can select an already-uploaded drive file into a new post
  without re-uploading it. PR6's `entry_files` must be a genuine
  many-to-many join, and PR6/PR7 must define the ownership/GC rule for a
  `files` row still referenced by `entry_files` when a note is deleted (and
  vice versa).
- **`drive/files/move-bulk` never needs implementing.** Aria probes
  `POST /api/endpoints` first and transparently falls back to per-file
  moves when the name is absent from that list — simply never advertising
  it there is suffient, not a compatibility gap.
- **Five Drive endpoints are confirmed unused by Aria** and should stay
  unimplemented (`UNSUPPORTED_FEATURE`, not silently invented): `drive/
  stream`, `drive/files/find`, `drive/files/check-existence`, `drive/
  files/find-by-hash`, `drive/folders/find`.
- **`DriveFile` has no discriminator-key trap** (unlike `User`/`avatarId`
  traced for Issue #65) — every field decodes structurally. `url` is
  required, `thumbnailUrl` is optional.
- **The avatar upload sequence is confirmed**: `drive/files/create` (or
  `createAsBinary`) followed by `POST /api/i/update {avatarId: "<fileId>"}`,
  with an explicit `avatarId: null` sent (not omitted) to clear the avatar
  — PR5 must accept that explicit null as "remove avatar."
- **`notes/create`'s `fileIds` omits the key entirely when empty** (never
  an explicit `[]`), and is ordered by the composer's own
  user-reorderable attachment order — `entry_files.position` must preserve
  that order.
- **`POST /api/notes/timeline`'s `withFiles` filter is already accepted
  and currently ignored** (`internal/httpserver/noteapi_handlers.go`).
  PR6 left it ignored even once `note.files` became real data; whether to
  start honoring it is a PR7 decision, out of PR0's scope.

## PR1: Drive foundation (`internal/drive`)

**Status: complete.** No HTTP surface, no repository against the `files`
table — unit tests only. This is the foundation PR3/PR4/PR5/PR6 build on.

- `Storage` interface (`internal/drive/storage.go`: `Put`/`Get`/`Delete`,
  create-only `Put`, idempotent `Delete`) with two implementations,
  selected once per deployment via `DRIVE_BACKEND` (not per-file/
  per-request): `Local` (`internal/drive/localdisk.go`, writes under
  `DRIVE_DATA_DIR`, keys checked with `filepath.IsLocal` and rejected
  rather than silently renormalized) and `S3` (`internal/drive/s3.go`,
  via `minio-go` — see ADR-0007 D4 for why not the full AWS SDK for Go
  v2). Both share one contract test suite
  (`internal/drive/storage_contract_test.go`); `S3`'s own tests run
  against an in-process fake S3-compatible `httptest.Server`
  (`internal/drive/s3_test.go`), not a real MinIO/S3 endpoint.
- `files` table (migration `0024_files.sql`): `id`, `owner_actor_id`
  (nullable), `purpose` (closed enum: `avatar`/`source_favicon`/
  `attachment`/`app_icon`), `mime`, `byte_size`, `sha256`, `storage_key`
  (unique), `width`/`height` (nullable), `created_at`. No repository
  reads or writes it yet — see ADR-0007 D6 for why that is deferred to
  PR3/PR4/PR5/PR6, the same precedent migration `0017` (OWUI-C) set.
- Image validation (`internal/drive/validate.go`'s `ValidateImage`):
  decodes against an allowlist (`png`, `jpeg`, `webp`) with Go's
  `image.DecodeConfig`, ignoring any client-declared `Content-Type`.
  SVG is rejected by the same fails-to-decode path (ADR-0007 D2) — no
  dedicated SVG/XML detector exists. Width/height ceilings
  (`DRIVE_MAX_IMAGE_WIDTH`/`DRIVE_MAX_IMAGE_HEIGHT`) are checked against
  the decoded header, independent of `DRIVE_MAX_FILE_BYTES`.
- S3 credentials (`DRIVE_S3_ACCESS_KEY_ID`/`DRIVE_S3_SECRET_ACCESS_KEY`)
  are plain configured values in this PR, not a `secret_ref` indirection
  — see ADR-0007 D5 for why `internal/openwebui/registry.go`'s pattern
  does not apply here yet (no Drive configuration is persisted to any
  database row for a `secret_ref` to name).
- New configuration keys (`internal/config.DriveConfig`, no
  `DRIVE_ENABLED` flag — see ADR-0007's "Consequences"):
  `DRIVE_BACKEND` (default `localdisk`), `DRIVE_DATA_DIR` (default
  `./data/drive`), `DRIVE_S3_ENDPOINT`/`DRIVE_S3_BUCKET`/
  `DRIVE_S3_ACCESS_KEY_ID`/`DRIVE_S3_SECRET_ACCESS_KEY` (required when
  `DRIVE_BACKEND=s3compat`), `DRIVE_S3_USE_SSL` (default `true`),
  `DRIVE_S3_REGION`, `DRIVE_MAX_FILE_BYTES` (default 10 MiB),
  `DRIVE_MAX_IMAGE_WIDTH`/`DRIVE_MAX_IMAGE_HEIGHT` (default 8000).
  Documented in `docs/operations/configuration.md`'s "Known
  configuration keys" table and "Drive storage foundation" section.
- New ADR: [`docs/decisions/0007-drive-storage-boundary.md`](../decisions/0007-drive-storage-boundary.md).
- New dependencies: `github.com/minio/minio-go/v7` (S3-compatible
  client, ADR-0007 D4) and `golang.org/x/image` (WebP decode support,
  ADR-0007 D2) — both standard-library-adjacent or narrowly scoped to
  the concrete need above, per AGENTS.md's "new dependencies require a
  concrete reason."

## PR2: Portal/app static icons

**Status: complete.** Fixed favicon/OGP/PWA icon set served via
`embed.FS`. Independent of Drive; ran after PR1 in this repository's
implementation order but has no dependency on it.

- This repository has no real logo or brand asset and no HTML page of
  its own (AGENTS.md: do not add a custom web UI), so the icon set is a
  code-generated "monogram" placeholder — a solid-color square with a
  white "M" — rather than hand-designed art or a page with `<link>`
  tags: see
  [`internal/httpserver/staticassets/doc.go`](../../internal/httpserver/staticassets/doc.go)
  and [`gen/main.go`](../../internal/httpserver/staticassets/gen/main.go)'s
  doc comments for the exact design and why it is meant to be replaced
  by real design work later. `gen/main.go` uses
  `golang.org/x/image/font/{opentype,gofont/goregular}` — already a
  dependency since PR1 added `golang.org/x/image` for WebP decoding, so
  this needed no new one — to render the letter, and a small in-package
  ICO encoder (the standard "PNG-in-ICO" container; Go's standard
  library has no ICO encoder) to build `favicon.ico`. Regenerate with
  `go generate ./internal/httpserver/staticassets/...`; the output is
  deterministic (no timestamps, no randomness) and committed as static
  files, not generated at server startup.
- `internal/httpserver/staticicons.go` embeds the generated files and
  serves each at its conventional well-known path unconditionally (like
  `GET /healthz`, no configuration or dependency): `GET /favicon.ico`,
  `/favicon-{16x16,32x32}.png`, `/favicon-{16x16,32x32}-dark.png`,
  `/apple-touch-icon.png`, `/icon-{192,512}.png`, `/og-image.png`,
  `/site.webmanifest`. This satisfies Issue #77's "新規インストール直後
  でもfavicon・アプリアイコンが表示される" acceptance criterion for a
  browser tab pointed at this origin — for example during the MiAuth
  flow's plain-text page (`miauth_handlers.go`'s `writePlainTextPage`) —
  which auto-requests `/favicon.ico`/`/apple-touch-icon.png` with no
  markup needed.
- The `*-dark.png` variants and `/og-image.png`/`/site.webmanifest` are
  served now but have no consumer yet: dark-mode favicon switching and
  Open Graph meta tags both require an HTML `<head>` to put a `<link>`/
  `<meta>` tag on, and this repository has none. They exist at fixed
  paths so a future page (if one is ever added) can reference them
  without a further asset change.

## PR3: Misskey-compatible Drive API

**Status: complete.** Depended on PR0 (this document) and PR1.

- `internal/drive.Service` (`internal/drive/service.go`) is the new
  business-logic layer this PR adds on top of PR1's `Storage`/
  `ValidateImage`: ownership checks, folder containment, partial-update
  semantics (an explicit `null` on `comment`/`folderId` clears the
  field, matching Aria's own convention — see PR0's findings above), and
  a folder-move cycle check. It depends on two new domain repositories
  (`domain.FileRepository`, `domain.FolderRepository`,
  `internal/domain/file.go`) backed by `internal/storage/sqlite`'s new
  `file_repository.go`/`folder_repository.go`, and on
  `internal/ingest/safehttp` for `upload-from-url`'s SSRF-protected
  fetch.
- Two new migrations: `0025_drive_folders.sql` (the `folders` table —
  PR0's trace found folder support is not optional to skip) and
  `0026_drive_files_metadata.sql` (adds `name`/`comment`/`is_sensitive`/
  `folder_id`/`md5` to the `files` table PR1 deliberately left bare —
  ADR-0007 D6's "repository ships when a concrete use case exists" now
  applies to these columns too).
- `POST /api/drive`, `/files`, `/files/create`, `/files/show`,
  `/files/update`, `/files/delete`, `/files/upload-from-url`,
  `/files/attached-notes`, `/folders`, `/folders/create`,
  `/folders/delete`, `/folders/update`, `/folders/show` are all
  implemented (`internal/httpserver/drive_handlers.go`); `stream`,
  `files/find`, `files/check-existence`, `files/find-by-hash`,
  `folders/find`, and `files/move-bulk` are deliberately not — see PR0's
  findings above. `files/create` authenticates itself directly against
  `s.miauth` rather than going through `RequireScope`, since Aria sends
  its local API token as a multipart form field for this one endpoint,
  not the JSON body every other route reads it from.
- `GET /files/{id}` (unauthenticated, `internal/httpserver/
  drive_handlers.go`'s `handleFilesShow`) is the new byte-serving route
  `DriveFile.url` resolves to — the file id's own 128 bits of
  `crypto/rand` unguessability is the access control, matching how
  Misskey/Mastodon-shaped services already serve media in production.
- New `ScopeReadDrive`/`ScopeWriteDrive` added to
  `internal/miauth/scope.go`'s `grantableScopes`. Aria's MiAuth
  `permission` query already requested `read:drive,write:drive` before
  this PR shipped, so an API token issued before it will need
  re-authorization through `miauthctl` to gain these scopes (same
  situation this repository already has for `read:notifications`/
  Issue #23 PR6).
- New `driveFile`/`driveFolder`/`driveResponse` wire types
  (`internal/httpserver/drive_wire.go`), matching PR0's traced required/
  nullable field split; `internal/httpserver/drive_handlers.go`'s Drive
  error ids/codes (`NO_SUCH_FILE`, `FOLDER_NOT_EMPTY`, ...) follow this
  service's own established kebab-case-id/SCREAMING_SNAKE-code
  convention, not real Misskey's error UUIDs.
- Formal contract write-up added to `docs/compat/aria-v1.5.11.md`'s
  "Drive API and note attachments" section (PR0's investigation findings
  promoted from "planned" to "implemented," plus a new "PR3
  implementation notes" subsection recording decisions PR0 did not
  already settle: raster-only for every upload, `force` accepted and
  ignored, `md5` as a real digest, synchronous `upload-from-url`,
  `attached-notes` always empty until PR6, and `POST /api/drive`'s
  `capacity` being a cosmetic fixed constant).
- README.md's "Known limitations" no longer lists Drive/files as an
  unimplemented Misskey feature (it also no longer lists reactions/
  notifications, both already inaccurate before this PR — see plan-77
  §4).

## PR4: RSS/external-source icons and attribution

**Status: complete.** Depended on PR1. The identity/attribution half of
this PR (plan-77 §2.4, ADR-0008's design-A decision) landed in PR4's own
commit; favicon fetching/storage — left open at the end of PR4, see
"Deferred to PR5" below — landed as part of PR5 instead (owner decision,
2026-09-08: favicon fetch and `actors.avatar_file_id` share the same
Drive-backed image storage infrastructure, so bundling them was judged
more coherent than a separate PR4-follow-up commit).

Done:

- `domain.ActorExternalSource` (Issue #52's "1 external identity = 1
  actor row" pattern, applied to RSS-kind `domain.ExternalSource`), a
  rebuild migration (`0027_actors_external_source_type.sql`, mirroring
  migration 0016's shape) and two plain-ADD-COLUMN migrations
  (`0028_external_sources_identity.sql`: `actor_id`/`username`/`host`
  plus a `UNIQUE(host, username)` partial index;
  `0029_entries_provenance_url.sql`: denormalizes each ingested entry's
  source-item URL onto `entries`).
- `internal/ingest/rss`'s `HostFromFeedURL`/`DefaultUsername` (host
  parsing plus a Issue-75-`GenerateActorSlug`-shaped, deliberately not
  shared, normalize/hash-fallback/disambiguate derivation) and
  `cmd/server`'s `ensureRSSSourcesWithActors`, which replaces
  `EnsureFromConfig` for RSS specifically: it creates each genuinely new
  source's actor and source row together, atomically, so neither is ever
  left orphaned.
- `RSS_FEED_URLS`' new optional `|<username>` per-entry suffix
  (`internal/config`), letting the owner set a feed's username at
  registration; unset falls back to a host-derived default.
- `resolveUserLite`'s new `ExternalSourceResolver` case
  (`internal/httpserver/noteapi_wire.go`) and `note.url`'s projection
  from `domain.Entry.ProvenanceURL` (`timeline.Service.
  CreateExternalEntry` now denormalizes it from
  `domain.ExternalItem.ProvenanceURL` at creation time).
- `docs/compat/aria-v1.5.11.md`'s "Note.text provenance markers" section
  now documents the real `user.host`/`note.url` signals PR4 adds
  alongside the pre-existing text markers (which are unchanged).
- New ADR: [`docs/decisions/0008-external-source-identity.md`](../decisions/0008-external-source-identity.md)
  — the design-A decision, the considered-and-accepted impersonation-
  adjacent concerns, why IMAP is deliberately excluded, and the
  "Revisit if real federation ships" condition plan-77 §2.4.7 required.
- **Deliberately excluded from this PR (ADR-0008's own scope note):**
  IMAP-kind sources keep projecting as the shared `system` actor,
  unchanged.

Deferred to PR5 (see PR5's own entry for what actually shipped):

- **Favicon fetching/storage.** plan-77 §3's one-line PR4 summary lists
  "favicon解決" (an `internal/ingest/favicon` package, SSRF-safe via
  `internal/ingest/safehttp`, storing the result through PR1's Drive
  foundation as a `source_favicon`-purpose `files` row, and projecting
  it as the source actor's avatar). Plan-77's detailed §2.4 rewrite
  (workerA, 2026-09-08) focused entirely on the host/username design
  question above and did not re-specify favicon mechanics; the owner
  resolved the open "PR4 follow-up vs. fold into PR5 vs. own PR" question
  by choosing the fold-into-PR5 option this entry originally flagged.

## PR5: Profile images + favicon fetching

**Status: complete.** Depended on PR1 and PR3. Scope grew from the
original plan-77 outline (profile images only) to include PR4's deferred
favicon fetching (see PR4's "Deferred to PR5" note above), on the owner's
explicit reasoning that both need the same Drive-backed image storage
infrastructure.

Profile images:

- `actors.avatar_file_id` (nullable FK to `files`, migration
  `0030_actors_avatar_file_id.sql`), plumbed through
  `ActorRepository.SetAvatarFileID` / `domain.Actor.AvatarFileID` /
  `miauth.Service.UpdateOwnerAvatar` (owner-only; `ErrNotOwner` otherwise)
  and `OwnerProfile.AvatarFileID`.
- `avatarUrl` (never `avatarId` — see this document's PR0 notes and
  `docs/compat/aria-v1.5.11.md`'s existing `User`/`avatarId` discriminator
  guardrail) on `userLite` and `userDetailedNotMe`, resolved by the shared
  `avatarURLFromFileID` helper to an absolute `GET /files/{id}` URL.
  **Design decision:** when `AvatarFileID` is nil, `avatarURLFromFileID`
  returns `nil` and `avatarUrl` is simply `null` on the wire — this
  service never generates an initials/placeholder image server-side.
  This matches real Misskey servers, which likewise send `avatarUrl:
  null` for an unset avatar and leave rendering a fallback (an initial,
  a gray silhouette, ...) entirely to the client. Issue #77's acceptance
  criterion "新規インストール直後でもデフォルトアバターが表示される" is
  therefore satisfied by Aria's own existing null-avatar fallback
  rendering, not by anything new here.
- `POST /api/i/update` accepts an optional `avatarId` field alongside the
  pre-existing `name` field — **`name` changed from always-required to
  optional**, a genuine bug fix: PR0's Aria trace found
  `INotifier.setAvatarId` sends `{"avatarId": ...}` alone with no `name`
  key, which the pre-PR5 "name is required" shape would have wrongly
  rejected. An explicit `avatarId: null` clears the avatar (plan-77's
  confirmed upload sequence); a non-null `avatarId` is validated as a
  `files` row the requesting owner actually owns
  (`drive.Service.ShowFile`) before being written.
- VirtualActor (Open WebUI model) avatars are **not** wired in this PR —
  `avatar_file_id` is generic to any actor, but no `openwebuictl` command
  or other write path sets it for a model actor yet; left for a future
  PR if ever needed.

Favicon fetching:

- `internal/ingest/favicon` (new package): `Fetch` GETs `https://<host>/
  favicon.ico` over a dedicated `internal/ingest/safehttp.Client`
  (fixed `AllowInsecureHTTP: false` policy, independent of
  `RSS_ALLOW_INSECURE_HTTP` — a deliberate simplification, since every
  failure here is meant to be swallowed as best-effort rather than
  surfaced); `ExtractPNGFromICO` parses the ICO container and returns the
  largest embedded "PNG-in-ICO" image, rejecting the legacy BMP-in-ICO
  shape as an accepted, documented limitation.
- `drive.Service.CreateSystemFile` (new, alongside a refactored-but-
  behavior-unchanged `CreateFile`): stores an owner-less file — used only
  for `source_favicon`-purpose files, since a favicon belongs to no Drive
  user.
- `cmd/server.ensureRSSSourcesWithActors` calls the new
  `fetchAndSetSourceFavicon` after each newly-registered RSS source's
  actor+source pair commits: fetch → validate/store via
  `CreateSystemFile` → `ActorRepository.SetAvatarFileID`, entirely
  best-effort (every failure is logged at `info`/`warn` and swallowed,
  never blocking source registration) and outside the actor/source
  database transaction.
- `resolveUserLite`'s existing `ActorExternalSource` case
  (`internal/httpserver/noteapi_wire.go`) now also projects
  `avatarUrl` from the resolved actor's `AvatarFileID`, reusing the same
  `avatarURLFromFileID` helper profile images use.

## PR6: Post attachments

**Status: complete.** Depended on PR1 and PR3.

- `POST /api/notes/create` accepts `fileIds`; each ID is validated
  (`drive.Service.ValidateAttachmentFiles`) as a `files` row owned by the
  requesting owner with `purpose = 'attachment'` — a bad ID rejects the
  whole call with `NO_SUCH_FILE` before any entry or attachment row is
  written. An omitted `fileIds` key (PR0's trace: the common,
  attachment-less case) is unaffected.
- New `entry_files` table (`entry_id`, `file_id`, `position`; migration
  `0031_entry_files.sql`) as a genuine many-to-many join — see PR0's
  finding above on why a 1:1 design is insufficient. The attachment link
  is written inside the same transaction that creates the entry, via
  `internal/timeline.Service`'s existing `EntryHook` mechanism
  (Issue #53's own extension point); `internal/httpserver`'s new
  `combineEntryHooks` composes it with the pre-existing Open WebUI bridge
  hook, since `CreateRootWithHook`/`CreateReplyWithHook` accept only one.
- `note.fileIds`/`note.files` (previously hardcoded empty,
  `internal/httpserver/noteapi_wire.go`) now project real data —
  `(*Server).projectNote` queries `timeline.Service.AttachedFiles` and
  reuses PR3's `DriveFile` wire type/`projectDriveFile` helper, in
  attachment order.
- `POST /api/drive/files/attached-notes` (PR3 shipped it always
  returning `[]`) now queries `entry_files` for real via the new
  `timeline.Service.AttachedEntries`, filtering out a hidden/archived
  note the same way every other note-reading endpoint does.
- **Deleting a still-attached file is rejected, not cascaded**:
  `drive.Service.DeleteFile` checks `EntryFileRepository.CountByFile`
  first and returns the new `ErrFileAttached` (wire: `FILE_ATTACHED`) —
  this PR's own resolution of the "ownership/GC edge case" PR0 left open,
  the entries-row half of which never arises since `notes/delete` is a
  soft hide, never a hard delete (`docs/decisions/0004-note-delete-as-hide.md`).
- Attachments are never auto-injected into an LLM prompt (AGENTS.md;
  vision-input use is a separate future issue if ever pursued) — nothing
  in this PR touches `internal/llmreply`/`internal/llmclassify`.

## PR7: Operational hardening

**Status: complete.** Last PR; depended on the full stack above. Issue
#77's final PR — every acceptance criterion is now implemented.

- **Orphaned-file GC**, backend-agnostic by construction: `internal/drive.
  Storage` gained a `List` method (implemented for both `Local` and `S3`,
  the latter through minio-go's `ListObjects`), and `Service.RunOrphanGC`
  cross-references its output against the new
  `FileRepository.ListStorageKeys` to delete any object no `files` row
  references — the safety net for `storeValidatedImage`'s and
  `DeleteFile`'s own documented best-effort-cleanup failure paths. A new
  `internal/drive.OrphanGCJob`/`GCScheduler` pair (mirroring
  `internal/openwebui.CatalogSyncJob`/`CatalogScheduler`'s "one global
  sweep per tick" shape) runs it on `DRIVE_ORPHAN_GC_INTERVAL` (default
  `24h`) through the existing `internal/jobs` durable-job infrastructure,
  registered and scheduled unconditionally in `cmd/server` — Drive has no
  "off" state, so this sweep always runs regardless of `DRIVE_BACKEND` or
  how many files exist.
- **Backup/restore**: `cmd/backupctl`'s row-count summary (`backupTables`)
  now includes `files`, so `backupctl verify` reports Drive metadata
  alongside every other core table. The actual object bytes are
  documented, not reimplemented: `docs/operations/backup-restore.md`'s
  new "Drive-backed media" section covers `localdisk` (an ordinary
  directory tree — back it up with any general-purpose tool, ideally on
  the same schedule/destination as the database) and `s3compat`
  ("delegate to the object store's own versioning/replication," per
  plan-77 v2 §2.7's own framing), plus the operational implication of a
  restore where the database and object storage snapshots come from
  different points in time (a `files` row whose object went missing —
  the opposite direction from what orphan GC cleans up).
- **VirtualActor avatar management** (plan-77 v2 §2.5's explicit "モデル
  ごとに専用の管理コマンドをopenwebuictlに追加する程度で足りる"
  recommendation, deferred from PR5): new `openwebuictl avatar-set
  <model-slug> <image-path>` / `avatar-clear <model-slug>` subcommands,
  closing Issue #77's own acceptance criterion that both users *and*
  VirtualActors can set/change/delete a profile image — PR5 only ever
  covered the user half. Reuses `domain.FilePurposeAvatar` (declared
  since PR1's migration, never actually assigned by any code path until
  now — the owner's own avatar reuses an ordinary
  `FilePurposeAttachment` file already uploaded through the Drive API,
  matching Aria's real upload-then-set-avatarId sequence, so this is
  genuinely the first caller `FilePurposeAvatar` existed for) via
  `drive.Service.CreateSystemFile`.
- **A genuine bug found and fixed while writing this PR's SSRF regression
  test**: `drive.Service.UploadFromURL`'s own error wrapping used
  `fmt.Errorf("%w: %v", ErrUploadFromURLFailed, err)` — a single `%w`
  verb wraps only `ErrUploadFromURLFailed`, silently dropping the
  underlying `err` (frequently `safehttp.ErrPolicyViolation`, an SSRF
  policy rejection) from the resulting error chain, contradicting
  `ErrUploadFromURLFailed`'s own doc comment's promise that
  `errors.Is(err, safehttp.ErrPolicyViolation)` would work. Fixed to
  `fmt.Errorf("%w: %w", ...)` (Go 1.20+'s multi-`%w` support); locked in
  by `TestService_UploadFromURL_PreservesPolicyViolationInErrorChain`.
  `internal/ingest/rss`'s own SSRF classification (`classifyDoError`) was
  checked and found unaffected — it inspects the raw transport error
  before any re-wrapping, unlike `UploadFromURL`.
- **Storage-failure-path tests** (new): a working `files` row whose
  backing object errors out on the storage backend must never be
  confused with the row not existing at all
  (`TestService_OpenFile_StorageBackendFailureIsNotConfusedWithNotFound`);
  a row-insert failure after the object was already stored must clean
  the object back up
  (`TestService_CreateFile_CleansUpStorageObjectWhenRowInsertFails`); a
  failing storage-side delete must not block the row itself from being
  removed
  (`TestService_DeleteFile_SucceedsEvenWhenStorageDeleteFails`) — all
  three exercised through a new `failingStorage` test fixture
  (`internal/drive/service_test.go`) rather than real backend outages.
- **Dedicated image-validation tests** (SVG rejection was already
  covered, from PR1): added an exact dimension-limit boundary test
  (`TestValidateImage_DimensionLimitIsInclusive`, pinning `>` not `>=`)
  and an empty-input rejection test. SSRF/oversized-response tests for
  favicon fetching landed already, in PR5 (`internal/ingest/favicon`'s
  own test file). (The `entry_files` delete-while-attached edge case
  PR0 originally left open was already resolved in PR6 itself, not
  deferred here — see PR6's own entry above for `ErrFileAttached`.)

## Documentation follow-ups tracked across this feature

- `README.md`'s "Known limitations" line listing "files/drive" as
  unimplemented must be updated once Drive ships (PR3), not in PR0/PR1
  while it is still accurate.
- `docs/compat/aria-v1.5.11.md`: PR0 added the Drive/attachment contract
  section and allowlist rows (done). PR3 promotes them from "planned" to
  "implemented" (done). PR4 updated the existing provenance-is-a-fixed-
  actor framing in "Note.text provenance markers" (done). PR5 updated the
  `i/update` avatar non-goal note PR0 had flagged (done). PR6 promoted
  the `fileIds`/`attached-notes` allowlist rows to "implemented" and
  added its own "PR6 implementation notes" subsection recording the
  file-attached delete rejection and the entry-creation-transaction hook
  composition (done).
- New `docs/decisions/000X-drive-storage-boundary.md` ADR (PR1).
- `docs/operations/configuration.md`: new Drive-related configuration keys
  (PR1, PR7's `DRIVE_ORPHAN_GC_INTERVAL`) (done).
- `docs/operations/backup-restore.md`: local-disk and S3 backup guidance,
  plus `backupctl verify`'s new `files` row count (done, PR7).
- `docs/operations/runbook.md`: storage failure / orphan GC / Drive
  incident procedures (done, PR7).

## Issue #77 acceptance criteria — final cross-check (PR7)

Checked against the tracker issue's own 受け入れ条件 list at the close of
PR7, the last PR in this roadmap:

1. **新規インストール直後でもfavicon・アプリアイコン・デフォルトアバターが
   表示される** — ✅ favicon/app icon: PR2's static asset set. Default
   avatar: `avatarUrl` is correctly `null` until set (PR5); rendering a
   placeholder for that `null` is Aria's own baseline client
   responsibility for any Misskey-compatible server, not something this
   service hosts or generates — see docs/compat/aria-v1.5.11.md's newly
   added note on this (要実機確認, consistent with this document's
   existing convention for unverified client-rendering claims).
2. **RSSソースごとにアイコンと帰属が表示され、system固定ではない** — ✅
   PR4 (`ActorExternalSource`, host = real feed domain, ADR-0007) + PR5
   (favicon fetch → actor avatar).
3. **RSS投稿から元記事とソース情報を確認できる** — ✅ PR4's `note.url` =
   `ProvenanceURL`; the pre-existing `[news: DisplayName] ...` body
   marker.
4. **ユーザーとVirtualActorがプロフィール画像を設定・変更・削除できる** —
   ✅ User: PR5's `POST /api/i/update {avatarId}` (set/change, explicit
   `null` to delete). VirtualActor: **closed in this PR** —
   `openwebuictl avatar-set`/`avatar-clear` (see PR7's own entry above);
   PR5 had deliberately deferred this half, and it was not picked up
   again until this final cross-check caught it.
5. **画像アップロードにサイズ／形式／認可の検証がある** — ✅
   `DRIVE_MAX_FILE_BYTES`/`ValidateImage` (format + pixel dimensions,
   SVG rejected) + `ScopeReadDrive`/`ScopeWriteDrive` + per-request
   ownership checks (PR3).
6. **Drive API／CLIの設計とDBスキーマ、ローカル保存の実装がある** — ✅
   PR1 (schema, `Local`/`S3` backends) + PR3 (the Drive API itself). The
   plan's own §2.5 framed a CLI avatar path as an optional emergency
   fallback for the owner (Drive API + `i/update` is the primary path),
   not a required deliverable on its own; PR7 adds one for VirtualActors
   specifically because, unlike the owner, a model has no other write
   path to its own avatar at all.
7. **バックアップ／リストア、孤児GC、ストレージ障害時の挙動をテストする**
   — ✅ All three landed in this PR: `RunOrphanGC` + `GCScheduler`/
   `OrphanGCJob`, `backupctl`'s `files` row count, and the new
   `failingStorage`-based storage-failure-path tests.
8. **favicon自動取得のSSRF・過大レスポンス・外部追跡リスクをテストする**
   — ✅ SSRF and oversized-response: `internal/ingest/favicon`'s own test
   file (PR5). External tracking risk: structurally eliminated by
   design, not merely tested — `favicon.Fetch` only ever requests a
   fixed, self-derived `https://<host>/favicon.ico` path; it does not
   parse or follow any feed-supplied `<icon>`/`<logo>` URL at all
   (confirmed: `internal/ingest/rss` never parses either field), so
   there is no arbitrary/tracking URL this service could be tricked into
   fetching in the first place.

Every acceptance criterion is satisfied; item 4's VirtualActor gap is
the only one that required new work discovered specifically by this
final cross-check (rather than mid-PR review), and it is closed in this
PR's own commit.

## References

- Issue #77 and its plan record (ccserver document board key `plan-77`,
  v2)
- [`docs/compat/aria-v1.5.11.md`](../compat/aria-v1.5.11.md)'s "Drive API
  and note attachments" section (PR0's detailed findings) and updated
  endpoint allowlist table
- [`docs/roadmap/openwebui.md`](openwebui.md) — the sibling large-feature
  roadmap document this one follows the structure of
- `internal/openwebui/registry.go`'s `SecretRefAPIKey` — the credential
  pattern PR1's S3 secrets reuse
- `internal/ingest/safehttp` — the SSRF-safe HTTP client PR5's
  `internal/ingest/favicon` fetcher reuses
