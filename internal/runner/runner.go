// package runner performs one cycle: backup, verify, check coverage, prune.
// the daemon and -once mode drive the same Cycle.
package runner

import (
	"context"
	"fmt"
	"log"
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

// Deps are the collaborators a cycle needs. Now is injected for testability.
type Deps struct {
	Cfg     *config.Config
	Store   storage.Backend
	Alerter alert.Alerter
	MaxAge  time.Duration
	Now     func() time.Time

	// Retention prunes at the end of each cycle. A zero policy keeps everything.
	Retention retention.Policy
}

// Cycle runs backup -> verify -> check once. It returns an error if the cycle
// did not end with healthy, verified coverage. Alerts are emitted as a side
// effect so a caller looping on a timer needs no extra wiring.
func Cycle(ctx context.Context, d Deps) error {
	now := d.Now()

	res, err := backup.Run(ctx, d.Cfg, d.Store, now)
	if err != nil {
		d.emit(ctx, alert.LevelError, "backup failed", err.Error(), now)
		return fmt.Errorf("backup: %w", err)
	}
	log.Printf("backup ok: %s (%d bytes)", res.Manifest.ID, res.Manifest.EncryptedSize)

	// verify.Run persists failures to the manifest before returning, so
	// absence-alerting still sees them.
	m, verr := verify.Run(ctx, d.Cfg, d.Store, res.Manifest.ID, now)
	if verr != nil {
		d.emit(ctx, alert.LevelError, "verify failed",
			fmt.Sprintf("backup %s did not restore: %v", res.Manifest.ID, verr), now)
		return fmt.Errorf("verify: %w", verr)
	}
	log.Printf("verify ok: %s restored %d tables", m.ID, m.Verification.TableCount)

	rep, err := check.Run(ctx, d.Store, d.MaxAge, now)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if !rep.Healthy {
		detail := strings.Join(rep.Reasons, "; ")
		d.emit(ctx, alert.LevelError, "backup coverage unhealthy", detail, now)
		return fmt.Errorf("coverage unhealthy: %s", detail)
	}

	// only after coverage is confirmed healthy: never delete on a cycle where
	// we are unsure the new backup is good.
	d.prune(ctx, now)

	// healthy, but coverage holds only because an older backup carries it.
	if len(rep.Warnings) > 0 {
		detail := strings.Join(rep.Warnings, "; ")
		log.Printf("cycle healthy with warnings: %s", detail)
		d.emit(ctx, alert.LevelWarn, "backup coverage degraded", detail, now)
		return nil
	}

	log.Printf("cycle healthy: verified backup within %s", d.MaxAge)
	return nil
}

// prune applies the retention policy. failures are logged and alerted but do
// not fail the cycle: coverage is already healthy and the cost is storage.
func (d Deps) prune(ctx context.Context, now time.Time) {
	if d.Retention.IsZero() {
		return
	}
	ms, err := artifact.LoadAll(ctx, d.Store)
	if err != nil {
		log.Printf("prune skipped: %v", err)
		return
	}
	plan := retention.Compute(ms, d.Retention, now)
	for _, w := range plan.Warnings {
		log.Printf("prune: %s", w)
	}
	if len(plan.Remove()) == 0 {
		return
	}

	freed, err := retention.Apply(ctx, d.Store, plan)
	if err != nil {
		log.Printf("prune error: %v", err)
		d.emit(ctx, alert.LevelWarn, "prune failed", err.Error(), now)
		return
	}
	log.Printf("pruned %d backup(s), freed %d bytes", len(plan.Remove()), freed)
}

func (d Deps) emit(ctx context.Context, level alert.Level, title, detail string, now time.Time) {
	if err := d.Alerter.Alert(ctx, alert.Event{Level: level, Title: title, Detail: detail, Time: now.UTC()}); err != nil {
		log.Printf("WARNING: failed to deliver alert %q: %v", title, err)
	}
}
