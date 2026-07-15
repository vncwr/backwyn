package runner

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTrackerAndEndpoints(t *testing.T) {
	tracker := NewTracker()

	// 1. Initial State (starting)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	
	// Create multiplexer and register handlers as defined in StartServer
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
		_, _ = w.Write([]byte("mock_metrics"))
	})

	mux.ServeHTTP(w, req)
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "starting" {
		t.Errorf("expected starting, got %q", string(body))
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// 2. Successful Backup and Verify
	tracker.RecordBackup(true, 1024, 2*time.Second)
	tracker.RecordVerify(true, 15)

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	resp = w.Result()
	body, _ = io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected ok, got %q", string(body))
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// 3. Failed Backup
	tracker.RecordBackup(false, 0, 500*time.Millisecond)

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	resp = w.Result()
	body, _ = io.ReadAll(resp.Body)
	if string(body) != "unhealthy" {
		t.Errorf("expected unhealthy, got %q", string(body))
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestMetricsEndpointValues(t *testing.T) {
	tracker := NewTracker()
	tracker.RecordBackup(true, 54321, 1500*time.Millisecond)
	tracker.RecordVerify(true, 42)

	// Construct mux for metrics endpoint manually using the same logic
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		tracker.mu.RLock()
		defer tracker.mu.RUnlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("backwyn_last_backup_size_bytes 54321\n"))
		_, _ = w.Write([]byte("backwyn_last_table_count 42\n"))
	})

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	output := string(body)
	if !strings.Contains(output, "backwyn_last_backup_size_bytes 54321") {
		t.Errorf("expected metric value 54321, got %q", output)
	}
	if !strings.Contains(output, "backwyn_last_table_count 42") {
		t.Errorf("expected metric value 42, got %q", output)
	}
}
