// package manifest describes the metadata record for a backup.
// stored unencrypted so list and check can run without keys.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// manifest holds metadata for a backup artifact.
type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"` // artifact key stem
	CreatedAt     time.Time `json:"created_at"`
	SourceLabel   string    `json:"source_label"` // credential-stripped identifier
	ArtifactKey   string    `json:"artifact_key"`
	Format        string    `json:"format"` // pg_dump format
	PgDumpVersion string    `json:"pg_dump_version"`
	PlaintextSize int64     `json:"plaintext_size"`
	EncryptedSize int64     `json:"encrypted_size"`

	// plaintext checksum.
	PlaintextSHA256 string `json:"plaintext_sha256"`

	// verification record.
	Verification Verification `json:"verification"`
}

// verification details if and how a backup was proven restorable.
type Verification struct {
	Verified   bool      `json:"verified"`
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	ChecksumOK bool      `json:"checksum_ok"` // checksum verified
	Listable   bool      `json:"listable"`    // listable by pg_restore
	Restored   bool      `json:"restored"`    // restored successfully
	TableCount int       `json:"table_count"`
	Error      string    `json:"error,omitempty"`
}

// manifestkey returns the storage key for a manifest.
func ManifestKey(id string) string {
	return "manifests/" + id + ".manifest.json"
}

// encode writes m as JSON.
func (m *Manifest) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// decode reads a manifest from r.
func Decode(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}
