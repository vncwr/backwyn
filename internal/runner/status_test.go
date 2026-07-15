package runner

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	resp := w.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestHealthzFollowsCoverage(t *testing.T) {
	tracker := NewTracker()
	h := Handler(tracker)

	// before the first check: not healthy — nothing is proven yet.
	code, body := get(t, h, "/healthz")
	if code != http.StatusServiceUnavailable || body != "starting" {
		t.Errorf("before first check: got %d %q, want 503 starting", code, body)
	}

	// a healthy coverage check turns it ok.
	tracker.RecordCheck(true, time.Now())
	code, body = get(t, h, "/healthz")
	if code != http.StatusOK || body != "ok" {
		t.Errorf("healthy coverage: got %d %q, want 200 ok", code, body)
	}

	// this cycle's backup failing does NOT flip health while coverage holds:
	// an older verified backup is still carrying it.
	tracker.RecordBackup(false, 0, time.Second)
	tracker.RecordVerify(false, 0)
	code, body = get(t, h, "/healthz")
	if code != http.StatusOK || body != "ok" {
		t.Errorf("failed cycle, healthy coverage: got %d %q, want 200 ok", code, body)
	}

	// coverage going stale is what flips health.
	tracker.RecordCheck(false, time.Now().Add(-48*time.Hour))
	code, body = get(t, h, "/healthz")
	if code != http.StatusServiceUnavailable || body != "unhealthy" {
		t.Errorf("unhealthy coverage: got %d %q, want 503 unhealthy", code, body)
	}
}

func TestMetricsExposeCoverageAndCycleState(t *testing.T) {
	tracker := NewTracker()
	h := Handler(tracker)

	// before anything runs, gauges must exist and read zero, not be absent.
	_, body := get(t, h, "/metrics")
	for _, want := range []string{
		"backwyn_coverage_healthy 0",
		"backwyn_last_verified_backup_time_seconds 0",
		"backwyn_last_check_time_seconds 0",
		"backwyn_last_backup_time_seconds 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("initial metrics missing %q", want)
		}
	}

	verified := time.Now().Add(-2 * time.Hour)
	tracker.RecordBackup(true, 54321, 1500*time.Millisecond)
	tracker.RecordVerify(true, 42)
	tracker.RecordCheck(true, verified)

	code, body := get(t, h, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status: got %d, want 200", code)
	}
	for _, want := range []string{
		"backwyn_coverage_healthy 1",
		"backwyn_last_verified_backup_time_seconds " + strconv.FormatInt(verified.Unix(), 10),
		"backwyn_last_backup_size_bytes 54321",
		"backwyn_last_backup_duration_seconds 1.500",
		"backwyn_last_backup_success 1",
		"backwyn_last_verify_success 1",
		"backwyn_last_table_count 42",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\ngot:\n%s", want, body)
		}
	}

	// a failed verify with stale coverage reads 0 across the board.
	tracker.RecordVerify(false, 0)
	tracker.RecordCheck(false, verified)
	_, body = get(t, h, "/metrics")
	for _, want := range []string{
		"backwyn_coverage_healthy 0",
		"backwyn_last_verify_success 0",
		"backwyn_last_table_count 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("after failure, metrics missing %q", want)
		}
	}
}
