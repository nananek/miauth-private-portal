package drive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocal_StorageContract(t *testing.T) {
	testStorageContract(t, func(t *testing.T) Storage {
		return NewLocal(t.TempDir())
	})
}

func TestLocal_PutCreatesNestedDirectories(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	if err := l.Put(context.Background(), "a/b/c/d.bin", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "b", "c", "d.bin")); err != nil {
		t.Errorf("expected nested file to exist: %v", err)
	}
}

func TestLocal_RejectsPathTraversal(t *testing.T) {
	l := NewLocal(t.TempDir())
	ctx := context.Background()
	for _, key := range []string{
		"../escape",
		"a/../../escape",
		"/etc/passwd",
		`a\..\..\escape`,
		"",
	} {
		if err := l.Put(ctx, key, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Errorf("Put(%q) should have been rejected as a traversal attempt", key)
		}
		if _, err := l.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) should have been rejected as a traversal attempt", key)
		}
	}
}

func TestLocal_FailedWriteLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	// size (10) exceeds what the reader actually provides, so io.CopyN
	// inside Put fails partway through.
	err := l.Put(context.Background(), "partial", bytes.NewReader([]byte("short")), 10)
	if err == nil {
		t.Fatal("expected Put to fail when the reader is shorter than size")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "partial")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("expected no partial file to remain, stat err = %v", statErr)
	}
}
