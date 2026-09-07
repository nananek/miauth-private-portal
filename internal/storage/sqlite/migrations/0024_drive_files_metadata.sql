-- Issue #77 PR3: adds the Drive-API-specific columns migration 0022
-- deliberately deferred (ADR-0006 D6) — name/comment/is_sensitive/
-- folder_id have no meaning for the other purposes files rows can hold
-- (avatar, source_favicon, app_icon), only for the 'attachment'-purpose
-- rows internal/drive.Service.CreateFile writes. Plain ADD COLUMN, not a
-- -- migrate:rebuild table rebuild (see 0016's own comment on when that
-- is required): none of these touch files' existing CHECK or UNIQUE
-- constraints.
--
-- name has a NOT NULL DEFAULT '' rather than being nullable: every row
-- 0022 already created is a static app icon or similar non-Drive-API
-- purpose with no meaningful name, and misskey_dart's DriveFile.name is
-- a required wire field (docs/compat/aria-v1.5.11.md's Drive API
-- section) — internal/drive.Service always supplies a real name for
-- every row it creates, so no code path ever needs to distinguish
-- "empty on purpose" from "not set yet."
ALTER TABLE files ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN comment TEXT;
ALTER TABLE files ADD COLUMN is_sensitive INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN folder_id TEXT REFERENCES folders (id);
-- md5 is a hex-encoded MD5 digest, wire-compatibility-only (see
-- domain.File.MD5's doc comment): misskey_dart's DriveFile has a
-- required `md5` field distinct from this table's own sha256 identity
-- hash, which predates this column and keeps its original role.
ALTER TABLE files ADD COLUMN md5 TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_files_folder_id ON files (folder_id) WHERE folder_id IS NOT NULL;
