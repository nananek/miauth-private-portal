-- Issue #77 PR4 (ADR-0008): the "1 external identity = 1 actor row"
-- columns an RSS-kind external_sources row needs. Plain ADD COLUMN, not
-- a -- migrate:rebuild table rebuild: none of these touch this table's
-- existing UNIQUE(kind, uri) or any CHECK constraint.
--
-- All three are NULL for an imap-kind row: ADR-0008's design-A host
-- display is RSS-only (a spoofable email-sender-domain equivalent for
-- IMAP was explicitly rejected as a materially different phishing risk,
-- not merely deferred) — imap sources keep projecting as the shared
-- system actor, unchanged.
ALTER TABLE external_sources ADD COLUMN actor_id TEXT REFERENCES actors (id);
-- Misskey username character set (internal/config's ownerUsernamePattern:
-- ASCII letters, digits, underscore), owner-settable at feed
-- registration time (RSS_FEED_URLS, see internal/config's parsing) or
-- auto-derived from the feed's own host when left unset.
ALTER TABLE external_sources ADD COLUMN username TEXT;
-- The feed's own real origin host (net/url.Parse(uri).Host), computed
-- once when the source is first registered and never recomputed even if
-- a later config edit changes uri's host — re-registering under a new
-- URI is a new source, not an edit to this one (UNIQUE(kind, uri)).
ALTER TABLE external_sources ADD COLUMN host TEXT;

CREATE INDEX idx_external_sources_actor_id ON external_sources (actor_id) WHERE actor_id IS NOT NULL;

-- Two different sources must never present as the same @username@host
-- pair — Aria's own UserLite model has no third disambiguating field a
-- client could use to tell them apart.
CREATE UNIQUE INDEX idx_external_sources_host_username ON external_sources (host, username)
    WHERE host IS NOT NULL AND username IS NOT NULL;
