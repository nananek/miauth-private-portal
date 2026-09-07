# Drive-backed media storage, Misskey Drive API, and attachments roadmap

- Status: PR0 (investigation), PR1 (Drive foundation), PR2 (static app
  icons), and PR3 (Misskey-compatible Drive API) complete. PR4
  (RSS/external-source icons and attribution) partially complete — its
  identity/host mechanism (ADR-0007) is done; favicon fetching/storage
  is not. PR5–PR7 not started.
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
  Whether to start honoring it is a PR6/PR7 decision, out of PR0's scope.

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

**Status: partially complete.** Depended on PR1. The identity/
attribution half of this PR (plan-77 §2.4, ADR-0007's design-A decision)
is done; **favicon fetching/storage is not yet implemented** — see
"Not yet done" below.

Done:

- `domain.ActorExternalSource` (Issue #52's "1 external identity = 1
  actor row" pattern, applied to RSS-kind `domain.ExternalSource`), a
  rebuild migration (`0025_actors_external_source_type.sql`, mirroring
  migration 0016's shape) and two plain-ADD-COLUMN migrations
  (`0026_external_sources_identity.sql`: `actor_id`/`username`/`host`
  plus a `UNIQUE(host, username)` partial index;
  `0027_entries_provenance_url.sql`: denormalizes each ingested entry's
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
- New ADR: [`docs/decisions/0007-external-source-identity.md`](../decisions/0007-external-source-identity.md)
  — the design-A decision, the considered-and-accepted impersonation-
  adjacent concerns, why IMAP is deliberately excluded, and the
  "Revisit if real federation ships" condition plan-77 §2.4.7 required.
- **Deliberately excluded from this PR (ADR-0007's own scope note):**
  IMAP-kind sources keep projecting as the shared `system` actor,
  unchanged.

Not yet done (open follow-up, not covered by this PR's commit):

- **Favicon fetching/storage.** plan-77 §3's one-line PR4 summary lists
  "favicon解決" (an `internal/ingest/favicon` package, SSRF-safe via
  `internal/ingest/safehttp`, storing the result through PR1's Drive
  foundation as a `source_favicon`-purpose `files` row, and projecting
  it as the source actor's avatar). Plan-77's detailed §2.4 rewrite
  (workerA, 2026-09-08) focuses entirely on the host/username design
  question above and does not re-specify favicon mechanics; whether to
  implement it as a PR4 follow-up commit, fold it into PR5 (which
  already owns `actors.avatar_file_id`, the column favicon storage would
  reuse per plan-77 §2.5), or track it as its own PR is an open decision
  for the tracker, not resolved by this entry.

## PR5: Profile images

**Status: not started.** Depends on PR1 and PR3. Adds
`actors.avatar_file_id` (nullable FK), projects `avatarUrl` (never
`avatarId` — see this document's PR0 notes and
`docs/compat/aria-v1.5.11.md`'s existing `User`/`avatarId` discriminator
guardrail) onto `userLite`/`userDetailedNotMe`, and implements the
`i/update {avatarId}` field PR0 confirmed Aria sends. VirtualActor
(Open WebUI model) avatars reuse the same `avatar_file_id` column, set via
an `openwebuictl` command rather than the Drive API (a model is never a
login-capable Aria user).

## PR6: Post attachments

**Status: not started.** Depends on PR1 and PR3.

- `POST /api/notes/create` accepts `fileIds`; each ID must be a `files`
  row owned by the requesting owner with `purpose = 'attachment'`.
- New `entry_files` table (`entry_id`, `file_id`, `position`) as a genuine
  many-to-many join — see PR0's finding above on why a 1:1 design is
  insufficient.
- `note.fileIds`/`note.files` (currently hardcoded empty,
  `internal/httpserver/noteapi_wire.go`) switch to real data, reusing
  PR3's `DriveFile` wire type.
- Attachments are never auto-injected into an LLM prompt (AGENTS.md;
  vision-input use is a separate future issue if ever pursued).

## PR7: Operational hardening

**Status: not started.** Last PR; depends on the full stack above.

- Orphaned-file GC via the existing `internal/jobs` durable-job
  infrastructure, backend-agnostic (keys off the `files` table regardless
  of `localdisk`/`s3compat`).
- Backup/restore: local disk gets a `cmd/backupctl` extension or
  documented directory-level backup; S3 backend backup is documented as
  "delegate to the object store's own versioning/replication," not
  reimplemented in this repository.
- Storage-failure-path tests, SSRF/oversized-response tests for favicon
  fetching (PR4), and dedicated image-validation tests (SVG rejection
  included).

## Documentation follow-ups tracked across this feature

- `README.md`'s "Known limitations" line listing "files/drive" as
  unimplemented must be updated once Drive ships (PR3), not in PR0/PR1
  while it is still accurate.
- `docs/compat/aria-v1.5.11.md`: PR0 added the Drive/attachment contract
  section and allowlist rows (done). PR3 promotes them from "planned" to
  "implemented" (done). PR4 updated the existing provenance-is-a-fixed-
  actor framing in "Note.text provenance markers" (done). PR5 must
  update the `i/update` avatar non-goal note (PR0 already flagged the
  exact sentence).
- New `docs/decisions/000X-drive-storage-boundary.md` ADR (PR1).
- `docs/operations/configuration.md`: new Drive-related configuration keys
  (PR1).
- `docs/operations/backup-restore.md`: local-disk and S3 backup guidance
  (PR7).
- `docs/operations/runbook.md`: storage failure / orphan GC / Drive
  incident procedures (PR7).

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
- `internal/ingest/safehttp` — the SSRF-safe HTTP client PR4's favicon
  fetcher reuses
