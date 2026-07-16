package alert

import (
	"strings"
	"testing"
	"time"
)

func TestIsDiscord(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://discord.com/api/webhooks/123/token", true},
		{"https://discordapp.com/api/webhooks/123/token", true},
		{"https://canary.discord.com/api/webhooks/123/token", true},
		{"https://discord.com/other/path", false},
		{"https://example.com/api/webhooks/123", false},
		{"https://evil.com/discord.com/api/webhooks/123", false},
		{"://not a url", false},
	}
	for _, c := range cases {
		if got := isDiscord(c.url); got != c.want {
			t.Errorf("isDiscord(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestPayload(t *testing.T) {
	e := Event{Level: LevelError, Title: "backup failed", Detail: "boom", Time: time.Now()}

	if got := payload("https://example.com/hook", e); got != any(e) {
		t.Errorf("non-discord url: got %v, want raw event", got)
	}

	got, ok := payload("https://discord.com/api/webhooks/123/token", e).(map[string]string)
	if !ok {
		t.Fatalf("discord url: payload is not a message map")
	}
	if !strings.Contains(got["content"], "backup failed") || !strings.Contains(got["content"], "boom") {
		t.Errorf("discord content missing title or detail: %q", got["content"])
	}

	e.Detail = strings.Repeat("x", 3000)
	long := payload("https://discord.com/api/webhooks/123/token", e).(map[string]string)
	if n := len([]rune(long["content"])); n > 2000 {
		t.Errorf("discord content is %d runes, want <= 2000", n)
	}
}
