package sqlite

import "context"

// Checker is the health.Checker (internal/health.Checker) this service
// registers so /readyz reflects SQLite connectivity. It is defined here,
// not in internal/health, so internal/health never needs to depend on
// this package or on database/sql; Go's structural interface satisfaction
// is enough for cmd/server to register it with health.Registry.
type Checker struct {
	db *DB
}

// Checker returns the health.Checker for d.
func (d *DB) Checker() *Checker {
	return &Checker{db: d}
}

func (c *Checker) Name() string { return "sqlite" }

// Check runs "SELECT 1", a real round trip through the query engine,
// rather than PRAGMA integrity_check: that check is a full-database scan,
// too expensive to run on every readiness poll.
func (c *Checker) Check(ctx context.Context) error {
	var one int
	return c.db.sqlDB.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
