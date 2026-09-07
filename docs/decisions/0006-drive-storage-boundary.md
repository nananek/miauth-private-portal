# ADR-0006: Drive storage abstraction boundary

- Status: Accepted for Issue #77 PR1
- Date: 2026-09-08
- Scope: Issue #77 PR1 (`internal/drive` foundation: `Storage`
  abstraction, local disk and S3-compatible implementations, raster-image
  validation, the `files` table). Does not cover the Misskey-compatible
  Drive API (PR3), profile avatars (PR5), external-source favicons (PR4),
  or post attachments (PR6) — those are later PRs against this
  foundation; see `docs/roadmap/media-drive.md`.

## Context

Issue #77 (v2, scope-confirmed) requires a real media/avatar storage
backend where today there is none: no `Actor`/`VirtualActor`/wire-projected
user carries an avatar field, and no static file route exists at all. The
owner confirmed four scope points that shape this foundation:

1. The Misskey-compatible Drive API itself must be implemented (PR3), not
   only a CLI-managed avatar path — Issue #77 PR0's investigation confirmed
   Aria's drive screen is a real, actively used surface.
2. Generic post-attachment upload (PR6) is in scope, not only avatars/icons.
3. SVG/vector images must be rejected — raster only.
4. An S3-compatible storage backend must be implemented, not only local
   disk.

Several design questions had to be settled before any of PR3-PR6 could
start:

- What does the byte-storage interface look like, and how many backend
  implementations does this PR provide?
- How does an S3-compatible backend authenticate, and does that reuse an
  existing pattern in this codebase?
- How is "this is really an image, and really not SVG" enforced, given
  that a client's declared `Content-Type` and file extension are both
  attacker-controlled?
- What does the `files` metadata table look like, and does this PR wire a
  repository to it?

## Decision

### D1: A narrow `Storage` interface, not a filesystem-shaped one

`internal/drive.Storage` is exactly three methods:

```go
type Storage interface {
    Put(ctx context.Context, key string, r io.Reader, size int64) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Delete(ctx context.Context, key string) error
}
```

No `List`, no `Stat`/metadata retrieval, no directory/prefix concept. Every
metadata question (what MIME type, whose file, what purpose) is the
`files` table's job, not `Storage`'s — this mirrors AGENTS.md's "put
persistence and provider boundaries behind narrow interfaces" and keeps
`internal/drive` ignorant of the Misskey wire format or any HTTP concern,
the same way `internal/ingest/safehttp` stays ignorant of any specific
feed adapter. `Put` is create-only (an existing key is `ErrKeyExists`, not
silently overwritten): keys are meant to be assigned once per uploaded
file by a caller-side UUID-derived scheme, never reused, so a second `Put`
of the same key is always a caller bug worth surfacing rather than a
legitimate overwrite. `Delete` is idempotent (deleting a missing key is
not an error) because orphan GC (PR7) and a failed upload's own cleanup
path both need to delete without first checking existence.

### D2: Raster-only via decode, not declared-type or extension checks

`ValidateImage` decodes an upload's claimed image structure with Go's
`image.DecodeConfig` against an explicit allowlist (`png`, `jpeg`,
`webp`), reading only the header (not the full pixel data, so this is
cheap even for a large file). It never trusts a client-declared
`Content-Type` or file extension — both are attacker-controlled input
(AGENTS.md: "treat ... remote API responses ... as untrusted data",
applied here to an upload's own metadata).

**SVG is rejected by this same fails-to-decode path, not a dedicated
SVG/XML detector.** An SVG document is XML text, not any registered
raster format's binary header, so `image.DecodeConfig` already fails to
decode it — no separate SVG-sniffing or XML-parsing code exists or needs
to exist. This is deliberately the same mechanism that rejects "a text
file renamed to .png" or any other non-image upload: SVG gets no special
case, it simply never satisfies the one check every upload goes through.

WebP support requires `golang.org/x/image/webp` (the Go team's own
extended-standard-library module) alongside the two decoders
(`image/png`, `image/jpeg`) already in the standard library, since
`image/webp` is not part of it. This is the only new dependency D2 needed.

Width/height ceilings (`DRIVE_MAX_IMAGE_WIDTH`/`DRIVE_MAX_IMAGE_HEIGHT`)
are checked against the same decoded header, independent of the byte-size
ceiling (`DRIVE_MAX_FILE_BYTES`): a small but pathologically
large-dimension file (a "decompression bomb") is rejected even when it
fits comfortably under the byte bound.

### D3: Two Storage implementations, one per deployment for its whole lifetime

`Local` (local disk) and `S3` (S3-compatible) both ship in this PR, per
the owner's confirmed scope point 4. `DRIVE_BACKEND` selects exactly one
for the deployment's entire lifetime — there is no per-file or
per-request backend switch, and this PR includes no migration tool to
move existing objects from one backend to another (a future CLI
follow-up, if ever needed, is explicitly out of scope here).

`Local` requires its root directory (`DRIVE_DATA_DIR`) to already exist;
it never creates the root itself, only `key`'s own intermediate
subdirectories under it — a misconfigured path fails closed at first use
rather than silently growing an unexpected directory tree. Every key is
checked with `filepath.IsLocal` before it touches the filesystem, which
*rejects* a traversal attempt (`"../escape"`) rather than the alternative
of `filepath.Clean`-then-`Join`, which would silently renormalize such a
key into a different (still safely-contained) path — rejection surfaces
a caller bug instead of quietly hiding it.

### D4: `minio-go`, not the AWS SDK for Go v2, for the S3-compatible backend

AGENTS.md requires a concrete reason for a new dependency. The concrete
reason here: this deployment's stated target is "an S3-compatible object
store" (AWS S3 itself, or commonly a self-hosted MinIO instance), not
AWS-specific service integration. `minio-go` is a client library whose
primary design goal is broad S3-compatible-server support; the full AWS
SDK for Go v2 is a much larger dependency surface oriented around AWS's
own service catalog, most of which this deployment would never use. The
`S3` implementation issues plain, non-chunked-signed-payload requests
(`SendContentMd5: true`, `DisableContentSha256: true` on every `PutObject`
call) rather than `minio-go`'s default streaming-signed-payload upload
encoding, since not every S3-compatible server this deployment might
point at is guaranteed to support that encoding — a plain MD5-verified
body is the more broadly compatible choice for a backend whose only
promised property is "S3-compatible," not "AWS S3 itself."

### D5: S3 credentials are plain configured values in this PR, not a `secret_ref`

`internal/openwebui/registry.go`'s `secret_ref` pattern — a database row
stores only a configuration key's *name*, never a credential's value —
exists because `openwebui_workspaces` is a database-persisted
configuration row a `secret_ref` column can point *at* a named
environment variable from. This PR persists no Drive configuration to any
database row: `DriveConfig`'s S3 fields (`S3AccessKeyID`,
`S3SecretAccessKey`) are plain values internal/config resolves directly
from `DRIVE_S3_ACCESS_KEY_ID`/`DRIVE_S3_SECRET_ACCESS_KEY`, the same shape
`OpenWebUIConfig.APIKey` itself has before `Registry.Seed` ever writes a
`secret_ref` naming it into a workspace row. If a future PR adds a
database-stored Drive setting that needs to name a credential, it should
reuse the same `secret_ref` indirection rather than storing the raw value
a second time — this decision is about *this* PR's scope, not a rejection
of the pattern.

### D6: The `files` table ships now; its repository does not

Migration `0022_files.sql` creates the `files` table (`id`,
`owner_actor_id` nullable, `purpose` closed-enum, `mime`, `byte_size`,
`sha256`, `storage_key` unique, `width`/`height` nullable, `created_at`)
in this PR. No `domain.FileRepository` interface, no SQLite
implementation, and no row in `domain.Repos` is added here — the same
precedent migration `0017_openwebui_registry.sql` (Issue #52 OWUI-C) set
for `openwebui_workspaces`/`openwebui_models`, whose comment explicitly
notes "no row is created by this migration and nothing is wired to it
yet." Each of PR3 (Drive API uploads), PR4 (favicons), PR5 (avatars), and
PR6 (attachments) is expected to add its own narrow repository against
this table when it has a concrete use case driving that interface's
shape, rather than this PR guessing at one now that no caller yet
exercises — AGENTS.md's "do not add [work] unless an issue in the tracker
explicitly promotes that work," applied to premature repository
abstraction the same way it applies to premature features.

### D7: No automatic LLM prompt injection

Nothing in this foundation, nor anything planned for PR3-PR6, feeds an
uploaded file's bytes or metadata into an LLM prompt automatically.
AGENTS.md's "treat ... remote API responses ... as untrusted data" and
the broader principle of never feeding external content into a prompt
without explicit configuration both apply; a future issue that wants to
use an attachment as LLM vision input must add that explicitly and
separately.

## Consequences

- `go.mod` gains three new direct-ish dependencies:
  `github.com/minio/minio-go/v7` (D4) and `golang.org/x/image` (D2, for
  WebP decoding). Both are pulled in by concrete, documented needs above,
  not spuriously.
- A deployment cannot mix backends or migrate between them without a
  future, separately-scoped tool; operators choosing `s3compat` after
  already running `localdisk` in production (or vice versa) must plan for
  that as a manual one-time migration outside this repository's tooling.
- Orphan GC (PR7) and backup/restore documentation (PR7) both key off the
  `files` table's metadata regardless of which backend is configured,
  since `Storage`'s narrow interface gives both implementations identical
  observable behavior from a caller's point of view.
- `DRIVE_BACKEND` and its sibling keys are validated on every startup
  (there is no `DRIVE_ENABLED` flag to gate them behind) even though
  nothing reads through this configuration yet — an operator who sets
  `DRIVE_BACKEND=s3compat` without also setting the required S3 fields
  gets a startup-time configuration error today, well before PR3 gives
  that misconfiguration any visible effect.
