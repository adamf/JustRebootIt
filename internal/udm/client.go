// Package udm reads operational statistics from a UniFi Dream Machine Pro (or
// other UniFi OS gateway) so WAN throughput, gateway load, and the gateway's
// own latency/speedtest numbers can be correlated against externally measured
// latency. It speaks the local UniFi OS API: authenticate once, then read the
// proxied Network application endpoints.
package udm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"
)

// FlexFloat tolerates UniFi's habit of returning the same field as a JSON
// number in one place and a quoted string in another (notably the "*-r" rate
// fields). It unmarshals either form into a float64.
type FlexFloat float64

// UnmarshalJSON accepts numbers, quoted numbers, and null/empty as zero.
func (f *FlexFloat) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		// UniFi pads some string-encoded numbers with surrounding whitespace
		// (e.g. system-stats cpu/mem come back as " 80.2"), which ParseFloat
		// rejects; trim before parsing and treat all-whitespace as zero.
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*f = FlexFloat(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = FlexFloat(v)
	return nil
}

// Float returns the value as a plain float64.
func (f FlexFloat) Float() float64 { return float64(f) }

// Config describes how to reach and authenticate to the gateway.
type Config struct {
	// BaseURL is the gateway's controller URL, e.g. https://192.168.1.1.
	BaseURL string
	// Username and Password are a local UniFi OS account (a read-only local
	// admin is recommended rather than a Ubiquiti SSO account).
	Username string
	Password string
	// Site is the Network site name; "default" for almost all home setups.
	Site string
	// InsecureSkipVerify disables TLS verification, which is usually required
	// because the gateway presents a self-signed certificate.
	InsecureSkipVerify bool
	// Timeout bounds each HTTP request.
	Timeout time.Duration
}

// Client is an authenticated UniFi OS HTTP client. It logs in lazily and
// re-authenticates when the session expires. It is safe for sequential use by
// a single collector; it does not guard against concurrent scrapes.
type Client struct {
	cfg        Config
	http       *http.Client
	csrf       string
	authedOnce bool
}

// NewClient builds a Client. A cookie jar carries the session token across
// requests; TLS verification follows cfg.InsecureSkipVerify.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Site == "" {
		cfg.Site = "default"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Jar: jar, Timeout: cfg.Timeout, Transport: tr},
	}, nil
}

// login authenticates against the UniFi OS auth endpoint and captures the CSRF
// token returned for subsequent state-changing requests.
func (c *Client) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{
		"username": c.cfg.Username,
		"password": c.cfg.Password,
		"remember": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login failed: status %d", resp.StatusCode)
	}
	// UniFi OS returns the CSRF token in a response header; keep whichever of
	// the known header names is present.
	if v := resp.Header.Get("x-csrf-token"); v != "" {
		c.csrf = v
	} else if v := resp.Header.Get("x-updated-csrf-token"); v != "" {
		c.csrf = v
	}
	c.authedOnce = true
	return nil
}

// getJSON fetches a Network-application path (relative to /proxy/network) and
// decodes it into out. It logs in on first use and retries once after a
// re-login if the session has expired (HTTP 401).
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	if !c.authedOnce {
		if err := c.login(ctx); err != nil {
			return err
		}
	}
	status, data, err := c.doGet(ctx, path)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		if err := c.login(ctx); err != nil {
			return err
		}
		if status, data, err = c.doGet(ctx, path); err != nil {
			return err
		}
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, status)
	}
	return json.Unmarshal(data, out)
}

func (c *Client) doGet(ctx context.Context, path string) (int, []byte, error) {
	url := c.cfg.BaseURL + "/proxy/network" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if c.csrf != "" {
		req.Header.Set("x-csrf-token", c.csrf)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if v := resp.Header.Get("x-updated-csrf-token"); v != "" {
		c.csrf = v
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// drain consumes and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
