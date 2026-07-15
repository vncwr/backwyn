package artifact_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/vncwr/backwyn/internal/crypto"
	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/storage"

	"github.com/vncwr/backwyn/internal/artifact"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

// seed stores plaintext as an encrypted artifact plus its manifest, mirroring
// what internal/backup writes, and returns the manifest.
func seed(t *testing.T, s storage.Backend, plaintext, key []byte) *manifest.Manifest {
	t.Helper()
	ctx := context.Background()

	var ct bytes.Buffer
	if err := crypto.Encrypt(&ct, bytes.NewReader(plaintext), key); err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	sum := sha256.Sum256(plaintext)
	m := &manifest.Manifest{
		SchemaVersion:   1,
		ID:              "20260715T120000Z",
		CreatedAt:       time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		ArtifactKey:     "artifacts/20260715T120000Z.dump.enc",
		PlaintextSize:   int64(len(plaintext)),
		PlaintextSHA256: hex.EncodeToString(sum[:]),
	}
	if err := s.Put(ctx, m.ArtifactKey, bytes.NewReader(ct.Bytes())); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if err := artifact.Save(ctx, s, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	return m
}

func newStore(t *testing.T) storage.Backend {
	t.Helper()
	s, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return s
}

func TestMaterializeRoundTrip(t *testing.T) {
	s, key := newStore(t), testKey(t)
	plaintext := bytes.Repeat([]byte("PGDMP-ish payload "), 5000)
	m := seed(t, s, plaintext, key)

	var got bytes.Buffer
	if err := artifact.Materialize(context.Background(), s, m, key, &got); err != nil {
		t.Fatalf("artifact.Materialize: %v", err)
	}
	if !bytes.Equal(got.Bytes(), plaintext) {
		t.Error("materialized plaintext does not match the original dump")
	}
}

func TestMaterializeMissingArtifactReportsFetchStage(t *testing.T) {
	s, key := newStore(t), testKey(t)
	m := seed(t, s, []byte("data"), key)
	m.ArtifactKey = "artifacts/does-not-exist.enc"

	err := artifact.Materialize(context.Background(), s, m, key, io.Discard)
	if err == nil {
		t.Fatal("materializing a missing artifact must fail")
	}
	if got := artifact.StageOf(err); got != artifact.StageFetch {
		t.Errorf("stage = %q, want %q", got, artifact.StageFetch)
	}
}

func TestMaterializeCorruptArtifactReportsDecryptStage(t *testing.T) {
	s, key := newStore(t), testKey(t)
	m := seed(t, s, bytes.Repeat([]byte("data"), 5000), key)

	// flip a bit in the stored ciphertext, as localtest does on disk.
	rc, err := s.Get(context.Background(), m.ArtifactKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	ct, _ := io.ReadAll(rc)
	rc.Close()
	ct[len(ct)/2] ^= 0xFF
	if err := s.Put(context.Background(), m.ArtifactKey, bytes.NewReader(ct)); err != nil {
		t.Fatalf("put tampered: %v", err)
	}

	err = artifact.Materialize(context.Background(), s, m, key, io.Discard)
	if err == nil {
		t.Fatal("a tampered artifact must fail to materialize")
	}
	if got := artifact.StageOf(err); got != artifact.StageDecrypt {
		t.Errorf("stage = %q, want %q", got, artifact.StageDecrypt)
	}
}

func TestMaterializeWrongKeyReportsDecryptStage(t *testing.T) {
	s := newStore(t)
	m := seed(t, s, []byte("secret data"), testKey(t))

	err := artifact.Materialize(context.Background(), s, m, testKey(t), io.Discard)
	if err == nil {
		t.Fatal("the wrong key must fail to materialize")
	}
	if got := artifact.StageOf(err); got != artifact.StageDecrypt {
		t.Errorf("stage = %q, want %q", got, artifact.StageDecrypt)
	}
}

// TestMaterializeChecksumMismatchReportsChecksumStage covers corruption that
// AES-GCM cannot see: the artifact decrypts perfectly, but the plaintext is not
// what was dumped, because the manifest records a different checksum.
func TestMaterializeChecksumMismatchReportsChecksumStage(t *testing.T) {
	s, key := newStore(t), testKey(t)
	m := seed(t, s, []byte("the real dump"), key)
	m.PlaintextSHA256 = hex.EncodeToString(bytes.Repeat([]byte{0xAB}, 32))

	err := artifact.Materialize(context.Background(), s, m, key, io.Discard)
	if err == nil {
		t.Fatal("a checksum mismatch must fail")
	}
	if got := artifact.StageOf(err); got != artifact.StageChecksum {
		t.Errorf("stage = %q, want %q", got, artifact.StageChecksum)
	}
}

// TestMaterializeTruncatedArtifactFails guards the case crypto's terminator
// frame exists for: a short-read artifact must not pass as a good dump.
func TestMaterializeTruncatedArtifactFails(t *testing.T) {
	s, key := newStore(t), testKey(t)
	m := seed(t, s, bytes.Repeat([]byte("data"), 5000), key)

	rc, err := s.Get(context.Background(), m.ArtifactKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	ct, _ := io.ReadAll(rc)
	rc.Close()
	if err := s.Put(context.Background(), m.ArtifactKey, bytes.NewReader(ct[:len(ct)-4])); err != nil {
		t.Fatalf("put truncated: %v", err)
	}

	if err := artifact.Materialize(context.Background(), s, m, key, io.Discard); err == nil {
		t.Fatal("a truncated artifact must not materialize successfully")
	}
}

func TestMaterializeTempWritesFileAndCleansUp(t *testing.T) {
	s, key := newStore(t), testKey(t)
	plaintext := bytes.Repeat([]byte("payload"), 1000)
	m := seed(t, s, plaintext, key)

	path, cleanup, err := artifact.MaterializeTemp(context.Background(), s, m, key)
	if err != nil {
		t.Fatalf("artifact.MaterializeTemp: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("temp file contents do not match the original dump")
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup should remove the temp file, stat err = %v", err)
	}
}

// TestMaterializeTempLeavesNoFileOnFailure matters because a leftover partial
// dump could be mistaken for a good one. On failure there must be no path to
// find at all.
func TestMaterializeTempLeavesNoFileOnFailure(t *testing.T) {
	s := newStore(t)
	m := seed(t, s, []byte("secret data"), testKey(t))

	path, cleanup, err := artifact.MaterializeTemp(context.Background(), s, m, testKey(t))
	if err == nil {
		t.Fatal("the wrong key must fail")
	}
	if path != "" {
		t.Errorf("failure must not return a path, got %q", path)
	}
	if cleanup == nil {
		t.Error("cleanup must never be nil; callers defer it unconditionally")
	} else {
		cleanup() // must not panic
	}
}

func TestStageErrorUnwraps(t *testing.T) {
	backwynErr := errors.New("underlying")
	err := &artifact.StageError{Stage: artifact.StageFetch, Err: backwynErr}

	if !errors.Is(err, backwynErr) {
		t.Error("artifact.StageError must unwrap to the underlying error")
	}
	if artifact.StageOf(err) != artifact.StageFetch {
		t.Errorf("artifact.StageOf = %q, want %q", artifact.StageOf(err), artifact.StageFetch)
	}
	if artifact.StageOf(backwynErr) != "" {
		t.Error("artifact.StageOf on a plain error must return the empty stage")
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	s, key := newStore(t), testKey(t)
	m := seed(t, s, []byte("data"), key)

	m.Verification = manifest.Verification{Verified: true, TableCount: 7, ChecksumOK: true}
	if err := artifact.Save(context.Background(), s, m); err != nil {
		t.Fatalf("artifact.Save: %v", err)
	}

	got, err := artifact.Load(context.Background(), s, m.ID)
	if err != nil {
		t.Fatalf("artifact.Load: %v", err)
	}
	if !got.Verification.Verified || got.Verification.TableCount != 7 {
		t.Errorf("verification did not round trip: %+v", got.Verification)
	}
	if got.PlaintextSHA256 != m.PlaintextSHA256 {
		t.Error("checksum did not round trip")
	}
}

func TestLoadMissingManifest(t *testing.T) {
	if _, err := artifact.Load(context.Background(), newStore(t), "nope"); err == nil {
		t.Fatal("loading a missing manifest must fail")
	}
}
