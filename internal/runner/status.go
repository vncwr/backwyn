package runner

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// tracker holds in-memory state of the last backup, verify, and check.
type Tracker struct {
	mu sync.RWMutex

	lastBackupTime     time.Time
	lastBackupStatus   string
	lastBackupSize     int64
	lastBackupDuration time.Duration

	lastVerifyTime   time.Time
	lastVerifyStatus string
	lastTableCount   int

	lastCheckTime      time.Time
	lastCheckHealthy   bool
	lastVerifiedBackup time.Time
}

// newtracker constructs a new status tracker.
func NewTracker() *Tracker {
	return &Tracker{}
}

// recordbackup updates the backup status state.
func (t *Tracker) RecordBackup(success bool, size int64, dur time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastBackupTime = time.Now()
	t.lastBackupSize = size
	t.lastBackupDuration = dur
	if success {
		t.lastBackupStatus = "success"
	} else {
		t.lastBackupStatus = "failed"
	}
}

// recordverify updates the verification status state.
func (t *Tracker) RecordVerify(success bool, tableCount int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastVerifyTime = time.Now()
	t.lastTableCount = tableCount
	if success {
		t.lastVerifyStatus = "success"
	} else {
		t.lastVerifyStatus = "failed"
	}
}

// recordcheck updates coverage state. lastVerified is zero when none exists.
func (t *Tracker) RecordCheck(healthy bool, lastVerified time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastCheckTime = time.Now()
	t.lastCheckHealthy = healthy
	t.lastVerifiedBackup = lastVerified
}

// handler returns the observability endpoints. /healthz is 200 only when the
// last check found a verified backup within -max-age; before the first check
// it is 503 "starting", because nothing is proven yet.
func Handler(tracker *Tracker) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		tracker.mu.RLock()
		defer tracker.mu.RUnlock()

		if tracker.lastCheckTime.IsZero() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("starting"))
			return
		}
		if tracker.lastCheckHealthy {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unhealthy"))
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		tracker.mu.RLock()
		defer tracker.mu.RUnlock()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")

		epoch := func(t time.Time) int64 {
			if t.IsZero() {
				return 0
			}
			return t.Unix()
		}
		boolGauge := func(b bool) int {
			if b {
				return 1
			}
			return 0
		}

		_, _ = fmt.Fprintf(w, "# HELP backwyn_coverage_healthy Whether the last check found a verified backup within -max-age (1) or not (0).\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_coverage_healthy gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_coverage_healthy %d\n\n", boolGauge(tracker.lastCheckHealthy))

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_verified_backup_time_seconds Epoch timestamp of the newest verified backup, 0 if none.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_verified_backup_time_seconds gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_verified_backup_time_seconds %d\n\n", epoch(tracker.lastVerifiedBackup))

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_check_time_seconds Epoch timestamp of the last coverage check, 0 before the first cycle.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_check_time_seconds gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_check_time_seconds %d\n\n", epoch(tracker.lastCheckTime))

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_time_seconds Epoch timestamp of the last backup attempt.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_time_seconds gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_time_seconds %d\n\n", epoch(tracker.lastBackupTime))

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_success Indicates if the last backup succeeded (1) or failed (0).\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_success gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_success %d\n\n", boolGauge(tracker.lastBackupStatus == "success"))

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_size_bytes Size of the last encrypted backup artifact in bytes.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_size_bytes gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_size_bytes %d\n\n", tracker.lastBackupSize)

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_duration_seconds Duration of the last backup in seconds.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_duration_seconds gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_duration_seconds %.3f\n\n", tracker.lastBackupDuration.Seconds())

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_verify_time_seconds Epoch timestamp of the last verification attempt.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_verify_time_seconds gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_verify_time_seconds %d\n\n", epoch(tracker.lastVerifyTime))

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_verify_success Indicates if the last verification succeeded (1) or failed (0).\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_verify_success gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_verify_success %d\n\n", boolGauge(tracker.lastVerifyStatus == "success"))

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_table_count Number of tables successfully restored in the last verification.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_table_count gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_table_count %d\n", tracker.lastTableCount)
	})

	return mux
}

// startserver starts the healthz and metrics HTTP server.
func StartServer(addr string, tracker *Tracker) {
	slog.Info("starting observability HTTP server", "addr", addr)
	go func() {
		if err := http.ListenAndServe(addr, Handler(tracker)); err != nil && err != http.ErrServerClosed {
			slog.Error("observability HTTP server error", "err", err)
		}
	}()
}
