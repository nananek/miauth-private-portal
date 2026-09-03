package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOpen_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "test.db")
	db, err := Open(t.Context(), Config{Path: path, BusyTimeout: time.Second, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file was not created: %v", err)
	}
}

func TestOpen_InvalidPathFailsFast(t *testing.T) {
	// A path whose parent segment is a plain file, not a directory,
	// cannot have its parent directory created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Open(t.Context(), Config{Path: filepath.Join(blocker, "test.db"), BusyTimeout: time.Second, MaxOpenConns: 1})
	if err == nil {
		t.Fatal("expected an error opening a database under a non-directory path")
	}
}

// TestOpen_PragmasApplyToEveryPooledConnection backs the design in
// Open's doc comment: foreign_keys and journal_mode are embedded in the
// DSN specifically because database/sql can open more than one physical
// connection, and a one-time PRAGMA after Open would never reach any
// connection opened later. This forces MaxOpenConns distinct connections
// open at once (each holds its own transaction concurrently) and checks
// every one of them.
func TestOpen_PragmasApplyToEveryPooledConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(t.Context(), Config{Path: path, BusyTimeout: time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	const n = 4
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			tx, err := db.sqlDB.BeginTx(t.Context(), nil)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = tx.Rollback() }()

			var foreignKeys, journalMode string
			if err := tx.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
				errs <- err
				return
			}
			if err := tx.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journalMode); err != nil {
				errs <- err
				return
			}
			if foreignKeys != "1" {
				errs <- fmt.Errorf("foreign_keys = %q, want 1", foreignKeys)
				return
			}
			if journalMode != "wal" {
				errs <- fmt.Errorf("journal_mode = %q, want wal", journalMode)
				return
			}

			// Hold the connection briefly so all n goroutines are forced
			// to acquire distinct physical connections concurrently
			// instead of serializing through one reused connection.
			time.Sleep(50 * time.Millisecond)
			errs <- nil
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
