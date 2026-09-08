package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Slack posts notifications to an incoming webhook.
//
// This exists to prove the Notifier interface carries a second channel
// without changes elsewhere; it is disabled unless a webhook is configured.
type Slack struct {
	WebhookURL string
	Client     *http.Client

	on bool
}

// NewSlack creates a Slack notifier.
func NewSlack(enabled bool, webhookURL string) *Slack {
	return &Slack{
		WebhookURL: webhookURL,
		Client:     &http.Client{Timeout: 10 * time.Second},
		on:         enabled && webhookURL != "",
	}
}

// Name implements Notifier.
func (s *Slack) Name() string { return "slack" }

// Enabled implements Notifier.
func (s *Slack) Enabled() bool { return s.on }

// Notify implements Notifier.
func (s *Slack) Notify(ctx context.Context, ev Event) error {
	payload := map[string]any{
		"text": ev.Title,
		"blocks": []map[string]any{
			{
				"type": "section",
				"text": map[string]string{"type": "mrkdwn", "text": "*" + ev.Title + "*"},
			},
			{
				"type": "section",
				"text": map[string]string{"type": "mrkdwn", "text": ev.Body},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build Slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("post to Slack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Slack webhook returned %s", resp.Status)
	}
	return nil
}
