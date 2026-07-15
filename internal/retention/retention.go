// package retention decides which backups to prune.
// safety guards:
// 1. do not prune unless at least one backup is verified.
// 2. never prune the most recent verified backup.
// 3. never prune the most recent backup.
package retention

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/storage"
)

// policy is a grandfather-father-son retention policy.
type Policy struct {
	KeepLast    int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
}

// iszero reports if the policy is empty.
func (p Policy) IsZero() bool {
	return p.KeepLast == 0 && p.KeepDaily == 0 && p.KeepWeekly == 0 && p.KeepMonthly == 0
}

// decision records prune outcome.
type Decision struct {
	Manifest *manifest.Manifest
	Keep     bool
	Reason   string
}

// plan is the set of decisions.
type Plan struct {
	Decisions []Decision
	// warnings explain why a plan is conservative.
	Warnings []string
}

// remove returns manifests to delete.
func (p *Plan) Remove() []*manifest.Manifest {
	var out []*manifest.Manifest
	for _, d := range p.Decisions {
		if !d.Keep {
			out = append(out, d.Manifest)
		}
	}
	return out
}

// kept returns manifests to retain.
func (p *Plan) Kept() []*manifest.Manifest {
	var out []*manifest.Manifest
	for _, d := range p.Decisions {
		if d.Keep {
			out = append(out, d.Manifest)
		}
	}
	return out
}

// bytes returns total bytes to free.
func (p *Plan) Bytes() int64 {
	var n int64
	for _, m := range p.Remove() {
		n += m.EncryptedSize
	}
	return n
}

// compute builds a prune plan.
func Compute(ms []*manifest.Manifest, p Policy, now time.Time) *Plan {
	sorted := make([]*manifest.Manifest, len(ms))
	copy(sorted, ms)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	plan := &Plan{}
	keepAll := func(reason string) *Plan {
		for _, m := range sorted {
			plan.Decisions = append(plan.Decisions, Decision{Manifest: m, Keep: true, Reason: reason})
		}
		return plan
	}

	if len(sorted) == 0 {
		return plan
	}
	if p.IsZero() {
		plan.Warnings = append(plan.Warnings, "retention policy is empty: keeping everything")
		return keepAll("no retention policy configured")
	}

	// guard 1: do not prune if no verified backup exists.
	var newestVerified *manifest.Manifest
	for _, m := range sorted {
		if m.Verification.Verified {
			newestVerified = m
			break
		}
	}
	if newestVerified == nil {
		plan.Warnings = append(plan.Warnings,
			"refusing to prune: no verified backup exists, so coverage is already broken")
		return keepAll("kept: nothing is verified, pruning is unsafe")
	}

	reasons := make(map[string]string, len(sorted))
	keep := func(m *manifest.Manifest, reason string) {
		if _, already := reasons[m.ID]; !already {
			reasons[m.ID] = reason
		}
	}

	// guards 2 and 3.
	keep(newestVerified, "most recent verified backup (safety net, never pruned)")
	keep(sorted[0], "most recent backup (may be mid-verification)")

	// policy slots.
	var lastDay, lastWeek, lastMonth string
	var nLast, nDaily, nWeekly, nMonthly int
	for _, m := range sorted {
		if !m.Verification.Verified {
			continue
		}
		t := m.CreatedAt.UTC()

		if nLast < p.KeepLast {
			nLast++
			keep(m, fmt.Sprintf("one of the last %d verified backups", p.KeepLast))
		}
		if day := t.Format("2006-01-02"); day != lastDay && nDaily < p.KeepDaily {
			lastDay, nDaily = day, nDaily+1
			keep(m, "daily slot "+day)
		}
		if week := isoWeek(t); week != lastWeek && nWeekly < p.KeepWeekly {
			lastWeek, nWeekly = week, nWeekly+1
			keep(m, "weekly slot "+week)
		}
		if month := t.Format("2006-01"); month != lastMonth && nMonthly < p.KeepMonthly {
			lastMonth, nMonthly = month, nMonthly+1
			keep(m, "monthly slot "+month)
		}
	}

	for _, m := range sorted {
		if reason, ok := reasons[m.ID]; ok {
			plan.Decisions = append(plan.Decisions, Decision{Manifest: m, Keep: true, Reason: reason})
			continue
		}
		reason := "outside retention policy"
		if !m.Verification.Verified {
			reason = "unverified: never proven restorable, so not coverage"
		}
		plan.Decisions = append(plan.Decisions, Decision{Manifest: m, Keep: false, Reason: reason})
	}
	return plan
}

func isoWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// apply executes a plan, deleting manifests before artifacts.
func Apply(ctx context.Context, store storage.Backend, plan *Plan) (freed int64, err error) {
	var failures []string
	for _, m := range plan.Remove() {
		if e := store.Delete(ctx, manifest.ManifestKey(m.ID)); e != nil {
			failures = append(failures, fmt.Sprintf("%s manifest: %v", m.ID, e))
			continue // skip deleting artifact if manifest delete failed
		}
		if e := store.Delete(ctx, m.ArtifactKey); e != nil {
			failures = append(failures, fmt.Sprintf("%s artifact: %v (orphaned, sweep will reclaim)", m.ID, e))
			continue
		}
		freed += m.EncryptedSize
	}
	if len(failures) > 0 {
		return freed, fmt.Errorf("prune completed with %d failure(s): %s",
			len(failures), strings.Join(failures, "; "))
	}
	return freed, nil
}

// sweeporphans deletes artifacts older than minage that have no manifest.
func SweepOrphans(ctx context.Context, store storage.Backend, minAge time.Duration, now time.Time) (removed int, freed int64, err error) {
	manifestKeys, err := store.List(ctx, "manifests/")
	if err != nil {
		return 0, 0, err
	}
	live := make(map[string]bool, len(manifestKeys))
	for _, k := range manifestKeys {
		id := strings.TrimSuffix(strings.TrimPrefix(k, "manifests/"), ".manifest.json")
		live[id] = true
	}

	artifactKeys, err := store.List(ctx, "artifacts/")
	if err != nil {
		return 0, 0, err
	}

	var failures []string
	for _, k := range artifactKeys {
		id := strings.TrimSuffix(strings.TrimPrefix(k, "artifacts/"), ".dump.enc")
		if live[id] {
			continue
		}
		// id is the timestamp, no need for object metadata.
		created, perr := time.Parse("20060102T150405Z", id)
		if perr != nil {
			continue // not our file
		}
		if now.UTC().Sub(created) < minAge {
			continue // might be in-flight
		}

		size, _ := store.Stat(ctx, k)
		if e := store.Delete(ctx, k); e != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", k, e))
			continue
		}
		removed++
		freed += size
	}
	if len(failures) > 0 {
		return removed, freed, fmt.Errorf("sweep completed with %d failure(s): %s",
			len(failures), strings.Join(failures, "; "))
	}
	return removed, freed, nil
}
