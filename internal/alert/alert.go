// package alert delivers notifications when a cycle fails or coverage is unhealthy.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// level describes event severity.
type Level string

const (
	LevelInfo Level = "info"
	// warn level.
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// event is a notification payload.
type Event struct {
	Level  Level     `json:"level"`
	Title  string    `json:"title"`
	Detail string    `json:"detail"`
	Time   time.Time `json:"time"`
}

// alerter sends event notifications.
type Alerter interface {
	Alert(ctx context.Context, e Event) error
}

// new returns a webhook alerter if url is set, otherwise a noop.
func New(webhookURL string) Alerter {
	if webhookURL == "" {
		return Noop{}
	}
	return &Webhook{URL: webhookURL, client: &http.Client{Timeout: 10 * time.Second}}
}

// noop discards events.
type Noop struct{}

// alert implements alerter.
func (Noop) Alert(context.Context, Event) error { return nil }

// webhook posts events as json to a url.
type Webhook struct {
	URL    string
	client *http.Client
}

func (w *Webhook) Alert(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("post alert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("alert webhook returned %s", resp.Status)
	}
	return nil
}
