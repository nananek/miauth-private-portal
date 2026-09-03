// Package sqlite is the only package in this service allowed to import
// database/sql or modernc.org/sqlite. It implements every
// internal/domain repository interface, so SQLite-specific types and SQL
// stay out of domain/use-case code and a future PostgreSQL adapter
// (Issue #15) can implement the same domain interfaces without any
// domain-level change.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// Config configures the SQLite connection this service opens.
type Config struct {
	// Path is the SQLite database file path. Its parent directory is
	// created if it does not already exist.
	Path string
	// BusyTimeout bounds how long SQLite retries internally on
	// SQLITE_BUSY before returning an error, instead of failing a
	// request immediately under writer contention.
	BusyTimeout time.Duration
	// MaxOpenConns bounds the connection pool. WAL journal mode (always
	// enabled; see Open) allows concurrent readers alongside a single
	// writer, so this may safely be greater than 1.
	MaxOpenConns int
}

// DB wraps the opened database, plus the standalone (non-transactional)
// domain.Repos most callers use directly.
type DB struct {
	sqlDB *sql.DB
	domain.Repos
}

// Open opens (creating if necessary) the SQLite database at cfg.Path.
//
// foreign_keys, busy_timeout, and journal_mode=WAL are applied through the
// connection DSN rather than as a one-time PRAGMA statement after Open:
// database/sql can open additional physical connections later (concurrent
// HTTP handlers, and eventually Issue #8's worker), and a PRAGMA run once
// on the first connection would never reach those. DSN-embedded pragmas
// are re-applied by the driver to every connection it opens instead. See
// docs/operations/configuration.md.
//
// foreign_keys and journal_mode=WAL are always on; they are correctness
// requirements, not operator-configurable settings.
//
// _txlock=immediate makes every BEGIN (including WithinTx's) take
// SQLite's write lock up front instead of only on the transaction's first
// write. Without it a transaction that reads through one repository and
// writes through another (exactly the composition WithinTx exists for)
// can fail its read-to-write lock upgrade with SQLITE_BUSY_SNAPSHOT under
// concurrent access, an error busy_timeout's retry cannot recover from
// because the transaction's read snapshot is already stale by then.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if dir := filepath.Dir(cfg.Path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf(
		"file:%s?_foreign_keys=1&_busy_timeout=%d&_journal_mode=WAL&_txlock=immediate",
		cfg.Path, cfg.BusyTimeout.Milliseconds(),
	)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
		// Otherwise database/sql's default of 2 idle connections gets
		// closed back down after any burst above 2 concurrent callers,
		// forcing the next burst to pay for a fresh physical connection
		// (and its DSN pragmas) instead of reusing a pooled one.
		sqlDB.SetMaxIdleConns(cfg.MaxOpenConns)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}

	return &DB{sqlDB: sqlDB, Repos: newRepos(sqlDB)}, nil
}

// Close closes the underlying database connection pool.
func (d *DB) Close() error {
	return d.sqlDB.Close()
}
