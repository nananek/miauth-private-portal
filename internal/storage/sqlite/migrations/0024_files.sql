-- Issue #77 PR1 (renumbered to 0024 when this PR was cherry-picked onto
-- main after Issue #76's own migrations 0021-0023 landed, which post-
-- date this PR's original authorship): the files table backing
-- internal/drive's Storage abstraction (ADR-0007). This migration only
-- creates the table; no row is written and nothing reads from it yet.
-- PR3 (Misskey-compatible
-- Drive API), PR4 (external-source favicons), PR5 (profile avatars), and
-- PR6 (post attachments) are what populate and query it, each through
-- its own repository added when that use case exists.
--
-- storage_key is the opaque key internal/drive.Storage.Put/Get/Delete
-- use; it says nothing about the configured backend (local disk or
-- S3-compatible) — a deployment picks exactly one backend for its whole
-- lifetime (ADR-0007), so no column here distinguishes rows by backend.
CREATE TABLE files (
    id TEXT PRIMARY KEY,
    -- NULL for a file with no single owning actor (for example a PR4
    -- external-source favicon, which belongs to a source, not an actor).
    owner_actor_id TEXT REFERENCES actors (id),
    -- A closed set: what this file is for, not a free-form label. Every
    -- current use case (PR3 drive uploads default to 'attachment';
    -- PR4/PR5/PR6 name their own) fits one of these.
    purpose TEXT NOT NULL CHECK (purpose IN ('avatar', 'source_favicon', 'attachment', 'app_icon')),
    mime TEXT NOT NULL,
    byte_size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    -- Opaque key into the configured internal/drive.Storage backend.
    -- Unique because it is also the physical object's identity: two
    -- files rows must never race to write (or delete) the same
    -- underlying object.
    storage_key TEXT NOT NULL UNIQUE,
    -- Raster images only (ADR-0007 rejects SVG/vector); NULL together
    -- for a non-image file.
    width INTEGER,
    height INTEGER,
    created_at TEXT NOT NULL
);

CREATE INDEX idx_files_owner_actor_id ON files (owner_actor_id) WHERE owner_actor_id IS NOT NULL;
