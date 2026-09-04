package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type llmClassificationRepository struct{ q querier }

const llmClassificationSelectColumns = `SELECT id, entry_id, version, is_active, provider, model,
	prompt_version, status, error_category, summary, structured_output, prompt_tokens, completion_tokens,
	generated_at, created_at FROM llm_classifications`

func (r *llmClassificationRepository) Create(ctx context.Context, c domain.LLMClassification) (int64, error) {
	res, err := r.q.ExecContext(ctx,
		`INSERT INTO llm_classifications (entry_id, version, is_active, provider, model, prompt_version,
			status, error_category, summary, structured_output, prompt_tokens, completion_tokens,
			generated_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.EntryID, c.Version, boolToInt(c.IsActive), c.Provider, c.Model, c.PromptVersion, string(c.Status),
		nullableString(c.ErrorCategory), nullableString(c.Summary), nullableString(c.StructuredOutput),
		nullableInt(c.PromptTokens), nullableInt(c.CompletionTokens), formatTimePtr(c.GeneratedAt),
		formatTime(c.CreatedAt),
	)
	if err != nil {
		return 0, mapWriteError(err)
	}
	return res.LastInsertId()
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

	if err := row.Scan(&c.ID, &c.EntryID, &c.Version, &isActive, &c.Provider, &c.Model, &c.PromptVersion,
		&status, &errorCategory, &summary, &structuredOutput, &promptTokens, &completionTokens,
		&generatedAt, &createdAt); err != nil {
		return domain.LLMClassification{}, mapReadError(err)
	}

	c.IsActive = isActive != 0
	c.Status = domain.ClassificationStatus(status)
	c.ErrorCategory = stringPtr(errorCategory)
	c.Summary = stringPtr(summary)
	c.StructuredOutput = stringPtr(structuredOutput)
	c.PromptTokens = intPtr(promptTokens)
	c.CompletionTokens = intPtr(completionTokens)

	var err error
	if c.GeneratedAt, err = parseTimePtr(generatedAt); err != nil {
		return domain.LLMClassification{}, err
	}
	if c.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.LLMClassification{}, err
	}
	return c, nil
}
