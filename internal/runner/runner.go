// package runner performs backup, verify, check coverage, and prune.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vncwr/backwyn/internal/alert"
	"github.com/vncwr/backwyn/internal/artifact"
	"github.com/vncwr/backwyn/internal/backup"
	"github.com/vncwr/backwyn/internal/check"
	"github.com/vncwr/backwyn/internal/config"
	"github.com/vncwr/backwyn/internal/retention"
	"github.com/vncwr/backwyn/internal/storage"
	"github.com/vncwr/backwyn/internal/verify"
)

// deps holds collaborators for a cycle.
type Deps struct {
	Cfg     *config.Config
	Store   storage.Backend
	Alerter alert.Alerter
	MaxAge  time.Duration
	Now     func() time.Time
	Tracker *Tracker

	// retention policy.
	Retention retention.Policy
}

// cycle runs backup -> verify -> check once. check runs even when the cycle
// fails — an older verified backup may still hold coverage.
func Cycle(ctx context.Context, d Deps) error {
	now := d.Now()

	cycleErr := d.backupAndVerify(ctx, now)

	rep, err := check.Run(ctx, d.Store, d.MaxAge, now)
	if err != nil {
		if cycleErr != nil {
			return cycleErr
		}
		return fmt.Errorf("check: %w", err)
	}
	if d.Tracker != nil {
		var lastVerified time.Time
		if rep.LastVerified != nil {
			lastVerified = rep.LastVerified.CreatedAt
		}
		d.Tracker.RecordCheck(rep.Healthy, lastVerified)
	}

	if !rep.Healthy {
		detail := strings.Join(rep.Reasons, "; ")
		d.emit(ctx, alert.LevelError, "backup coverage unhealthy", detail, now)
		if cycleErr != nil {
			return cycleErr
		}
		return fmt.Errorf("coverage unhealthy: %s", detail)
	}

	// never prune on a failed cycle.
	if cycleErr != nil {
		return cycleErr
	}

	d.prune(ctx, now)

	// coverage is healthy but relies on an older backup.
	if len(rep.Warnings) > 0 {
		detail := strings.Join(rep.Warnings, "; ")
		slog.Warn("cycle healthy with warnings", "warnings", detail)
		d.emit(ctx, alert.LevelWarn, "backup coverage degraded", detail, now)
		return nil
	}

	slog.Info("cycle healthy", "max_age", d.MaxAge)
	return nil
}

// backupandverify runs one backup and its verification.
func (d Deps) backupAndVerify(ctx context.Context, now time.Time) error {
	startBackup := time.Now()
	res, err := backup.Run(ctx, d.Cfg, d.Store, now)
	if err != nil {
		if d.Tracker != nil {
			d.Tracker.RecordBackup(false, 0, time.Since(startBackup))
		}
		d.emit(ctx, alert.LevelError, "backup failed", err.Error(), now)
		return fmt.Errorf("backup: %w", err)
	}
	if d.Tracker != nil {
		d.Tracker.RecordBackup(true, res.Manifest.EncryptedSize, time.Since(startBackup))
	}
	slog.Info("backup ok", "id", res.Manifest.ID, "size_bytes", res.Manifest.EncryptedSize)

	// verify.Run persists failures to the manifest.
	m, verr := verify.Run(ctx, d.Cfg, d.Store, res.Manifest.ID, now)
	if verr != nil {
		if d.Tracker != nil {
			d.Tracker.RecordVerify(false, 0)
		}
		d.emit(ctx, alert.LevelError, "verify failed",
			fmt.Sprintf("backup %s did not restore: %v", res.Manifest.ID, verr), now)
		return fmt.Errorf("verify: %w", verr)
	}
	if d.Tracker != nil {
		d.Tracker.RecordVerify(true, m.Verification.TableCount)
	}
	slog.Info("verify ok", "id", m.ID, "table_count", m.Verification.TableCount)
	return nil
}

// prune applies the retention policy.
func (d Deps) prune(ctx context.Context, now time.Time) {
	if d.Retention.IsZero() {
		return
	}
	ms, err := artifact.LoadAll(ctx, d.Store)
	if err != nil {
		slog.Error("prune skipped", "err", err)
		return
	}
	plan := retention.Compute(ms, d.Retention, now)
	for _, w := range plan.Warnings {
		slog.Warn("prune warning", "warning", w)
	}
	if len(plan.Remove()) == 0 {
		return
	}

	freed, err := retention.Apply(ctx, d.Store, plan)
	if err != nil {
		slog.Error("prune error", "err", err)
		d.emit(ctx, alert.LevelWarn, "prune failed", err.Error(), now)
		return
	}
	slog.Info("pruned backups", "count", len(plan.Remove()), "freed_bytes", freed)
}

func (d Deps) emit(ctx context.Context, level alert.Level, title, detail string, now time.Time) {
	if err := d.Alerter.Alert(ctx, alert.Event{Level: level, Title: title, Detail: detail, Time: now.UTC()}); err != nil {
		slog.Warn("failed to deliver alert", "title", title, "err", err)
	}
}
