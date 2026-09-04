// Package jobs runs durable, leased, at-least-once background work.
//
// Enqueuers must choose a deterministic idempotency key for each logical
// request (for example, "classify:" + entryID + ":" + promptVersion) and
// treat domain.ErrConflict as "already scheduled". A lease prevents the same
// job row from being handed to two healthy workers concurrently, but a process
// can still stop after a handler's side effect and before Succeed is persisted.
// Handlers must therefore make their own result writes idempotent before they
// return success; user-authored source data must never be overwritten by a
// replayed handler.
//
// Job payloads are untrusted and are never written to worker logs. Handlers
// classify invalid or otherwise unrecoverable input with Permanent so it
// reaches the failed terminal state immediately. Retryable failures exhaust
// into dead instead, preserving the distinction for operators.
package jobs
