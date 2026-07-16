// package backup runs the backup pipeline: pg_dump -> sha256 -> encrypt -> store -> manifest.
// manifest is written last so it represents a completed backup.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/vncwr/backwyn/internal/config"
	"github.com/vncwr/backwyn/internal/crypto"
	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/pgtools"
	"github.com/vncwr/backwyn/internal/storage"
)

// result summarizes a backup.
type Result struct {
	Manifest *manifest.Manifest
}

// run performs a single backup.
func Run(ctx context.Context, cfg *config.Config, store storage.Backend, now time.Time) (*Result, error) {
	if err := pgtools.Require("pg_dump"); err != nil {
		return nil, err
	}

	// refuse a dump that RLS would silently truncate: with
	// --enable-row-security, any table the role cannot fully read exports
	// only its visible rows and still "succeeds".
	if cfg.DumpRowSecurity {
		uncovered, err := pgtools.RLSUncoveredTables(ctx, cfg.SourceDSN, cfg.DumpSchemas)
		if err != nil {
			return nil, fmt.Errorf("row security preflight: %w", err)
		}
		if len(uncovered) > 0 {
			return nil, fmt.Errorf(
				"row security preflight: %d table(s) would dump incomplete under RLS: %s "+
					"(grant the backup role a read-all policy — see sql/backup_role_rls.sql — or dump with a BYPASSRLS role)",
				len(uncovered), strings.Join(uncovered, ", "))
		}
	}

	id := now.UTC().Format("20060102T150405Z")
	artifactKey := "artifacts/" + id + ".dump.enc"

	dumpVer, err := pgtools.Version(ctx, "pg_dump")
	if err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp("", "backwyn-dump-*.pgc")
	if err != nil {
		return nil, fmt.Errorf("create temp dump file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := pgtools.Dump(ctx, cfg.SourceDSN, tmpPath, cfg.DumpSchemas, cfg.DumpRowSecurity); err != nil {
		return nil, err
	}

	sum, plaintextSize, err := hashFile(tmpPath)
	if err != nil {
		return nil, err
	}

	// encrypt into storage and count bytes.
	in, err := os.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("open dump for encryption: %w", err)
	}
	defer in.Close()

	pr, pw := io.Pipe()
	counter := &countingReader{r: pr}
	go func() {
		err := crypto.Encrypt(pw, in, cfg.EncryptionKey)
		pw.CloseWithError(err)
	}()
	if err := store.Put(ctx, artifactKey, counter); err != nil {
		return nil, fmt.Errorf("store encrypted artifact: %w", err)
	}

	// write manifest last so its existence implies completion.
	m := &manifest.Manifest{
		SchemaVersion:   1,
		ID:              id,
		CreatedAt:       now.UTC(),
		SourceLabel:     pgtools.SourceLabel(cfg.SourceDSN),
		ArtifactKey:     artifactKey,
		Format:          "custom",
		PgDumpVersion:   dumpVer,
		PlaintextSize:   plaintextSize,
		EncryptedSize:   counter.n,
		PlaintextSHA256: hex.EncodeToString(sum),
		Verification:    manifest.Verification{Verified: false},
	}

	pr2, pw2 := io.Pipe()
	go func() {
		err := m.Encode(pw2)
		pw2.CloseWithError(err)
	}()
	if err := store.Put(ctx, manifest.ManifestKey(id), pr2); err != nil {
		return nil, fmt.Errorf("store manifest: %w", err)
	}

	return &Result{Manifest: m}, nil
}

func hashFile(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}

// countingreader counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
