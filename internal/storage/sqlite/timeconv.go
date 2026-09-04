package sqlite

import (
	"database/sql"
	"time"
)

// timeLayout is the single explicit on-disk format for every timestamp
// this service persists: RFC 3339 with a fixed-width nanosecond fraction,
// always UTC. Every timestamp column is declared TEXT (not a
// driver-recognized "TIMESTAMP" type) so this conversion is always
// explicit here rather than depending on driver-version time-handling
// behavior.
//
// The fraction is fixed-width (".000000000", not time.RFC3339Nano's
// trimmed ".999999999") because SQLite's TEXT ordering, and every
// ORDER BY / row-value pagination cursor on a timestamp column in this
// package, is a plain byte-wise string comparison: RFC3339Nano's
// formatter omits the fraction entirely when it is exactly zero, so
// "...:00Z" (no fraction) sorts *after* "...:00.5Z" ('.' < 'Z') even
// though it is chronologically earlier. Always writing all nine digits
// keeps every stored timestamp the same width, so string order and time
// order agree.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders t for storage, normalizing it to UTC first so a
// caller passing a value in another location never silently changes how
// timeline ordering compares.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// formatTimePtr renders an optional timestamp column value, returning nil
// (SQL NULL) for a nil t.
func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// parseTime parses a stored timestamp back into UTC.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// parseTimePtr parses an optional stored timestamp column, returning nil
// for a SQL NULL.
func parseTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// nullableString adapts a *string domain field to a database/sql
// parameter, returning nil (SQL NULL) for a nil s.
func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// stringPtr adapts a scanned sql.NullString back to a *string domain
// field.
func stringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// nullableInt adapts a *int domain field to a database/sql parameter.
func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// intPtr adapts a scanned sql.NullInt64 back to a *int domain field.
func intPtr(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}

// boolToInt adapts a bool domain field to SQLite's 0/1 INTEGER
// representation (SQLite has no native boolean type).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
