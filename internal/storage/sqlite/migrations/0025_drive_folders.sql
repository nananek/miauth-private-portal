-- Issue #77 PR3: the folders table backing internal/drive.Service's
-- Misskey-compatible drive/folders/* endpoints. PR0's investigation
-- (docs/compat/aria-v1.5.11.md's "Drive API and note attachments"
-- section) found folder support is not optional to skip: Aria's drive
-- screen actively uses folder listing, creation, deletion, rename, and
-- move, so this is a real table, not a "minimal or unsupported"
-- placeholder plan-77 v2 originally floated before that trace.
--
-- files.folder_id (migration 0024, added after this table exists so its
-- REFERENCES target is valid) is the only other table pointing at this
-- one; nothing here points back at files.
CREATE TABLE folders (
    id TEXT PRIMARY KEY,
    -- Unlike files.owner_actor_id, this is never NULL: a folder only
    -- ever exists because a Drive API caller created it.
    owner_actor_id TEXT NOT NULL REFERENCES actors (id),
    name TEXT NOT NULL,
    -- NULL for a folder at Drive's root. internal/drive.Service enforces
    -- that a folder can never become its own descendant's child; this
    -- table has no constraint that could express that cycle check
    -- itself.
    parent_id TEXT REFERENCES folders (id),
    created_at TEXT NOT NULL
);

CREATE INDEX idx_folders_owner_parent ON folders (owner_actor_id, parent_id);
