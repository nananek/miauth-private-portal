ALTER TABLE llm_classifications ADD COLUMN priority TEXT CHECK (priority IN ('low', 'medium', 'high'));
ALTER TABLE llm_classifications ADD COLUMN notebook_candidate INTEGER NOT NULL DEFAULT 0 CHECK (notebook_candidate IN (0, 1));
ALTER TABLE llm_classifications ADD COLUMN review_candidate INTEGER NOT NULL DEFAULT 0 CHECK (review_candidate IN (0, 1));
ALTER TABLE llm_classifications ADD COLUMN unresolved INTEGER NOT NULL DEFAULT 0 CHECK (unresolved IN (0, 1));
-- Mirrors llm_generations.job_id (not unique): lets the "llm_classification"
-- job handler find whether this exact job already produced a row for an
-- entry, regardless of how much later a duplicate delivery arrives (a
-- crash between the handler committing its result and jobs.Manager
-- persisting Succeed leaves the job claimable again after its lease
-- expires). llm_classifications.id is AUTOINCREMENT, so unlike
-- llm_generations.id ("llmgen:" + job.ID) it cannot itself be the
-- deterministic idempotency key.
ALTER TABLE llm_classifications ADD COLUMN job_id TEXT REFERENCES jobs (id);

CREATE INDEX idx_llm_classifications_review_candidate ON llm_classifications (generated_at)
    WHERE is_active = 1 AND review_candidate = 1;
CREATE INDEX idx_llm_classifications_notebook_candidate ON llm_classifications (generated_at)
    WHERE is_active = 1 AND notebook_candidate = 1;
CREATE INDEX idx_llm_classifications_unresolved ON llm_classifications (generated_at)
    WHERE is_active = 1 AND unresolved = 1;
