package jobs

import (
	"context"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// Handler executes one claimed job. A nil error means success. Handlers must
// honor ctx cancellation so lease loss and bounded shutdown can stop work.
type Handler func(ctx context.Context, job domain.Job) error

// PermanentError marks a classified error for which retrying cannot help.
type PermanentError struct {
	err error
}

// Permanent marks err as non-retryable. A nil error remains nil so callers can
// safely write "return Permanent(err)" without manufacturing a failure.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{err: err}
}

func (e *PermanentError) Error() string { return e.err.Error() }
func (e *PermanentError) Unwrap() error { return e.err }
