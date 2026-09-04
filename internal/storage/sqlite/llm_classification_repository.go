package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type llmClassificationRepository struct{ q querier }

const llmClassificationSelectColumns = `SELECT id, entry_id, version, is_active, provider, model,
	prompt_version, status, error_category, summary, structured_output, prompt_tokens, completion_tokens,
	generated_at, created_at, priority, notebook_candidate, review_candidate, unresolved
	FROM llm_classifications`

func (r *llmClassificationRepository) Create(ctx context.Context, c domain.LLMClassification) (int64, error) {
	res, err := r.q.ExecContext(ctx,
		`INSERT INTO llm_classifications (entry_id, version, is_active, provider, model, prompt_version,
			status, error_category, summary, structured_output, prompt_tokens, completion_tokens,
			generated_at, created_at, priority, notebook_candidate, review_candidate, unresolved)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.EntryID, c.Version, boolToInt(c.IsActive), c.Provider, c.Model, c.PromptVersion, string(c.Status),
		nullableString(c.ErrorCategory), nullableString(c.Summary), nullableString(c.StructuredOutput),
		nullableInt(c.PromptTokens), nullableInt(c.CompletionTokens), formatTimePtr(c.GeneratedAt),
		formatTime(c.CreatedAt), nullableString(c.Priority), boolToInt(c.NotebookCandidate),
		boolToInt(c.ReviewCandidate), boolToInt(c.Unresolved),
	)
	if err != nil {
		return 0, mapWriteError(err)
	}
	return res.LastInsertId()
}

// Complete transitions entryID's version from pending to complete,
// mirroring LLMGenerationRepository.Complete's WHERE ... AND status =
// 'pending' guard so a replayed job handler delivery cannot double-apply
// a result.
func (r *llmClassificationRepository) Complete(ctx context.Context, entryID string, version int, summary, structuredOutput string,
	priority *string, notebookCandidate, reviewCandidate, unresolved bool,
	promptTokens, completionTokens *int, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE llm_classifications SET status = 'complete', summary = ?, structured_output = ?,
			priority = ?, notebook_candidate = ?, review_candidate = ?, unresolved = ?,
			prompt_tokens = ?, completion_tokens = ?, generated_at = ?
		 WHERE entry_id = ? AND version = ? AND status = 'pending'`,
		summary, structuredOutput, nullableString(priority), boolToInt(notebookCandidate),
		boolToInt(reviewCandidate), boolToInt(unresolved), nullableInt(promptTokens),
		nullableInt(completionTokens), formatTime(at), entryID, version,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

// Fail transitions entryID's version from pending to failed.
func (r *llmClassificationRepository) Fail(ctx context.Context, entryID string, version int, errorCategory string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE llm_classifications SET status = 'failed', error_category = ?, generated_at = ?
		 WHERE entry_id = ? AND version = ? AND status = 'pending'`,
		errorCategory, formatTime(at), entryID, version,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *llmClassificationRepository) ListReviewCandidates(ctx context.Context, limit int) ([]domain.LLMClassification, error) {
	return r.listFlagged(ctx, "review_candidate", limit)
}

func (r *llmClassificationRepository) ListNotebookCandidates(ctx context.Context, limit int) ([]domain.LLMClassification, error) {
	return r.listFlagged(ctx, "notebook_candidate", limit)
}

func (r *llmClassificationRepository) ListUnresolved(ctx context.Context, limit int) ([]domain.LLMClassification, error) {
	return r.listFlagged(ctx, "unresolved", limit)
}

// listFlagged backs the three ListXxxCandidates methods above: flagColumn
// is always one of this file's own hardcoded column names, never
// caller-supplied input, so building the query with fmt.Sprintf here
// cannot introduce a SQL-injection path.
func (r *llmClassificationRepository) listFlagged(ctx context.Context, flagColumn string, limit int) ([]domain.LLMClassification, error) {
	rows, err := r.q.QueryContext(ctx,
		fmt.Sprintf(llmClassificationSelectColumns+` WHERE is_active = 1 AND %s = 1 ORDER BY generated_at DESC LIMIT ?`, flagColumn),
		limit)
	if err != nil {
		return nil, fmt.Errorf("list classifications by %s: %w", flagColumn, err)
	}
	defer rows.Close()

	var classifications []domain.LLMClassification
	for rows.Next() {
		c, err := scanLLMClassification(rows)
		if err != nil {
			return nil, err
		}
		classifications = append(classifications, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate classifications by %s: %w", flagColumn, err)
	}

	for i, c := range classifications {
		full, err := r.attachTagsAndRelated(ctx, c)
		if err != nil {
			return nil, err
		}
		classifications[i] = full
	}
	return classifications, nil
}

// Activate marks entryID's classification at version active and
// deactivates every other version for that entry. The two UPDATEs only
// commit together when called inside UnitOfWork.WithinTx; called
// standalone, a failure between them can leave no version active, which
// is safe (GetActive then reports ErrNotFound) but not atomic.
func (r *llmClassificationRepository) Activate(ctx context.Context, entryID string, version int) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE llm_classifications SET is_active = 0 WHERE entry_id = ? AND is_active = 1`, entryID,
	); err != nil {
		return mapWriteError(err)
	}
	res, err := r.q.ExecContext(ctx,
		`UPDATE llm_classifications SET is_active = 1 WHERE entry_id = ? AND version = ?`, entryID, version,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffected(res)
}

func (r *llmClassificationRepository) GetActive(ctx context.Context, entryID string) (domain.LLMClassification, error) {
	row := r.q.QueryRowContext(ctx,
		llmClassificationSelectColumns+` WHERE entry_id = ? AND is_active = 1`, entryID)
	c, err := scanLLMClassification(row)
	if err != nil {
		return domain.LLMClassification{}, err
	}
	return r.attachTagsAndRelated(ctx, c)
}

func (r *llmClassificationRepository) ListVersions(ctx context.Context, entryID string) ([]domain.LLMClassification, error) {
	rows, err := r.q.QueryContext(ctx,
		llmClassificationSelectColumns+` WHERE entry_id = ? ORDER BY version`, entryID)
	if err != nil {
		return nil, fmt.Errorf("list classification versions: %w", err)
	}
	defer rows.Close()

	var classifications []domain.LLMClassification
	for rows.Next() {
		c, err := scanLLMClassification(rows)
		if err != nil {
			return nil, err
		}
		classifications = append(classifications, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate classification versions: %w", err)
	}

	for i, c := range classifications {
		full, err := r.attachTagsAndRelated(ctx, c)
		if err != nil {
			return nil, err
		}
		classifications[i] = full
	}
	return classifications, nil
}

func (r *llmClassificationRepository) AddTag(ctx context.Context, classificationID int64, tag string) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO llm_classification_tags (classification_id, tag) VALUES (?, ?)`, classificationID, tag)
	return mapWriteError(err)
}

func (r *llmClassificationRepository) AddRelatedEntry(ctx context.Context, classificationID int64, relatedEntryID string) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO llm_classification_related_entries (classification_id, related_entry_id) VALUES (?, ?)`,
		classificationID, relatedEntryID)
	return mapWriteError(err)
}

func (r *llmClassificationRepository) attachTagsAndRelated(ctx context.Context, c domain.LLMClassification) (domain.LLMClassification, error) {
	tagRows, err := r.q.QueryContext(ctx,
		`SELECT tag FROM llm_classification_tags WHERE classification_id = ? ORDER BY tag`, c.ID)
	if err != nil {
		return domain.LLMClassification{}, fmt.Errorf("list classification tags: %w", err)
	}
	defer tagRows.Close()
	for tagRows.Next() {
		var tag string
		if err := tagRows.Scan(&tag); err != nil {
			return domain.LLMClassification{}, fmt.Errorf("scan classification tag: %w", err)
		}
		c.Tags = append(c.Tags, tag)
	}
	if err := tagRows.Err(); err != nil {
		return domain.LLMClassification{}, fmt.Errorf("iterate classification tags: %w", err)
	}

	relatedRows, err := r.q.QueryContext(ctx,
		`SELECT related_entry_id FROM llm_classification_related_entries WHERE classification_id = ? ORDER BY related_entry_id`, c.ID)
	if err != nil {
		return domain.LLMClassification{}, fmt.Errorf("list related entries: %w", err)
	}
	defer relatedRows.Close()
	for relatedRows.Next() {
		var relatedID string
		if err := relatedRows.Scan(&relatedID); err != nil {
			return domain.LLMClassification{}, fmt.Errorf("scan related entry: %w", err)
		}
		c.RelatedEntryIDs = append(c.RelatedEntryIDs, relatedID)
	}
	if err := relatedRows.Err(); err != nil {
		return domain.LLMClassification{}, fmt.Errorf("iterate related entries: %w", err)
	}

	return c, nil
}

func scanLLMClassification(row rowScanner) (domain.LLMClassification, error) {
	var c domain.LLMClassification
	var isActive int
	var status string
	var errorCategory, summary, structuredOutput sql.NullString
	var promptTokens, completionTokens sql.NullInt64
	var generatedAt sql.NullString
	var createdAt string
	var priority sql.NullString
	var notebookCandidate, reviewCandidate, unresolved int

	if err := row.Scan(&c.ID, &c.EntryID, &c.Version, &isActive, &c.Provider, &c.Model, &c.PromptVersion,
		&status, &errorCategory, &summary, &structuredOutput, &promptTokens, &completionTokens,
		&generatedAt, &createdAt, &priority, &notebookCandidate, &reviewCandidate, &unresolved); err != nil {
		return domain.LLMClassification{}, mapReadError(err)
	}

	c.IsActive = isActive != 0
	c.Status = domain.ClassificationStatus(status)
	c.ErrorCategory = stringPtr(errorCategory)
	c.Summary = stringPtr(summary)
	c.StructuredOutput = stringPtr(structuredOutput)
	c.PromptTokens = intPtr(promptTokens)
	c.CompletionTokens = intPtr(completionTokens)
	c.Priority = stringPtr(priority)
	c.NotebookCandidate = notebookCandidate != 0
	c.ReviewCandidate = reviewCandidate != 0
	c.Unresolved = unresolved != 0

	var err error
	if c.GeneratedAt, err = parseTimePtr(generatedAt); err != nil {
		return domain.LLMClassification{}, err
	}
	if c.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.LLMClassification{}, err
	}
	return c, nil
}
