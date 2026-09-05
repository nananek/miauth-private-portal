-- Issue #23 PR1: owner profile self-edit via POST /api/i/update. Only
-- display_name is mutable through that endpoint; username has no wire
-- path in either Aria or misskey_dart (see docs/compat/aria-v1.5.11.md's
-- "POST /api/i/update" section) and remains fixed by the OWNER_USERNAME
-- config value, so no username column is added here.
ALTER TABLE actors ADD COLUMN display_name TEXT;
