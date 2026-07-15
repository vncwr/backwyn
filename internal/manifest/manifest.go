// package manifest describes the metadata record written for every backup.
//
// holds no secrets, so it is stored unencrypted and check/list work without the
// encryption key.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Manifest is the metadata for a single backup artifact.
type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"` // also the artifact key stem
	CreatedAt     time.Time `json:"created_at"`
	SourceLabel   string    `json:"source_label"` // credentials stripped
	ArtifactKey   string    `json:"artifact_key"`
	Format        string    `json:"format"` // pg_dump output format
	PgDumpVersion string    `json:"pg_dump_version"`
	PlaintextSize int64     `json:"plaintext_size"`
	EncryptedSize int64     `json:"encrypted_size"`

	// PlaintextSHA256 is the checksum of the unencrypted dump, re-checked on
	// every verify to detect corruption anywhere in the round trip.
	PlaintextSHA256 string `json:"plaintext_sha256"`

	// Verification records the last restore-test. A backup is not trustworthy
	// until Verified is true.
	Verification Verification `json:"verification"`
}

// Verification captures whether and how a backup was proven restorable.
type Verification struct {
	Verified   bool      `json:"verified"`
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	ChecksumOK bool      `json:"checksum_ok"` // plaintext matched PlaintextSHA256
	Listable   bool      `json:"listable"`    // pg_restore --list parsed it
	Restored   bool      `json:"restored"`    // restored into a throwaway db
	TableCount int       `json:"table_count"`
	Error      string    `json:"error,omitempty"`
}

// ManifestKey returns the storage key for a backup's manifest.
func ManifestKey(id string) string {
	return "manifests/" + id + ".manifest.json"
}

// Encode writes m as indented JSON.
func (m *Manifest) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// Decode reads a manifest from r.
func Decode(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}
