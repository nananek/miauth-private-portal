-- Upstream Misskey owner verification was superseded by host-local
-- SSH+CLI approval (ADR-0002). Drop only its credential/session records;
-- local actors, local sessions, API tokens, and user content remain.
DROP TABLE upstream_tokens;
DROP TABLE owner_bindings;
DROP TABLE miauth_upstream_sessions;
DROP TABLE bootstrap_gates;

-- miauth_local_sessions.state is intentionally retained for now. It is
-- covered by a UNIQUE constraint and SQLite cannot DROP such a column
-- without rebuilding the table. New code treats it as an inert storage
-- compatibility value, not as an authentication credential.
