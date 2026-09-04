-- Issue #11: fetch-cursor and observability columns for periodic
-- external-source polling (RSS/Atom, and later IMAP). cursor is an
-- adapter-opaque JSON string (see internal/ingest/rss's ETag/
-- Last-Modified cursor); last_fetched_at/last_error/consecutive_failures
-- let an operator see a source's health without a separate admin
-- surface, mirroring how jobs.last_error already works for durable jobs.
ALTER TABLE external_sources ADD COLUMN cursor TEXT;
ALTER TABLE external_sources ADD COLUMN last_fetched_at TEXT;
ALTER TABLE external_sources ADD COLUMN last_error TEXT;
ALTER TABLE external_sources ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
