package runner

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// tracker maintains in-memory state of the last backup and verify cycles.
type Tracker struct {
	mu sync.RWMutex

	lastBackupTime     time.Time
	lastBackupStatus   string
	lastBackupSize     int64
	lastBackupDuration time.Duration

	lastVerifyTime   time.Time
	lastVerifyStatus string
	lastTableCount   int
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

// startserver starts the healthz and metrics HTTP server.
func StartServer(addr string, tracker *Tracker) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		tracker.mu.RLock()
		defer tracker.mu.RUnlock()

		if tracker.lastBackupTime.IsZero() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("starting"))
			return
		}

		if tracker.lastBackupStatus == "success" && tracker.lastVerifyStatus == "success" {
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

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_time_seconds Epoch timestamp of the last backup attempt.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_time_seconds gauge\n")
		var lastBackupSec int64
		if !tracker.lastBackupTime.IsZero() {
			lastBackupSec = tracker.lastBackupTime.Unix()
		}
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_time_seconds %d\n\n", lastBackupSec)

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_success Indicates if the last backup succeeded (1) or failed (0).\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_success gauge\n")
		lastBackupSuccess := 0
		if tracker.lastBackupStatus == "success" {
			lastBackupSuccess = 1
		}
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_success %d\n\n", lastBackupSuccess)

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_size_bytes Size of the last encrypted backup artifact in bytes.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_size_bytes gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_size_bytes %d\n\n", tracker.lastBackupSize)

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_backup_duration_seconds Duration of the last backup in seconds.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_backup_duration_seconds gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_backup_duration_seconds %.3f\n\n", tracker.lastBackupDuration.Seconds())

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_verify_time_seconds Epoch timestamp of the last verification attempt.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_verify_time_seconds gauge\n")
		var lastVerifySec int64
		if !tracker.lastVerifyTime.IsZero() {
			lastVerifySec = tracker.lastVerifyTime.Unix()
		}
		_, _ = fmt.Fprintf(w, "backwyn_last_verify_time_seconds %d\n\n", lastVerifySec)

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_verify_success Indicates if the last verification succeeded (1) or failed (0).\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_verify_success gauge\n")
		lastVerifySuccess := 0
		if tracker.lastVerifyStatus == "success" {
			lastVerifySuccess = 1
		}
		_, _ = fmt.Fprintf(w, "backwyn_last_verify_success %d\n\n", lastVerifySuccess)

		_, _ = fmt.Fprintf(w, "# HELP backwyn_last_table_count Number of tables successfully restored in the last verification.\n")
		_, _ = fmt.Fprintf(w, "# TYPE backwyn_last_table_count gauge\n")
		_, _ = fmt.Fprintf(w, "backwyn_last_table_count %d\n", tracker.lastTableCount)
	})

	slog.Info("starting observability HTTP server", "addr", addr)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil && err != http.ErrServerClosed {
			slog.Error("observability HTTP server error", "err", err)
		}
	}()
}
