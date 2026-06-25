package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "targets.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeBoth writes a targets.yml and a sibling overrides.yml in the same temp
// dir and returns the targets.yml path, so Load auto-discovers the overrides.
func writeBoth(t *testing.T, base, overrides string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "targets.yml")
	if err := os.WriteFile(p, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "overrides.yml"), []byte(overrides), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOverridesAdditiveMerge(t *testing.T) {
	base := `
pings: 20
targets:
  - name: home-gateway
    host: 192.168.1.1
    group: gateway
  - name: cloudflare
    host: 1.1.1.1
    group: anchor
`
	overrides := `
pings: 30
targets:
  - name: home-gateway
    host: 10.0.0.1
    group: gateway
  - name: plex-peer
    host: 203.0.113.40
    group: streaming
    trace: true
`
	cfg, err := Load(writeBoth(t, base, overrides))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Scalar overridden.
	if cfg.Pings != 30 {
		t.Errorf("Pings = %d, want 30 from overrides", cfg.Pings)
	}
	// Same-name target replaced in place; new-name target appended; others kept.
	byName := map[string]Target{}
	for _, tg := range cfg.Targets {
		byName[tg.Name] = tg
	}
	if len(cfg.Targets) != 3 {
		t.Fatalf("got %d targets, want 3: %+v", len(cfg.Targets), cfg.Targets)
	}
	if byName["home-gateway"].Host != "10.0.0.1" {
		t.Errorf("home-gateway host = %q, want 10.0.0.1 (replaced)", byName["home-gateway"].Host)
	}
	if byName["cloudflare"].Host != "1.1.1.1" {
		t.Errorf("cloudflare host = %q, want 1.1.1.1 (untouched)", byName["cloudflare"].Host)
	}
	if p, ok := byName["plex-peer"]; !ok || !p.Trace || p.Group != "streaming" {
		t.Errorf("plex-peer not appended correctly: %+v", p)
	}
}

func TestOverridesMissingFileIsNoOp(t *testing.T) {
	// writeTemp creates only targets.yml; the sibling overrides.yml is absent.
	cfg, err := Load(writeTemp(t, `
targets:
  - name: a
    host: 1.1.1.1
`))
	if err != nil {
		t.Fatalf("load without overrides should succeed: %v", err)
	}
	if cfg.Pings != 20 {
		t.Errorf("defaults should still apply, Pings = %d", cfg.Pings)
	}
}

func TestOverridesExplicitMissingIsError(t *testing.T) {
	base := writeTemp(t, "targets:\n  - {name: a, host: 1.1.1.1}\n")
	if _, err := LoadWithOverrides(base, filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Error("an explicitly named, missing overrides file should error")
	}
}

func TestOverridesMergeCandidatesByName(t *testing.T) {
	base := `
targets:
  - name: a
    host: 1.1.1.1
discovery:
  enabled: true
  candidates:
    - { name: c1, host: 9.9.9.9 }
`
	overrides := `
discovery:
  candidates:
    - { name: c2, host: 8.8.4.4 }
`
	cfg, err := Load(writeBoth(t, base, overrides))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := map[string]bool{}
	for _, c := range cfg.Discovery.Candidates {
		names[c.Name] = true
	}
	if !names["c1"] || !names["c2"] {
		t.Errorf("candidates should merge by name, got %+v", cfg.Discovery.Candidates)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	p := writeTemp(t, `
targets:
  - name: cloudflare
    host: 1.1.1.1
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Pings != 20 {
		t.Errorf("Pings = %d, want default 20", cfg.Pings)
	}
	if cfg.Interval != 10*time.Second {
		t.Errorf("Interval = %s, want default 10s", cfg.Interval)
	}
	if cfg.ListenAddr != ":9430" {
		t.Errorf("ListenAddr = %q, want default :9430", cfg.ListenAddr)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Host != "1.1.1.1" {
		t.Errorf("unexpected targets: %+v", cfg.Targets)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	p := writeTemp(t, `
interval: 30s
pings: 10
listen_addr: :9999
targets:
  - name: g
    host: 8.8.8.8
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Interval != 30*time.Second || cfg.Pings != 10 || cfg.ListenAddr != ":9999" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

// TestShippedConfigLoads guards the committed config/targets.yml against drift:
// it must parse and validate with the real defaults.
func TestShippedConfigLoads(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "config", "targets.yml")); err != nil {
		t.Fatalf("shipped config/targets.yml failed to load: %v", err)
	}
}

func baseUnderload() Config {
	c := Default()
	c.Targets = []Target{{Name: "a", Host: "h"}}
	c.Underload.Enabled = true
	c.Underload.Target = "uplink"
	c.Underload.Host = "1.1.1.1"
	return c
}

func TestValidateUnderload(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid defaults", func(c *Config) {}, false},
		{"missing host", func(c *Config) { c.Underload.Host = "" }, true},
		{"missing target label", func(c *Config) { c.Underload.Target = "" }, true},
		{"bad direction", func(c *Config) { c.Underload.Direction = "sideways" }, true},
		{"zero streams", func(c *Config) { c.Underload.Streams = 0 }, true},
		{"timeout too large for duration", func(c *Config) { c.Underload.Timeout = 10 * time.Second }, true},
		{"up needs up_url", func(c *Config) { c.Underload.Direction = "up"; c.Underload.UpURL = "" }, true},
		{"down needs down_url", func(c *Config) { c.Underload.DownURL = "" }, true},
		{"disabled skips checks", func(c *Config) { c.Underload.Enabled = false; c.Underload.Host = "" }, false},
		// The manual button shares the transfer settings, so they're validated
		// even when the scheduled probe is off — but host/target are not required.
		{"manual-only valid without host", func(c *Config) {
			c.Underload.Enabled = false
			c.Underload.Host = ""
			c.Underload.Target = ""
			c.Underload.Manual.Enabled = true
		}, false},
		{"manual-only validates transfer params", func(c *Config) {
			c.Underload.Enabled = false
			c.Underload.Manual.Enabled = true
			c.Underload.Direction = "sideways"
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseUnderload()
			tc.mutate(&c)
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"no targets", Config{Interval: time.Second, Pings: 1}, true},
		{"zero pings", Config{Interval: time.Second, Pings: 0, Targets: []Target{{Name: "a", Host: "h"}}}, true},
		{"timeout >= interval", Config{Interval: time.Second, Timeout: time.Second, Pings: 1, Targets: []Target{{Name: "a", Host: "h"}}}, true},
		{"duplicate name", Config{Interval: time.Second, Timeout: time.Millisecond, Pings: 1, Targets: []Target{{Name: "a", Host: "h"}, {Name: "a", Host: "h2"}}}, true},
		{"missing host", Config{Interval: time.Second, Timeout: time.Millisecond, Pings: 1, Targets: []Target{{Name: "a"}}}, true},
		{"valid", Config{Interval: time.Second, Timeout: time.Millisecond, Pings: 1, Targets: []Target{{Name: "a", Host: "h"}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
