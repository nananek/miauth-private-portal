package drive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sort"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// testStorageContract exercises the behavior every Storage
// implementation must share, so Local and S3 (storage_test.go, s3_test.go)
// run the exact same assertions against a fresh instance each provides.
func testStorageContract(t *testing.T, newStorage func(t *testing.T) Storage) {
	t.Helper()

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		s := newStorage(t)
		ctx := context.Background()
		content := []byte("hello drive")
		if err := s.Put(ctx, "a/b/c.bin", bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("Put: %v", err)
		}
		rc, err := s.Get(ctx, "a/b/c.bin")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("got %q, want %q", got, content)
		}
	})

	t.Run("GetMissingKeyReturnsNotFound", func(t *testing.T) {
		s := newStorage(t)
		_, err := s.Get(context.Background(), "does/not/exist")
		if err == nil {
			t.Fatal("expected an error for a missing key")
		}
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("err = %v, want it to satisfy errors.Is(err, domain.ErrNotFound)", err)
		}
	})

	t.Run("PutExistingKeyFails", func(t *testing.T) {
		s := newStorage(t)
		ctx := context.Background()
		if err := s.Put(ctx, "dup", bytes.NewReader([]byte("first")), 5); err != nil {
			t.Fatalf("first Put: %v", err)
		}
		err := s.Put(ctx, "dup", bytes.NewReader([]byte("second")), 6)
		if err == nil {
			t.Fatal("expected the second Put of the same key to fail")
		}
		if !errors.Is(err, ErrKeyExists) {
			t.Errorf("err = %v, want it to satisfy errors.Is(err, ErrKeyExists)", err)
		}
		// The original content must survive the rejected overwrite.
		rc, err := s.Get(ctx, "dup")
		if err != nil {
			t.Fatalf("Get after rejected overwrite: %v", err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if string(got) != "first" {
			t.Errorf("content after rejected overwrite = %q, want %q", got, "first")
		}
	})

	t.Run("DeleteThenGetIsNotFound", func(t *testing.T) {
		s := newStorage(t)
		ctx := context.Background()
		if err := s.Put(ctx, "to-delete", bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := s.Delete(ctx, "to-delete"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get(ctx, "to-delete"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Get after Delete: err = %v, want domain.ErrNotFound", err)
		}
	})

	t.Run("DeleteMissingKeyIsIdempotent", func(t *testing.T) {
		s := newStorage(t)
		if err := s.Delete(context.Background(), "never-existed"); err != nil {
			t.Errorf("Delete of a missing key should be a no-op, got: %v", err)
		}
	})

	t.Run("ListReturnsEveryPutKey", func(t *testing.T) {
		s := newStorage(t)
		ctx := context.Background()
		want := []string{"a.bin", "dir/b.bin", "dir/nested/c.bin"}
		for _, k := range want {
			if err := s.Put(ctx, k, bytes.NewReader([]byte("x")), 1); err != nil {
				t.Fatalf("Put %q: %v", k, err)
			}
		}
		got, err := s.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("List = %v, want %v", got, want)
		}
	})

	t.Run("ListEmptyStorageReturnsEmpty", func(t *testing.T) {
		s := newStorage(t)
		got, err := s.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("List = %v, want empty", got)
		}
	})

	t.Run("ListExcludesDeletedKeys", func(t *testing.T) {
		s := newStorage(t)
		ctx := context.Background()
		if err := s.Put(ctx, "keep", bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("Put keep: %v", err)
		}
		if err := s.Put(ctx, "gone", bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("Put gone: %v", err)
		}
		if err := s.Delete(ctx, "gone"); err != nil {
			t.Fatalf("Delete gone: %v", err)
		}
		got, err := s.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0] != "keep" {
			t.Errorf("List = %v, want [keep]", got)
		}
	})
}
