ALTER TABLE llm_classifications ADD COLUMN priority TEXT CHECK (priority IN ('low', 'medium', 'high'));
ALTER TABLE llm_classifications ADD COLUMN notebook_candidate INTEGER NOT NULL DEFAULT 0 CHECK (notebook_candidate IN (0, 1));
ALTER TABLE llm_classifications ADD COLUMN review_candidate INTEGER NOT NULL DEFAULT 0 CHECK (review_candidate IN (0, 1));
ALTER TABLE llm_classifications ADD COLUMN unresolved INTEGER NOT NULL DEFAULT 0 CHECK (unresolved IN (0, 1));

CREATE INDEX idx_llm_classifications_review_candidate ON llm_classifications (generated_at)
    WHERE is_active = 1 AND review_candidate = 1;
CREATE INDEX idx_llm_classifications_notebook_candidate ON llm_classifications (generated_at)
    WHERE is_active = 1 AND notebook_candidate = 1;
CREATE INDEX idx_llm_classifications_unresolved ON llm_classifications (generated_at)
    WHERE is_active = 1 AND unresolved = 1;
