// package artifact converts a stored backup back into a plaintext dump.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/vncwr/backwyn/internal/crypto"
	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/storage"
)

// stage identifies the failed step of materialization.
type Stage string

const (
	StageFetch    Stage = "fetch"
	StageDecrypt  Stage = "decrypt"
	StageChecksum Stage = "checksum"
	StageWrite    Stage = "write"
)

// stageerror wraps an error with the step where it failed.
type StageError struct {
	Stage Stage
	Err   error
}

func (e *StageError) Error() string { return fmt.Sprintf("%s: %v", e.Stage, e.Err) }
func (e *StageError) Unwrap() error { return e.Err }

func stageErr(s Stage, format string, args ...any) *StageError {
	return &StageError{Stage: s, Err: fmt.Errorf(format, args...)}
}

// stageof returns the stage of err.
func StageOf(err error) Stage {
	var se *StageError
	if errors.As(err, &se) {
		return se.Stage
	}
	return ""
}

// materialize streams, decrypts, and checks the sha256 checksum of an artifact.
func Materialize(ctx context.Context, store storage.Backend, m *manifest.Manifest, key []byte, dst io.Writer) error {
	rc, err := store.Get(ctx, m.ArtifactKey)
	if err != nil {
		return stageErr(StageFetch, "fetch artifact %s: %w", m.ArtifactKey, err)
	}
	defer rc.Close()

	h := sha256.New()
	if err := crypto.Decrypt(io.MultiWriter(dst, h), rc, key); err != nil {
		return stageErr(StageDecrypt, "%w", err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != m.PlaintextSHA256 {
		return stageErr(StageChecksum, "checksum mismatch: manifest %s, got %s", m.PlaintextSHA256, got)
	}
	return nil
}

// materializetemp materializes the backup into a temporary file.
func MaterializeTemp(ctx context.Context, store storage.Backend, m *manifest.Manifest, key []byte) (path string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "backwyn-"+m.ID+"-*.pgc")
	if err != nil {
		return "", func() {}, stageErr(StageWrite, "create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	discard := func() {
		// clean up on error.
		tmp.Close()
		os.Remove(tmpPath)
	}

	if err := Materialize(ctx, store, m, key, tmp); err != nil {
		discard()
		return "", func() {}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", func() {}, stageErr(StageWrite, "close temp file: %w", err)
	}
	return tmpPath, func() { os.Remove(tmpPath) }, nil
}

// load reads a manifest from storage.
func Load(ctx context.Context, store storage.Backend, id string) (*manifest.Manifest, error) {
	rc, err := store.Get(ctx, manifest.ManifestKey(id))
	if err != nil {
		return nil, fmt.Errorf("open manifest for %s: %w", id, err)
	}
	defer rc.Close()
	return manifest.Decode(rc)
}

// loadall reads all manifests from storage.
func LoadAll(ctx context.Context, store storage.Backend) ([]*manifest.Manifest, error) {
	keys, err := store.List(ctx, "manifests/")
	if err != nil {
		return nil, err
	}
	ms := make([]*manifest.Manifest, 0, len(keys))
	for _, k := range keys {
		rc, err := store.Get(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", k, err)
		}
		m, err := manifest.Decode(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", k, err)
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// save writes a manifest back to storage.
func Save(ctx context.Context, store storage.Backend, m *manifest.Manifest) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(m.Encode(pw))
	}()
	return store.Put(ctx, manifest.ManifestKey(m.ID), pr)
}
