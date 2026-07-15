// package restore restores backups to a database or a file.
package restore

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/vncwr/backwyn/internal/artifact"
	"github.com/vncwr/backwyn/internal/config"
	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/pgtools"
	"github.com/vncwr/backwyn/internal/storage"
)

// options configures a restore.
type Options struct {
	TargetDSN string
	ToFile    string
	// force overrides guards.
	Force bool
	// allowunverified allows restoring unverified backups.
	AllowUnverified bool
}

// result summarizes a restore.
type Result struct {
	Manifest    *manifest.Manifest
	Path        string // path to output file
	TargetLabel string // stripped connection label
	TableCount  int
	Duration    time.Duration
}

// run restores a backup.
func Run(ctx context.Context, cfg *config.Config, store storage.Backend, id string, opts Options) (*Result, error) {
	if opts.TargetDSN == "" && opts.ToFile == "" {
		return nil, fmt.Errorf("a restore target is required: pass -to <dsn> or -to-file <path>")
	}
	if opts.TargetDSN != "" && opts.ToFile != "" {
		return nil, fmt.Errorf("-to and -to-file are mutually exclusive")
	}

	m, err := artifact.Load(ctx, store, id)
	if err != nil {
		return nil, err
	}

	// guard: refuse unverified unless allowed.
	if !m.Verification.Verified && !opts.AllowUnverified {
		reason := m.Verification.Error
		if reason == "" {
			reason = "it has not been verified yet"
		}
		return nil, fmt.Errorf("backup %s is not verified: %s\n"+
			"run 'backwyn verify %s' first, or pass -allow-unverified to restore it anyway",
			id, reason, id)
	}

	start := time.Now()

	if opts.ToFile != "" {
		if err := materializeToFile(ctx, store, m, cfg.EncryptionKey, opts.ToFile, opts.Force); err != nil {
			return nil, err
		}
		return &Result{Manifest: m, Path: opts.ToFile, Duration: time.Since(start)}, nil
	}

	return toDatabase(ctx, cfg, store, m, opts, start)
}

func toDatabase(ctx context.Context, cfg *config.Config, store storage.Backend, m *manifest.Manifest, opts Options, start time.Time) (*Result, error) {
	if err := pgtools.Require("pg_restore", "psql"); err != nil {
		return nil, err
	}

	// guard: do not overwrite the source database.
	if cfg.SourceDSN != "" && pgtools.SameTarget(opts.TargetDSN, cfg.SourceDSN) && !opts.Force {
		return nil, fmt.Errorf("refusing to restore into the source database %s\n"+
			"restore into a new database instead, or pass -force if overwriting the source is genuinely what you want",
			pgtools.SourceLabel(cfg.SourceDSN))
	}

	// guard: target database must be empty.
	existing, err := pgtools.CountUserTables(ctx, opts.TargetDSN)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect restore target %s: %w\n"+
			"the target database must already exist (create it with: createdb <name>)",
			pgtools.SourceLabel(opts.TargetDSN), err)
	}
	if existing > 0 && !opts.Force {
		return nil, fmt.Errorf("refusing to restore into %s: it already contains %d user table(s)\n"+
			"restore into an empty database, or pass -force to restore over its contents",
			pgtools.SourceLabel(opts.TargetDSN), existing)
	}

	tmpPath, cleanup, err := artifact.MaterializeTemp(ctx, store, m, cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("restore %s: %w", m.ID, err)
	}
	defer cleanup()

	// clean existing database if not empty.
	if err := pgtools.Restore(ctx, opts.TargetDSN, tmpPath, pgtools.RestoreOptions{
		Clean: existing > 0,
	}); err != nil {
		return nil, fmt.Errorf("restore %s into %s: %w", m.ID, pgtools.SourceLabel(opts.TargetDSN), err)
	}

	count, err := pgtools.CountUserTables(ctx, opts.TargetDSN)
	if err != nil {
		return nil, fmt.Errorf("restore completed but counting tables failed: %w", err)
	}

	return &Result{
		Manifest:    m,
		TargetLabel: pgtools.SourceLabel(opts.TargetDSN),
		TableCount:  count,
		Duration:    time.Since(start),
	}, nil
}

func materializeToFile(ctx context.Context, store storage.Backend, m *manifest.Manifest, key []byte, path string, force bool) error {
	// guard: do not overwrite existing file.
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; choose another path or pass -force to overwrite", path)
		}
		return err
	}

	if err := artifact.Materialize(ctx, store, m, key, f); err != nil {
		f.Close()
		// clean up failed file write.
		os.Remove(path)
		return fmt.Errorf("restore %s to file: %w", m.ID, err)
	}
	return f.Close()
}
