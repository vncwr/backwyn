// package config loads configuration from environment variables.
// secrets are read from the environment to avoid leaking in flags.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// backend selects where artifacts and manifests are stored.
type Backend string

const (
	BackendLocal Backend = "local"
	BackendS3    Backend = "s3"
)

// config is the resolved configuration for a run.
type Config struct {
	// source database connection string.
	SourceDSN string

	// encryption key for aes-256-gcm.
	EncryptionKey []byte

	// verifyadmindsn is the sandbox postgres admin dsn to test restores.
	VerifyAdminDSN string

	// storage backend selection.
	Backend Backend

	// local filesystem directory root.
	StorageDir string

	// s3 backend configuration.
	S3 S3Config

	// optional alert webhook url.
	AlertWebhook string

	// optional verify SQL query to execute on restore validation.
	VerifyQuery string

	// optional schemas to scope pg_dump to; empty dumps the whole database.
	DumpSchemas []string
}

// s3config configures s3 storage.
type S3Config struct {
	Bucket    string
	Endpoint  string // s3 endpoint url; empty for aws default
	Region    string // bucket region
	AccessKey string
	SecretKey string
	PathStyle bool // use path-style addressing
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
		VerifyQuery:  os.Getenv("BACKWYN_VERIFY_QUERY"),
		DumpSchemas:  splitList(os.Getenv("BACKWYN_DUMP_SCHEMAS")),
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

// splitlist splits a comma-separated value, dropping empty entries.
func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
