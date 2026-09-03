package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

type llmGenerationRepository struct{ q querier }

const llmGenerationSelectColumns = `SELECT id, target_entry_id, result_entry_id, kind, provider, model,
	prompt_version, status, error_category, body, prompt_tokens, completion_tokens, job_id, requested_at,
	generated_at FROM llm_generations`

func (r *llmGenerationRepository) Create(ctx context.Context, g domain.LLMGeneration) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO llm_generations (id, target_entry_id, result_entry_id, kind, provider, model,
			prompt_version, status, error_category, body, prompt_tokens, completion_tokens, job_id,
			requested_at, generated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.TargetEntryID, nullableString(g.ResultEntryID), string(g.Kind), g.Provider, g.Model,
		g.PromptVersion, string(g.Status), nullableString(g.ErrorCategory), nullableString(g.Body),
		nullableInt(g.PromptTokens), nullableInt(g.CompletionTokens), nullableString(g.JobID),
		formatTime(g.RequestedAt), formatTimePtr(g.GeneratedAt),
	)
	return mapWriteError(err)
}

func (r *llmGenerationRepository) Get(ctx context.Context, id string) (domain.LLMGeneration, error) {
	row := r.q.QueryRowContext(ctx, llmGenerationSelectColumns+` WHERE id = ?`, id)
	return scanLLMGeneration(row)
}

func (r *llmGenerationRepository) ListByTarget(ctx context.Context, targetEntryID string) ([]domain.LLMGeneration, error) {
	rows, err := r.q.QueryContext(ctx,
		llmGenerationSelectColumns+` WHERE target_entry_id = ? ORDER BY requested_at`, targetEntryID)
	if err != nil {
		return nil, fmt.Errorf("list generations by target: %w", err)
	}
	defer rows.Close()

	var generations []domain.LLMGeneration
	for rows.Next() {
		g, err := scanLLMGeneration(rows)
		if err != nil {
			return nil, err
		}
		generations = append(generations, g)
	}
	return generations, rows.Err()
}

// Complete atomically marks a pending generation as complete and links
// it to resultEntryID. Call together with EntryRepository.Create for
// resultEntryID inside the same UnitOfWork transaction.
func (r *llmGenerationRepository) Complete(ctx context.Context, id, resultEntryID, body string, promptTokens, completionTokens *int, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE llm_generations SET status = 'complete', result_entry_id = ?, body = ?, prompt_tokens = ?,
			completion_tokens = ?, generated_at = ?
		 WHERE id = ? AND status = 'pending'`,
		resultEntryID, body, nullableInt(promptTokens), nullableInt(completionTokens), formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func (r *llmGenerationRepository) Fail(ctx context.Context, id, errorCategory string, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE llm_generations SET status = 'failed', error_category = ?, generated_at = ?
		 WHERE id = ? AND status = 'pending'`,
		errorCategory, formatTime(at), id,
	)
	if err != nil {
		return mapWriteError(err)
	}
	return requireRowAffectedConflict(res)
}

func scanLLMGeneration(row rowScanner) (domain.LLMGeneration, error) {
	var g domain.LLMGeneration
	var resultEntryID sql.NullString
	var kind, status string
	var errorCategory, body, jobID sql.NullString
	var promptTokens, completionTokens sql.NullInt64
	var requestedAt string
	var generatedAt sql.NullString

	if err := row.Scan(&g.ID, &g.TargetEntryID, &resultEntryID, &kind, &g.Provider, &g.Model,
		&g.PromptVersion, &status, &errorCategory, &body, &promptTokens, &completionTokens, &jobID,
		&requestedAt, &generatedAt); err != nil {
		return domain.LLMGeneration{}, mapReadError(err)
	}

	g.ResultEntryID = stringPtr(resultEntryID)
	g.Kind = domain.GenerationKind(kind)
	g.Status = domain.GenerationStatus(status)
	g.ErrorCategory = stringPtr(errorCategory)
	g.Body = stringPtr(body)
	g.PromptTokens = intPtr(promptTokens)
	g.CompletionTokens = intPtr(completionTokens)
	g.JobID = stringPtr(jobID)

	var err error
	if g.RequestedAt, err = parseTime(requestedAt); err != nil {
		return domain.LLMGeneration{}, err
	}
	if g.GeneratedAt, err = parseTimePtr(generatedAt); err != nil {
		return domain.LLMGeneration{}, err
	}
	return g, nil
}
