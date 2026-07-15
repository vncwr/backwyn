package check_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/storage"

	"github.com/vncwr/backwyn/internal/check"
)

var now = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// store returns a Local-backed store seeded with the given manifests. Using the
// real Local backend rather than a fake keeps the List/Get contract honest.
func store(t *testing.T, ms ...*manifest.Manifest) storage.Backend {
	t.Helper()
	s, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	for _, m := range ms {
		pr, pw := io.Pipe()
		go func(m *manifest.Manifest) { pw.CloseWithError(m.Encode(pw)) }(m)
		if err := s.Put(context.Background(), manifest.ManifestKey(m.ID), pr); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	return s
}

// verified builds a manifest created age ago that passed verification.
func verified(id string, age time.Duration) *manifest.Manifest {
	return &manifest.Manifest{
		SchemaVersion: 1,
		ID:            id,
		CreatedAt:     now.Add(-age),
		Verification: manifest.Verification{
			Verified:   true,
			VerifiedAt: now.Add(-age),
			ChecksumOK: true,
			Listable:   true,
			Restored:   true,
			TableCount: 12,
		},
	}
}

// broken builds a manifest created age ago whose verification failed.
func broken(id string, age time.Duration, reason string) *manifest.Manifest {
	return &manifest.Manifest{
		SchemaVersion: 1,
		ID:            id,
		CreatedAt:     now.Add(-age),
		Verification:  manifest.Verification{Verified: false, Error: reason},
	}
}

func run(t *testing.T, s storage.Backend, maxAge time.Duration) *check.Report {
	t.Helper()
	rep, err := check.Run(context.Background(), s, maxAge, now)
	if err != nil {
		t.Fatalf("check.Run: %v", err)
	}
	return rep
}

func joined(ss []string) string { return strings.Join(ss, "; ") }

func TestNoBackupsAtAllIsUnhealthy(t *testing.T) {
	rep := run(t, store(t), 24*time.Hour)

	if rep.Healthy {
		t.Error("no backups must not be healthy")
	}
	if rep.LastVerified != nil || rep.LatestBackup != nil {
		t.Error("no backups should leave LastVerified and LatestBackup nil")
	}
	if !strings.Contains(joined(rep.Reasons), "no backups exist") {
		t.Errorf("reasons = %v, want mention of no backups", rep.Reasons)
	}
}

func TestFreshVerifiedIsHealthy(t *testing.T) {
	rep := run(t, store(t, verified("20260715T060000Z", 6*time.Hour)), 24*time.Hour)

	if !rep.Healthy {
		t.Fatalf("a 6h-old verified backup must be healthy under a 24h threshold: %v", rep.Reasons)
	}
	if rep.LastVerified == nil || rep.LastVerified.ID != "20260715T060000Z" {
		t.Errorf("LastVerified = %v, want the seeded backup", rep.LastVerified)
	}
	if rep.LastVerifiedAge != 6*time.Hour {
		t.Errorf("LastVerifiedAge = %s, want 6h", rep.LastVerifiedAge)
	}
}

// TestReasonsEmptyWhenHealthy pins the contract stated on check.Report.Reasons.
// It matters because the CLI only prints Reasons on the unhealthy path, so a
// reason attached to a healthy report is a reason nobody ever reads.
func TestReasonsEmptyWhenHealthy(t *testing.T) {
	s := store(t,
		verified("20260715T060000Z", 6*time.Hour),
		broken("20260715T110000Z", 1*time.Hour, "checksum mismatch"),
	)
	rep := run(t, s, 24*time.Hour)

	if !rep.Healthy {
		t.Fatalf("older verified backup within the window means coverage is healthy: %v", rep.Reasons)
	}
	if len(rep.Reasons) != 0 {
		t.Errorf("Reasons must be empty when healthy, got %v", rep.Reasons)
	}
}

// TestBrokenLatestIsWarnedAboutEvenWhenHealthy is the silent-failure case this
// tool exists to catch: older coverage is still good, so the operator is not
// yet in danger, but their most recent backup is broken and they must hear
// about it rather than see a bare "OK".
func TestBrokenLatestIsWarnedAboutEvenWhenHealthy(t *testing.T) {
	s := store(t,
		verified("20260715T060000Z", 6*time.Hour),
		broken("20260715T110000Z", 1*time.Hour, "checksum mismatch"),
	)
	rep := run(t, s, 24*time.Hour)

	if !rep.Healthy {
		t.Fatalf("expected healthy coverage from the older verified backup: %v", rep.Reasons)
	}
	w := joined(rep.Warnings)
	if !strings.Contains(w, "20260715T110000Z") {
		t.Errorf("warnings must name the broken latest backup, got %v", rep.Warnings)
	}
	if !strings.Contains(w, "checksum mismatch") {
		t.Errorf("warnings must carry the failure reason, got %v", rep.Warnings)
	}
}

func TestStaleVerifiedIsUnhealthy(t *testing.T) {
	rep := run(t, store(t, verified("20260713T120000Z", 48*time.Hour)), 24*time.Hour)

	if rep.Healthy {
		t.Error("a 48h-old verified backup must not be healthy under a 24h threshold")
	}
	if rep.LastVerified == nil {
		t.Fatal("LastVerified should still report the stale backup")
	}
	if !strings.Contains(joined(rep.Reasons), "older than the 24h0m0s threshold") {
		t.Errorf("reasons = %v, want mention of the threshold", rep.Reasons)
	}
}

func TestBackupsExistButNoneVerifiedIsUnhealthy(t *testing.T) {
	s := store(t,
		broken("20260715T060000Z", 6*time.Hour, "pg_restore --list: bad header"),
		broken("20260715T110000Z", 1*time.Hour, "checksum mismatch"),
	)
	rep := run(t, s, 24*time.Hour)

	if rep.Healthy {
		t.Error("unverified backups must not count as coverage")
	}
	if rep.LastVerified != nil {
		t.Error("LastVerified must be nil when nothing is verified")
	}
	if !strings.Contains(joined(rep.Reasons), "none are verified") {
		t.Errorf("reasons = %v, want mention that none are verified", rep.Reasons)
	}
}

// TestPicksMostRecentVerified guards the ordering: freshness must be measured
// from the newest verified backup, not whichever the store happened to list.
func TestPicksMostRecentVerified(t *testing.T) {
	s := store(t,
		verified("20260713T120000Z", 48*time.Hour),
		verified("20260715T100000Z", 2*time.Hour),
		verified("20260714T120000Z", 24*time.Hour),
	)
	rep := run(t, s, 24*time.Hour)

	if !rep.Healthy {
		t.Fatalf("expected healthy: %v", rep.Reasons)
	}
	if rep.LastVerified.ID != "20260715T100000Z" {
		t.Errorf("LastVerified = %s, want the newest verified backup", rep.LastVerified.ID)
	}
	if rep.LastVerifiedAge != 2*time.Hour {
		t.Errorf("LastVerifiedAge = %s, want 2h", rep.LastVerifiedAge)
	}
	if rep.LatestBackup.ID != "20260715T100000Z" {
		t.Errorf("LatestBackup = %s, want the newest backup", rep.LatestBackup.ID)
	}
}

// TestBoundaryIsInclusive: a backup exactly at the threshold is still covered.
func TestBoundaryIsInclusive(t *testing.T) {
	rep := run(t, store(t, verified("20260714T120000Z", 24*time.Hour)), 24*time.Hour)

	if !rep.Healthy {
		t.Errorf("a backup exactly at the threshold must count as healthy: %v", rep.Reasons)
	}
}

func TestUnverifiedLatestDoesNotMaskOlderCoverageAge(t *testing.T) {
	s := store(t,
		verified("20260715T090000Z", 3*time.Hour),
		broken("20260715T113000Z", 30*time.Minute, "decrypt: message authentication failed"),
	)
	rep := run(t, s, 24*time.Hour)

	// Age must come from the verified backup, not the newer broken one.
	if rep.LastVerifiedAge != 3*time.Hour {
		t.Errorf("LastVerifiedAge = %s, want 3h (from the verified backup)", rep.LastVerifiedAge)
	}
	if rep.LatestBackup.ID != "20260715T113000Z" {
		t.Errorf("LatestBackup = %s, want the broken newer backup", rep.LatestBackup.ID)
	}
}
