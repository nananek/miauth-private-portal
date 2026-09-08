-- Issue #77 PR6: entry_files is POST /api/notes/create's fileIds
-- many-to-many join between an entry and the Drive files attached to
-- it. PR0's trace (docs/compat/aria-v1.5.11.md's "Drive API and note
-- attachments" section) found a single drive file can be attached to
-- more than one note (AttachedNotesNotifier exists specifically to
-- answer "which notes is this file attached to" as a one-to-many
-- query), correcting plan-77 v2's tentative "1:1 might be enough"
-- framing — so this must be a genuine many-to-many join, not a 1:1
-- pointer on either table.
--
-- position preserves Aria's own compose-order attachment order (the
-- same PR0 trace: fileIds is order-preserving, matching
-- AttachesNotifier.reorder's user-visible order). Rows are written at
-- most once per entry, at creation time, inside the same transaction
-- that creates the entries row — there is no update path (notes/update
-- is not implemented).
--
-- No ON DELETE behavior is declared on either foreign key: entries are
-- never hard-deleted (notes/delete maps onto hidden_at, per
-- docs/decisions/0004-note-delete-as-hide.md), so entry_id never dangles;
-- files can be hard-deleted (drive/files/delete), so
-- internal/drive.Service.DeleteFile checks CountByFile first and rejects
-- deleting a still-attached file (ErrFileAttached) rather than letting
-- SQLite's foreign-key enforcement surface a raw constraint error, or
-- silently cascading the delete (AGENTS.md: unsupported/unsafe
-- operations must fail explicitly).
CREATE TABLE entry_files (
    entry_id TEXT NOT NULL REFERENCES entries (id),
    file_id TEXT NOT NULL REFERENCES files (id),
    position INTEGER NOT NULL,
    PRIMARY KEY (entry_id, file_id)
);

-- Backs ListFilesByEntry's position-ordered read.
CREATE INDEX idx_entry_files_entry_id ON entry_files (entry_id, position);
-- Backs CountByFile/ListEntriesByFile's "which entries reference this
-- file" queries.
CREATE INDEX idx_entry_files_file_id ON entry_files (file_id);
