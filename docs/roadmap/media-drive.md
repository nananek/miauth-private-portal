# Drive-backed media storage, Misskey Drive API, and attachments roadmap

- Status: PR0 (investigation) and PR1 (Drive foundation) complete.
  PR2–PR7 not started.
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
  -> PR3 (Misskey-compatible Drive API)
  -> PR6 (post attachments)

PR1 (Drive foundation: Storage abstraction, local disk + S3-compatible,
     files table, image validation)
  -> PR3, PR4, PR5, PR6

PR2 (static app/source icons) — independent, no dependency on PR1/PR0

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
  via `minio-go` — see ADR-0006 D4 for why not the full AWS SDK for Go
  v2). Both share one contract test suite
  (`internal/drive/storage_contract_test.go`); `S3`'s own tests run
  against an in-process fake S3-compatible `httptest.Server`
  (`internal/drive/s3_test.go`), not a real MinIO/S3 endpoint.
- `files` table (migration `0022_files.sql`): `id`, `owner_actor_id`
  (nullable), `purpose` (closed enum: `avatar`/`source_favicon`/
  `attachment`/`app_icon`), `mime`, `byte_size`, `sha256`, `storage_key`
  (unique), `width`/`height` (nullable), `created_at`. No repository
  reads or writes it yet — see ADR-0006 D6 for why that is deferred to
  PR3/PR4/PR5/PR6, the same precedent migration `0017` (OWUI-C) set.
- Image validation (`internal/drive/validate.go`'s `ValidateImage`):
  decodes against an allowlist (`png`, `jpeg`, `webp`) with Go's
  `image.DecodeConfig`, ignoring any client-declared `Content-Type`.
  SVG is rejected by the same fails-to-decode path (ADR-0006 D2) — no
  dedicated SVG/XML detector exists. Width/height ceilings
  (`DRIVE_MAX_IMAGE_WIDTH`/`DRIVE_MAX_IMAGE_HEIGHT`) are checked against
  the decoded header, independent of `DRIVE_MAX_FILE_BYTES`.
- S3 credentials (`DRIVE_S3_ACCESS_KEY_ID`/`DRIVE_S3_SECRET_ACCESS_KEY`)
  are plain configured values in this PR, not a `secret_ref` indirection
  — see ADR-0006 D5 for why `internal/openwebui/registry.go`'s pattern
  does not apply here yet (no Drive configuration is persisted to any
  database row for a `secret_ref` to name).
- New configuration keys (`internal/config.DriveConfig`, no
  `DRIVE_ENABLED` flag — see ADR-0006's "Consequences"):
  `DRIVE_BACKEND` (default `localdisk`), `DRIVE_DATA_DIR` (default
  `./data/drive`), `DRIVE_S3_ENDPOINT`/`DRIVE_S3_BUCKET`/
  `DRIVE_S3_ACCESS_KEY_ID`/`DRIVE_S3_SECRET_ACCESS_KEY` (required when
  `DRIVE_BACKEND=s3compat`), `DRIVE_S3_USE_SSL` (default `true`),
  `DRIVE_S3_REGION`, `DRIVE_MAX_FILE_BYTES` (default 10 MiB),
  `DRIVE_MAX_IMAGE_WIDTH`/`DRIVE_MAX_IMAGE_HEIGHT` (default 8000).
  Documented in `docs/operations/configuration.md`'s "Known
  configuration keys" table and "Drive storage foundation" section.
- New ADR: [`docs/decisions/0006-drive-storage-boundary.md`](../decisions/0006-drive-storage-boundary.md).
- New dependencies: `github.com/minio/minio-go/v7` (S3-compatible
  client, ADR-0006 D4) and `golang.org/x/image` (WebP decode support,
  ADR-0006 D2) — both standard-library-adjacent or narrowly scoped to
  the concrete need above, per AGENTS.md's "new dependencies require a
  concrete reason."

## PR2: Portal/app static icons

**Status: not started.** Fixed favicon/OGP/PWA icon set served via
`embed.FS`. Independent of Drive; can proceed in parallel with PR1.

## PR3: Misskey-compatible Drive API

**Status: not started.** Depends on PR0 (this document) and PR1.

- `POST /api/drive`, `/files`, `/files/create`, `/files/show`,
  `/files/update`, `/files/delete`, `/files/upload-from-url`,
  `/files/attached-notes`, `/folders`, `/folders/create`,
  `/folders/delete`, `/folders/update`, `/folders/show` — see PR0's
  findings above on which endpoints are genuinely required and which
  (`stream`, `files/find`, `files/check-existence`, `files/find-by-hash`,
  `folders/find`, `files/move-bulk`) are not.
- New `ScopeReadDrive`/`ScopeWriteDrive` added to
  `internal/miauth/scope.go`'s `grantableScopes`. Aria's MiAuth
  `permission` query already requests `read:drive,write:drive` today, so
  an API token issued before this PR ships will need re-authorization to
  gain these scopes (same situation this repository already has for
  `read:notifications`/Issue #23 PR6).
- New `DriveFile` wire type (`internal/httpserver/drive_wire.go`),
  matching PR0's traced required/nullable field split.
- Formal contract write-up added to `docs/compat/aria-v1.5.11.md` (PR0
  already added the investigation findings; PR3 promotes the relevant
  parts to "implemented").

## PR4: RSS/external-source icons and attribution

**Status: not started.** Depends on PR1. Adds an `external_source` actor
type (rebuild migration), favicon resolution via `internal/ingest/favicon`
(new package, SSRF-safe via the existing `internal/ingest/safehttp`
client), `resolveUserLite` projection, and provenance-URL projection onto
`note.url`. Also updates `docs/compat/aria-v1.5.11.md`'s existing
"single local owner/system actor" provenance framing, which this PR makes
inaccurate for RSS/mail-authored notes.

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
  "implemented." PR4 must update the existing provenance-is-a-fixed-actor
  framing. PR5 must update the `i/update` avatar non-goal note (PR0 already
  flagged the exact sentence).
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
