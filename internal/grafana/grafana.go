// Package grafana posts native Grafana annotations. The AI diagnostics use it
// to write a numbered, tagged note onto the dashboard's timeline at the moment
// an event occurred, so the human-readable "why" shows up right where the
// latency spike is.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client posts annotations to a Grafana instance using basic auth.
type Client struct {
	base string
	user string
	pass string
	http *http.Client
}

// New builds a Client. base is the Grafana URL (e.g. http://grafana:3000); user
// and pass are an account allowed to create annotations (the admin account
// works). It returns nil when base/user/pass are not all set, so callers can
// treat annotation posting as best-effort.
func New(base, user, pass string) *Client {
	if base == "" || user == "" || pass == "" {
		return nil
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		user: user,
		pass: pass,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Annotation is a single dashboard note.
type Annotation struct {
	// Time is when the event occurred (the annotation marker position).
	Time time.Time
	// Tags categorize the annotation so a dashboard panel can filter for them.
	Tags []string
	// Text is the annotation body (Markdown is rendered in the tooltip).
	Text string
}

// Post creates the annotation. A nil Client is a no-op, so the caller can write
// `c.Post(...)` unconditionally.
func (c *Client) Post(ctx context.Context, a Annotation) error {
	if c == nil {
		return nil
	}
	payload := map[string]any{
		"time": a.Time.UnixMilli(),
		"tags": a.Tags,
		"text": a.Text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/annotations", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("grafana annotation: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
