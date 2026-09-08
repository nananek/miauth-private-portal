-- Issue #77 PR5: actors.avatar_file_id (plan-77 v2 §2.5), a nullable FK
-- into the files table (migration 0024) any actor type can use — the
-- owner (via POST /api/i/update's avatarId field), and, per §2.5's own
-- note, an ActorExternalSource's fetched favicon (PR4/ADR-0008) or a
-- future ActorOpenWebUIModel avatar, without a further schema change.
-- Plain ADD COLUMN: no existing CHECK/UNIQUE constraint on actors is
-- touched.
ALTER TABLE actors ADD COLUMN avatar_file_id TEXT REFERENCES files (id);
