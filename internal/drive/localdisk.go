package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Local is a Storage backed by a directory on the local filesystem
// (DRIVE_BACKEND=localdisk, the default). Every key becomes one file
// under Dir, with intermediate directories created on demand so a
// caller-chosen key like "avatars/<id>.png" nests naturally instead of
// requiring every key to be flat.
type Local struct {
	// Dir is the root directory every key is stored under. It must
	// already exist; Local never creates it (internal/config's startup
	// validation is what enforces this, so a misconfigured path fails
	// closed at startup rather than silently creating a directory in an
	// unexpected place).
	Dir string
}

// NewLocal builds a Local storage rooted at dir.
func NewLocal(dir string) *Local {
	return &Local{Dir: dir}
}

// resolvePath maps key to a path under Dir, rejecting any key that is
// not lexically confined to Dir's subtree (a "../" traversal, an
// absolute path, or a backslash) — keys are meant to be this package's
// own caller-assigned identifiers, never attacker-influenced filesystem
// paths, but this is enforced rather than merely assumed.
// filepath.IsLocal, not just filepath.Clean, is what makes this a
// rejection rather than a silent reinterpretation: Clean("/../escape")
// would harmlessly normalize to "/escape" and still resolve *inside*
// Dir, but a caller passing "../escape" has a bug worth surfacing
// rather than quietly renaming its own key.
func (l *Local) resolvePath(key string) (string, error) {
	if key == "" {
		return "", errors.New("drive: key must not be empty")
	}
	if strings.Contains(key, "\\") {
		return "", fmt.Errorf("drive: key %q contains a backslash", key)
	}
	if !filepath.IsLocal(key) {
		return "", fmt.Errorf("drive: key %q is not a local path relative to the storage root", key)
	}
	return filepath.Join(l.Dir, key), nil
}

func (l *Local) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := l.resolvePath(key)
	if err != nil {
		return err
	}
	// Dir itself must already exist (internal/config's startup
	// validation is what enforces this for the configured root, so a
	// misconfigured DRIVE_DATA_DIR fails closed at startup rather than
	// silently growing a directory tree in an unexpected place); only
	// key's own intermediate directories under Dir are created here.
	if _, err := os.Stat(l.Dir); err != nil {
		return fmt.Errorf("drive: storage root %q: %w", l.Dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("drive: create directory for key %q: %w", key, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("drive: put key %q: %w", key, ErrKeyExists)
		}
		return fmt.Errorf("drive: open key %q for write: %w", key, err)
	}
	// A failed or partial write must not leave a corrupt object behind
	// under a key future Get/Put calls will treat as valid: on any
	// error path the file is closed and removed, so callers only ever
	// observe either a complete object or none at all.
	if _, copyErr := io.CopyN(f, r, size); copyErr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("drive: write key %q: %w", key, copyErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("drive: close key %q: %w", key, closeErr)
	}
	return nil
}

func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := l.resolvePath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFound(key)
		}
		return nil, fmt.Errorf("drive: open key %q: %w", key, err)
	}
	return f, nil
}

// List walks Dir recursively, returning every regular file's path
// relative to Dir (using "/" separators regardless of host OS) as a key
// — the inverse of resolvePath. A root directory that does not exist yet
// (an otherwise-unused localdisk backend GC runs against before any file
// was ever stored) is treated as empty, not an error.
func (l *Local) List(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var keys []string
	err := filepath.WalkDir(l.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.Dir, path)
		if err != nil {
			return fmt.Errorf("drive: relativize path %q under %q: %w", path, l.Dir, err)
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("drive: list storage root %q: %w", l.Dir, err)
	}
	return keys, nil
}

func (l *Local) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := l.resolvePath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("drive: delete key %q: %w", key, err)
	}
	return nil
}
