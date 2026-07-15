// package config loads engine configuration from environment variables.
//
// secrets come from the environment only: flags leak into process listings and
// shell history.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
)

// Backend selects where artifacts and manifests are stored.
type Backend string

const (
	BackendLocal Backend = "local"
	BackendS3    Backend = "s3"
)

// Config is the fully-resolved engine configuration for a single run.
type Config struct {
	// SourceDSN is the connection string of the database being backed up.
	// should point at a least-privilege role, never the owner/superuser.
	SourceDSN string

	// EncryptionKey is a 32-byte key used for AES-256-GCM. Loaded from a
	// base64-encoded env var so raw key bytes never sit in shell history.
	EncryptionKey []byte

	// VerifyAdminDSN is an admin connection used to create and drop the
	// throwaway databases that backups are test-restored into. This should
	// be a LOCAL Postgres (the verify sandbox), not the production source.
	VerifyAdminDSN string

	// Backend selects the storage implementation.
	Backend Backend

	// StorageDir is the local filesystem backend root (Backend == local).
	StorageDir string

	// S3 holds the object-storage backend config (Backend == s3). This is how
	// off-provider / bring-your-own-bucket copies are configured.
	S3 S3Config

	// AlertWebhook receives JSON on failure/unhealthy coverage. empty disables it.
	AlertWebhook string
}

// S3Config configures an S3-compatible backend (AWS S3, Cloudflare R2, ...).
type S3Config struct {
	Bucket    string
	Endpoint  string // e.g. https://<account>.r2.cloudflarestorage.com; empty = AWS default
	Region    string // R2 uses "auto"
	AccessKey string
	SecretKey string
	PathStyle bool // R2 and most S3-compatible fakes want path-style addressing
}

// Load reads and validates configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		SourceDSN:      os.Getenv("BACKWYN_SOURCE_DSN"),
		VerifyAdminDSN: os.Getenv("BACKWYN_VERIFY_ADMIN_DSN"),
		Backend:        Backend(getDefault("BACKWYN_STORAGE", string(BackendLocal))),
		StorageDir:     os.Getenv("BACKWYN_STORAGE_DIR"),
		S3: S3Config{
			Bucket:    os.Getenv("BACKWYN_S3_BUCKET"),
			Endpoint:  os.Getenv("BACKWYN_S3_ENDPOINT"),
			Region:    getDefault("BACKWYN_S3_REGION", "auto"),
			AccessKey: os.Getenv("BACKWYN_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("BACKWYN_S3_SECRET_KEY"),
			PathStyle: os.Getenv("BACKWYN_S3_PATH_STYLE") == "true",
		},
		AlertWebhook: os.Getenv("BACKWYN_ALERT_WEBHOOK"),
	}

	if c.SourceDSN == "" {
		return nil, fmt.Errorf("BACKWYN_SOURCE_DSN is required")
	}

	switch c.Backend {
	case BackendLocal:
		if c.StorageDir == "" {
			return nil, fmt.Errorf("BACKWYN_STORAGE_DIR is required for the local backend")
		}
	case BackendS3:
		if c.S3.Bucket == "" {
			return nil, fmt.Errorf("BACKWYN_S3_BUCKET is required for the s3 backend")
		}
		if c.S3.AccessKey == "" || c.S3.SecretKey == "" {
			return nil, fmt.Errorf("BACKWYN_S3_ACCESS_KEY and BACKWYN_S3_SECRET_KEY are required for the s3 backend")
		}
	default:
		return nil, fmt.Errorf("BACKWYN_STORAGE must be %q or %q, got %q", BackendLocal, BackendS3, c.Backend)
	}

	rawKey := os.Getenv("BACKWYN_ENCRYPTION_KEY")
	if rawKey == "" {
		return nil, fmt.Errorf("BACKWYN_ENCRYPTION_KEY is required (base64-encoded 32 bytes)")
	}
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return nil, fmt.Errorf("BACKWYN_ENCRYPTION_KEY is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("BACKWYN_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(key))
	}
	c.EncryptionKey = key

	return c, nil
}

func getDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
