// package check checks that a verified backup exists within a freshness window.
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

// report is the outcome of a check.
type Report struct {
	Now    time.Time
	MaxAge time.Duration

	// last verified backup.
	LastVerified    *manifest.Manifest
	LastVerifiedAge time.Duration

	// most recent backup.
	LatestBackup *manifest.Manifest

	// healthy is true if a verified backup exists within maxage.
	Healthy bool

	// reasons healthy is false.
	Reasons []string

	// warnings for non-critical issues.
	Warnings []string
}

// run evaluates all manifests.
func Run(ctx context.Context, store storage.Backend, maxAge time.Duration, now time.Time) (*Report, error) {
	manifests, err := artifact.LoadAll(ctx, store)
	if err != nil {
		return nil, err
	}

	// sort by creation time.
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

	// warn if the newest backup is not verified.
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
