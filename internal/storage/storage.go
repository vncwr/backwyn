// package storage abstracts where backup artifacts and manifests live.
// Backend is small so new implementations need no pipeline changes.
package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Backend is a content-addressable-ish object store keyed by string paths.
type Backend interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Stat(ctx context.Context, key string) (size int64, err error)

	// Delete removes key. a missing key is not an error, so pruning is idempotent.
	Delete(ctx context.Context, key string) error
}

// Local is a filesystem-backed store rooted at Dir.
type Local struct {
	Dir string
}

// NewLocal returns a Local backend, creating the root directory if needed.
func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	return &Local{Dir: dir}, nil
}

func (l *Local) path(key string) string {
	return filepath.Join(l.Dir, filepath.FromSlash(key))
}

// Put writes r to key via temp file + rename: readers never see a partial write.
func (l *Local) Put(ctx context.Context, key string, r io.Reader) error {
	dst := l.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename below succeeded

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// Get opens key for reading.
func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return os.Open(l.path(key))
}

// List returns keys (slash-separated, relative to Dir) under prefix, sorted.
func (l *Local) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.Walk(l.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.Dir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete removes key, treating an already-absent key as success.
func (l *Local) Delete(ctx context.Context, key string) error {
	if err := os.Remove(l.path(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// Stat returns the size in bytes of key.
func (l *Local) Stat(ctx context.Context, key string) (int64, error) {
	info, err := os.Stat(l.path(key))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
