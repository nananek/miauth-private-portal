package sqlite

import "testing"

func TestChecker_SucceedsWhileOpen(t *testing.T) {
	db := newTestDB(t)
	if err := db.Checker().Check(t.Context()); err != nil {
		t.Errorf("Check() = %v, want nil", err)
	}
}

func TestChecker_FailsAfterClose(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := Open(t.Context(), Config{Path: path, BusyTimeout: 0, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	checker := db.Checker()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := checker.Check(t.Context()); err == nil {
		t.Error("expected Check() to fail after the database is closed")
	}
}

func TestChecker_Name(t *testing.T) {
	db := newTestDB(t)
	if got, want := db.Checker().Name(), "sqlite"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}
