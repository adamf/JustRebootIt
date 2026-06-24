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

func TestProbePlanJitter(t *testing.T) {
	cfg := Default() // Pings=20, Interval=10s, JitterPings=60, JitterInterval=5s
	normal := Target{Name: "n"}
	if p, iv := cfg.ProbePlan(normal); p != cfg.Pings || iv != cfg.Interval {
		t.Errorf("normal target = (%d,%s), want (%d,%s)", p, iv, cfg.Pings, cfg.Interval)
	}

	jit := Target{Name: "j", Jitter: true}
	if p, iv := cfg.ProbePlan(jit); p != 60 || iv != 5*time.Second {
		t.Errorf("jitter target = (%d,%s), want (60,5s)", p, iv)
	}

	// JitterPings<=0 disables the profile: jitter targets fall back to normal.
	cfg.JitterPings = 0
	if p, iv := cfg.ProbePlan(jit); p != cfg.Pings || iv != cfg.Interval {
		t.Errorf("disabled profile = (%d,%s), want (%d,%s)", p, iv, cfg.Pings, cfg.Interval)
	}

	// JitterInterval<=0 reuses the global interval.
	cfg = Default()
	cfg.JitterInterval = 0
	if _, iv := cfg.ProbePlan(jit); iv != cfg.Interval {
		t.Errorf("zero jitter_interval should reuse global interval, got %s", iv)
	}
}

func TestValidateJitterTimeout(t *testing.T) {
	cfg := Default()
	cfg.Timeout = 6 * time.Second // >= jitter_interval (5s)
	cfg.Targets = []Target{{Name: "a", Host: "h"}}
	if err := cfg.Validate(); err == nil {
		t.Error("timeout >= jitter_interval should be rejected")
	}
}
