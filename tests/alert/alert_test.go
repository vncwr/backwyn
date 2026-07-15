package alert_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vncwr/backwyn/internal/alert"
)

func TestWebhookPostsJSON(t *testing.T) {
	var got alert.Event
	var contentType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	a := alert.New(ts.URL)
	if _, ok := a.(*alert.Webhook); !ok {
		t.Fatalf("alert.New with URL should return *alert.Webhook, got %T", a)
	}

	want := alert.Event{Level: alert.LevelError, Title: "verify failed", Detail: "checksum mismatch", Time: time.Unix(1000, 0).UTC()}
	if err := a.Alert(context.Background(), want); err != nil {
		t.Fatalf("Alert: %v", err)
	}

	if contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", contentType)
	}
	if got.Level != want.Level || got.Title != want.Title || got.Detail != want.Detail {
		t.Errorf("received %+v, want %+v", got, want)
	}
}

func TestNoopWhenNoURL(t *testing.T) {
	a := alert.New("")
	if _, ok := a.(alert.Noop); !ok {
		t.Fatalf("alert.New(\"\") should return alert.Noop, got %T", a)
	}
	if err := a.Alert(context.Background(), alert.Event{Title: "x"}); err != nil {
		t.Fatalf("alert.Noop.Alert should never error: %v", err)
	}
}
