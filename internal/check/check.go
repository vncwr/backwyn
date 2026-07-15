// package check implements absence-alerting: alert when no verified backup
// exists within a freshness window, not just when something errors.
//
// a job that silently stops produces no error. the only signal is the growing
// age of the last good backup.
package check

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/vncwr/backwyn/internal/artifact"
	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/storage"
)

// Report is the outcome of an absence check.
type Report struct {
	Now    time.Time
	MaxAge time.Duration

	// LastVerified is the most recent backup that passed verification, or nil.
	LastVerified    *manifest.Manifest
	LastVerifiedAge time.Duration

	// LatestBackup is the most recent backup regardless of verification state.
	LatestBackup *manifest.Manifest

	// Healthy is true when a verified backup exists within MaxAge.
	Healthy bool

	// Reasons explains why Healthy is false (empty when healthy).
	Reasons []string

	// Warnings do not mean coverage lapsed: mainly a latest backup that failed
	// while an older one still covers the window. callers must report these even
	// when healthy, or a broken latest backup goes unnoticed until it's an outage.
	Warnings []string
}

// Run evaluates all manifests against the freshness window. now and maxAge are
// injected so the check is deterministic.
func Run(ctx context.Context, store storage.Backend, maxAge time.Duration, now time.Time) (*Report, error) {
	manifests, err := artifact.LoadAll(ctx, store)
	if err != nil {
		return nil, err
	}

	// newest first by snapshot time.
	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].CreatedAt.After(manifests[j].CreatedAt)
	})

	rep := &Report{Now: now.UTC(), MaxAge: maxAge}
	if len(manifests) > 0 {
		rep.LatestBackup = manifests[0]
	}
	for _, m := range manifests {
		if m.Verification.Verified {
			rep.LastVerified = m
			rep.LastVerifiedAge = now.UTC().Sub(m.CreatedAt)
			break
		}
	}

	switch {
	case len(manifests) == 0:
		rep.Reasons = append(rep.Reasons, "no backups exist at all")
	case rep.LastVerified == nil:
		rep.Reasons = append(rep.Reasons, "backups exist but none are verified (unrestorable until proven)")
	case rep.LastVerifiedAge > maxAge:
		rep.Reasons = append(rep.Reasons,
			fmt.Sprintf("last verified backup is %s old, older than the %s threshold",
				roundDur(rep.LastVerifiedAge), maxAge))
	}

	// a warning, not a reason: older coverage may still be within the window, so
	// this does not make the report unhealthy — but it must never be swallowed.
	if rep.LatestBackup != nil && !rep.LatestBackup.Verification.Verified {
		note := rep.LatestBackup.Verification.Error
		if note == "" {
			note = "not yet verified"
		}
		rep.Warnings = append(rep.Warnings,
			fmt.Sprintf("most recent backup %s is not verified: %s", rep.LatestBackup.ID, note))
	}

	rep.Healthy = rep.LastVerified != nil && rep.LastVerifiedAge <= maxAge
	return rep, nil
}

func roundDur(d time.Duration) time.Duration {
	if d > time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}
