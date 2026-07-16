// package verify verifies a stored backup is restorable.
package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/vncwr/backwyn/internal/artifact"
	"github.com/vncwr/backwyn/internal/config"
	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/pgtools"
	"github.com/vncwr/backwyn/internal/storage"
)

// run verifies a backup and updates its manifest.
func Run(ctx context.Context, cfg *config.Config, store storage.Backend, id string, now time.Time) (*manifest.Manifest, error) {
	if err := pgtools.Require("pg_restore", "psql"); err != nil {
		return nil, err
	}
	if cfg.VerifyAdminDSN == "" {
		return nil, fmt.Errorf("BACKWYN_VERIFY_ADMIN_DSN is required to verify (the local restore sandbox)")
	}

	m, err := artifact.Load(ctx, store, id)
	if err != nil {
		return nil, err
	}

	// reset state.
	m.Verification = manifest.Verification{}

	fail := func(format string, args ...any) (*manifest.Manifest, error) {
		m.Verification.Error = fmt.Sprintf(format, args...)
		m.Verification.Verified = false
		_ = artifact.Save(ctx, store, m) // persist results
		return m, fmt.Errorf("verification failed: %s", m.Verification.Error)
	}

	tmpPath, cleanup, err := artifact.MaterializeTemp(ctx, store, m, cfg.EncryptionKey)
	if err != nil {
		// a write error is a local environment issue, not a bad backup.
		if artifact.StageOf(err) == artifact.StageWrite {
			return nil, err
		}
		return fail("%v", err)
	}
	defer cleanup()
	m.Verification.ChecksumOK = true

	// verify the archive is listable.
	if _, err := pgtools.RestoreList(ctx, tmpPath); err != nil {
		return fail("pg_restore --list: %v", err)
	}
	m.Verification.Listable = true

	// restore and count tables.
	scratchDB := "backwyn_verify_" + id
	if err := pgtools.CreateDatabase(ctx, cfg.VerifyAdminDSN, scratchDB); err != nil {
		return fail("create scratch db: %v", err)
	}
	defer func() {
		// best-effort cleanup.
		_ = pgtools.DropDatabase(context.WithoutCancel(ctx), cfg.VerifyAdminDSN, scratchDB)
	}()

	targetDSN, err := pgtools.WithDatabase(cfg.VerifyAdminDSN, scratchDB)
	if err != nil {
		return fail("derive target dsn: %v", err)
	}
	// scratch db is empty, no clean needed.
	if err := pgtools.Restore(ctx, targetDSN, tmpPath, pgtools.RestoreOptions{Sandbox: true}); err != nil {
		return fail("restore into scratch db: %v", err)
	}
	m.Verification.Restored = true

	count, err := pgtools.CountUserTables(ctx, targetDSN)
	if err != nil {
		return fail("count tables: %v", err)
	}
	m.Verification.TableCount = count

	if cfg.VerifyQuery != "" {
		if _, err := pgtools.RunQuery(ctx, targetDSN, cfg.VerifyQuery); err != nil {
			return fail("verify query: %v", err)
		}
	}

	m.Verification.Verified = true
	m.Verification.VerifiedAt = now.UTC()
	if err := artifact.Save(ctx, store, m); err != nil {
		return nil, fmt.Errorf("save verified manifest: %w", err)
	}
	return m, nil
}
