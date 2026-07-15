package retention_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/storage"

	"github.com/vncwr/backwyn/internal/retention"
)

var now = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func at(t time.Time) *manifest.Manifest {
	id := t.UTC().Format("20060102T150405Z")
	return &manifest.Manifest{
		SchemaVersion: 1,
		ID:            id,
		CreatedAt:     t.UTC(),
		ArtifactKey:   "artifacts/" + id + ".dump.enc",
		EncryptedSize: 1000,
		Verification:  manifest.Verification{Verified: true, VerifiedAt: t.UTC()},
	}
}

func unverifiedAt(t time.Time) *manifest.Manifest {
	m := at(t)
	m.Verification = manifest.Verification{Verified: false, Error: "checksum mismatch"}
	return m
}

func daysAgo(n int) time.Time  { return now.AddDate(0, 0, -n) }
func hoursAgo(n int) time.Time { return now.Add(-time.Duration(n) * time.Hour) }

func ids(ms []*manifest.Manifest) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func contains(ms []*manifest.Manifest, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestEmptyPolicyPrunesNothing: an unconfigured policy must keep everything.
// The opposite default would delete a customer's history on a typo.
func TestEmptyPolicyPrunesNothing(t *testing.T) {
	ms := []*manifest.Manifest{at(daysAgo(1)), at(daysAgo(30)), at(daysAgo(400))}
	plan := retention.Compute(ms, retention.Policy{}, now)

	if got := plan.Remove(); len(got) != 0 {
		t.Errorf("empty policy removed %v, want nothing", ids(got))
	}
	if len(plan.Warnings) == 0 {
		t.Error("an empty policy should warn that it is keeping everything")
	}
}

// TestNoVerifiedBackupPrunesNothing is safety rule 1: never make a broken
// situation worse.
func TestNoVerifiedBackupPrunesNothing(t *testing.T) {
	ms := []*manifest.Manifest{
		unverifiedAt(hoursAgo(1)),
		unverifiedAt(daysAgo(10)),
		unverifiedAt(daysAgo(400)),
	}
	plan := retention.Compute(ms, retention.Policy{KeepLast: 1, KeepDaily: 1}, now)

	if got := plan.Remove(); len(got) != 0 {
		t.Errorf("removed %v with no verified backup present, want nothing", ids(got))
	}
	if !strings.Contains(strings.Join(plan.Warnings, "; "), "no verified backup") {
		t.Errorf("warnings = %v, want mention of no verified backup", plan.Warnings)
	}
}

// TestNewestVerifiedIsNeverPruned is safety rule 2 — the invariant the whole
// product rests on. Even a policy that keeps nothing by rule must not touch it.
func TestNewestVerifiedIsNeverPruned(t *testing.T) {
	survivor := at(daysAgo(400)) // ancient, and the only verified backup
	ms := []*manifest.Manifest{
		unverifiedAt(hoursAgo(1)),
		unverifiedAt(daysAgo(2)),
		survivor,
	}
	// A policy with slots that the ancient backup cannot plausibly fill.
	plan := retention.Compute(ms, retention.Policy{KeepDaily: 1}, now)

	if contains(plan.Remove(), survivor.ID) {
		t.Fatal("the only verified backup was pruned; coverage would be destroyed")
	}
	var reason string
	for _, d := range plan.Decisions {
		if d.Manifest.ID == survivor.ID {
			reason = d.Reason
		}
	}
	if !strings.Contains(reason, "safety net") {
		t.Errorf("kept for reason %q, want the safety-net rule to be what saved it", reason)
	}
}

// TestNewestBackupIsNeverPruned is safety rule 3: a just-written backup that
// verify has not reached yet must survive a concurrent prune.
func TestNewestBackupIsNeverPruned(t *testing.T) {
	inFlight := unverifiedAt(now) // written seconds ago, not yet verified
	ms := []*manifest.Manifest{inFlight, at(hoursAgo(6))}

	plan := retention.Compute(ms, retention.Policy{KeepLast: 1}, now)

	if contains(plan.Remove(), inFlight.ID) {
		t.Error("pruned the newest backup, racing an in-flight verify")
	}
}

func TestUnverifiedBackupsArePruned(t *testing.T) {
	broken := unverifiedAt(daysAgo(3))
	ms := []*manifest.Manifest{
		at(hoursAgo(1)), // newest + verified, protected
		broken,
		at(daysAgo(4)),
	}
	plan := retention.Compute(ms, retention.Policy{KeepLast: 1}, now)

	if !contains(plan.Remove(), broken.ID) {
		t.Error("an unverified backup should not be retained as coverage")
	}
	var reason string
	for _, d := range plan.Decisions {
		if d.Manifest.ID == broken.ID {
			reason = d.Reason
		}
	}
	if !strings.Contains(reason, "unverified") {
		t.Errorf("reason = %q, want it to explain the backup was never verified", reason)
	}
}

func TestKeepLastRetainsNewestN(t *testing.T) {
	ms := []*manifest.Manifest{
		at(hoursAgo(1)), at(hoursAgo(7)), at(hoursAgo(13)), at(hoursAgo(19)), at(hoursAgo(25)),
	}
	plan := retention.Compute(ms, retention.Policy{KeepLast: 3}, now)

	kept := plan.Kept()
	if len(kept) != 3 {
		t.Fatalf("kept %v, want exactly 3", ids(kept))
	}
	for _, h := range []int{1, 7, 13} {
		want := at(hoursAgo(h)).ID
		if !contains(kept, want) {
			t.Errorf("backup from %dh ago (%s) should be kept", h, want)
		}
	}
}

// dayAt builds a backup at a fixed hour on the day d days before now. Deriving
// times by subtracting hours instead would silently cross midnight and put
// "same day" backups on different calendar days.
func dayAt(d, hour int) *manifest.Manifest {
	day := now.AddDate(0, 0, -d)
	return at(time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, time.UTC))
}

// TestKeepDailyKeepsNewestPerDay pins the GFS semantics: one representative per
// period, not N backups total, and the representative is the newest in its day.
func TestKeepDailyKeepsNewestPerDay(t *testing.T) {
	// Two backups per day (02:00 and 10:00) for four days, all before `now`.
	var ms []*manifest.Manifest
	for d := 0; d < 4; d++ {
		ms = append(ms, dayAt(d, 2), dayAt(d, 10))
	}
	plan := retention.Compute(ms, retention.Policy{KeepDaily: 3}, now)

	kept := plan.Kept()
	if len(kept) != 3 {
		t.Fatalf("kept %v, want 3 (one per day for 3 days)", ids(kept))
	}
	for d := 0; d < 3; d++ {
		want := dayAt(d, 10).ID // the later backup of that day
		if !contains(kept, want) {
			t.Errorf("day -%d: expected that day's newest backup (%s) to be kept, got %v", d, want, ids(kept))
		}
		if contains(kept, dayAt(d, 2).ID) {
			t.Errorf("day -%d: the earlier backup should not also fill the slot", d)
		}
	}
	if len(plan.Remove()) != 5 {
		t.Errorf("removed %v, want 5", ids(plan.Remove()))
	}
}

func TestKeepMonthlyAndWeeklySlots(t *testing.T) {
	ms := []*manifest.Manifest{
		at(daysAgo(0)),
		at(daysAgo(9)),
		at(daysAgo(40)),
		at(daysAgo(75)),
		at(daysAgo(200)),
	}
	plan := retention.Compute(ms, retention.Policy{KeepMonthly: 3}, now)

	kept := plan.Kept()
	// Distinct months: Jul(0), Jul(9) same month, Jun(40), Apr/May(75), Dec(200).
	// 3 monthly slots take the newest of the 3 most recent distinct months.
	if !contains(kept, at(daysAgo(0)).ID) {
		t.Error("newest backup must be kept")
	}
	if contains(kept, at(daysAgo(200)).ID) {
		t.Error("a backup 200 days old should fall outside 3 monthly slots")
	}
	if contains(kept, at(daysAgo(9)).ID) {
		t.Error("the older backup in an already-filled month should be pruned")
	}
}

func TestPlanBytesCountsOnlyRemoved(t *testing.T) {
	ms := []*manifest.Manifest{at(hoursAgo(1)), at(daysAgo(50)), at(daysAgo(60))}
	plan := retention.Compute(ms, retention.Policy{KeepLast: 1}, now)

	if got, want := plan.Bytes(), int64(2000); got != want {
		t.Errorf("Bytes = %d, want %d (2 pruned backups at 1000 each)", got, want)
	}
}

func TestComputeDoesNotMutateInput(t *testing.T) {
	ms := []*manifest.Manifest{at(daysAgo(5)), at(hoursAgo(1)), at(daysAgo(2))}
	before := ids(ms)

	retention.Compute(ms, retention.Policy{KeepLast: 1}, now)

	for i, id := range ids(ms) {
		if id != before[i] {
			t.Fatalf("retention.Compute reordered its input: %v -> %v", before, ids(ms))
		}
	}
}

// --- retention.Apply / retention.SweepOrphans ---

func newStore(t *testing.T) storage.Backend {
	t.Helper()
	s, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return s
}

func seed(t *testing.T, s storage.Backend, ms ...*manifest.Manifest) {
	t.Helper()
	ctx := context.Background()
	for _, m := range ms {
		pr, pw := io.Pipe()
		go func(m *manifest.Manifest) { pw.CloseWithError(m.Encode(pw)) }(m)
		if err := s.Put(ctx, manifest.ManifestKey(m.ID), pr); err != nil {
			t.Fatalf("seed manifest %s: %v", m.ID, err)
		}
		if err := s.Put(ctx, m.ArtifactKey, strings.NewReader(strings.Repeat("x", 1000))); err != nil {
			t.Fatalf("seed artifact %s: %v", m.ID, err)
		}
	}
}

func exists(t *testing.T, s storage.Backend, key string) bool {
	t.Helper()
	_, err := s.Stat(context.Background(), key)
	return err == nil
}

func TestApplyDeletesBothObjects(t *testing.T) {
	s := newStore(t)
	keepM, dropM := at(hoursAgo(1)), at(daysAgo(90))
	seed(t, s, keepM, dropM)

	plan := retention.Compute([]*manifest.Manifest{keepM, dropM}, retention.Policy{KeepLast: 1}, now)
	freed, err := retention.Apply(context.Background(), s, plan)
	if err != nil {
		t.Fatalf("retention.Apply: %v", err)
	}
	if freed != 1000 {
		t.Errorf("freed = %d, want 1000", freed)
	}

	if exists(t, s, manifest.ManifestKey(dropM.ID)) || exists(t, s, dropM.ArtifactKey) {
		t.Error("pruned backup should have neither manifest nor artifact left")
	}
	if !exists(t, s, manifest.ManifestKey(keepM.ID)) || !exists(t, s, keepM.ArtifactKey) {
		t.Error("retained backup must be untouched")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	s := newStore(t)
	keepM, dropM := at(hoursAgo(1)), at(daysAgo(90))
	seed(t, s, keepM, dropM)

	plan := retention.Compute([]*manifest.Manifest{keepM, dropM}, retention.Policy{KeepLast: 1}, now)
	if _, err := retention.Apply(context.Background(), s, plan); err != nil {
		t.Fatalf("first retention.Apply: %v", err)
	}
	// Re-running the same plan must not error, so a partially-failed prune is
	// safe to retry.
	if _, err := retention.Apply(context.Background(), s, plan); err != nil {
		t.Fatalf("second retention.Apply must be a no-op, got: %v", err)
	}
}

func TestSweepOrphansRemovesArtifactWithoutManifest(t *testing.T) {
	s := newStore(t)
	live := at(hoursAgo(1))
	seed(t, s, live)

	// An artifact whose manifest never landed, from long enough ago that it
	// cannot be an in-flight backup.
	orphan := at(daysAgo(5))
	if err := s.Put(context.Background(), orphan.ArtifactKey, strings.NewReader(strings.Repeat("x", 1000))); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	removed, freed, err := retention.SweepOrphans(context.Background(), s, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("retention.SweepOrphans: %v", err)
	}
	if removed != 1 || freed != 1000 {
		t.Errorf("removed = %d, freed = %d, want 1 and 1000", removed, freed)
	}
	if exists(t, s, orphan.ArtifactKey) {
		t.Error("orphaned artifact should be gone")
	}
	if !exists(t, s, live.ArtifactKey) {
		t.Error("an artifact with a manifest must never be swept")
	}
}

// TestSweepOrphansSparesInFlightBackup guards the race: a backup uploads its
// artifact before writing its manifest, so a fresh manifest-less artifact is
// indistinguishable from a live backup in progress.
func TestSweepOrphansSparesInFlightBackup(t *testing.T) {
	s := newStore(t)
	inFlight := at(now) // artifact just uploaded, manifest not written yet
	if err := s.Put(context.Background(), inFlight.ArtifactKey, strings.NewReader("partial")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	removed, _, err := retention.SweepOrphans(context.Background(), s, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("retention.SweepOrphans: %v", err)
	}
	if removed != 0 {
		t.Error("swept an artifact younger than minAge, racing an in-flight backup")
	}
	if !exists(t, s, inFlight.ArtifactKey) {
		t.Error("in-flight artifact must survive")
	}
}

func TestSweepOrphansIgnoresUnrecognizableKeys(t *testing.T) {
	s := newStore(t)
	if err := s.Put(context.Background(), "artifacts/not-a-timestamp.dump.enc", strings.NewReader("x")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	removed, _, err := retention.SweepOrphans(context.Background(), s, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("retention.SweepOrphans: %v", err)
	}
	if removed != 0 {
		t.Error("a key we cannot date is not ours to delete")
	}
}
