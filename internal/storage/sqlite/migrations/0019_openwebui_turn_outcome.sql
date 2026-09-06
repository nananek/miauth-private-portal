-- Issue #53 (OWUI-B): per-turn outcome category and provenance.
--
-- failure_category is a local classification (auth_failed,
-- contract_failed, ...), never provider error text: ADR-0005 D6 requires
-- that text be discarded, since the observed instance echoes upstream
-- credentials into it verbatim. There is deliberately no CHECK pinning
-- the set of values, the same choice already made for provider_status
-- and jobs.job_type: new categories are a Go change (see
-- internal/domain's FailureCategory* constants), not a schema change.
--
-- prompt_tokens/completion_tokens/finish_reason are the provider's own
-- accounting metadata for a succeeded turn; nothing here orders, bounds,
-- or authorizes by them.
ALTER TABLE openwebui_turn_links ADD COLUMN failure_category TEXT;
ALTER TABLE openwebui_turn_links ADD COLUMN prompt_tokens INTEGER;
ALTER TABLE openwebui_turn_links ADD COLUMN completion_tokens INTEGER;
ALTER TABLE openwebui_turn_links ADD COLUMN finish_reason TEXT;
-- When the most recent provider attempt was recorded as starting
-- (BeginAttempt, called before the provider is ever called, so a crash
-- or lease expiry afterward is distinguishable from a never-attempted
-- turn) and when the turn's status last became terminal
-- (TurnProviderStatus.IsTerminal, written by RecordOutcome).
ALTER TABLE openwebui_turn_links ADD COLUMN last_attempt_at TEXT;
ALTER TABLE openwebui_turn_links ADD COLUMN completed_at TEXT;

-- Lets owner-facing recovery tooling (Issue #53's openwebuictl) list
-- links by state without a table scan.
CREATE INDEX idx_openwebui_links_state ON openwebui_conversation_links (state, created_at, id);
