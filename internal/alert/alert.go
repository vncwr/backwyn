// package alert delivers notifications when a cycle fails or coverage becomes
// unhealthy.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// describes the severity of an event
type Level string

const (
	LevelInfo Level = "info"
	// LevelWarn: not an outage yet, but becomes one if ignored.
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// a single notification.
type Event struct {
	Level  Level     `json:"level"`
	Title  string    `json:"title"`
	Detail string    `json:"detail"`
	Time   time.Time `json:"time"`
}

// delivers events somewhere a human or system will see them
type Alerter interface {
	Alert(ctx context.Context, e Event) error
}

// returns a Webhook alerter if url is non-empty, otherwise a no-op.
func New(webhookURL string) Alerter {
	if webhookURL == "" {
		return Noop{}
	}
	return &Webhook{URL: webhookURL, client: &http.Client{Timeout: 10 * time.Second}}
}

// discards events (used when no alerting is configured).
type Noop struct{}

// implements Alerter.
func (Noop) Alert(context.Context, Event) error { return nil }

// POSTs events as JSON to a URL (Slack-compatible incoming webhooks, a custom endpoint, etc.)
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
