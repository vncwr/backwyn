// package storage abstracts where artifacts and manifests are stored.
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

// backend is an object store.
type Backend interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Stat(ctx context.Context, key string) (size int64, err error)

	// delete removes key. missing keys return no error.
	Delete(ctx context.Context, key string) error
}

// local is a filesystem-backed store.
type Local struct {
	Dir string
}

// newlocal returns a local backend.
func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	return &Local{Dir: dir}, nil
}

func (l *Local) path(key string) string {
	return filepath.Join(l.Dir, filepath.FromSlash(key))
}

// put writes r to key via temp file and rename.
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

// get opens key for reading.
func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return os.Open(l.path(key))
}

// list returns sorted keys under prefix.
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

// delete removes key.
func (l *Local) Delete(ctx context.Context, key string) error {
	if err := os.Remove(l.path(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// stat returns key size in bytes.
func (l *Local) Stat(ctx context.Context, key string) (int64, error) {
	info, err := os.Stat(l.path(key))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
