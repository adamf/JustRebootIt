// Package config loads the prober configuration: the targets to probe and the
// timing/behavior knobs that control how probing happens.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Target is a single host to probe with pings and (optionally) traceroutes.
type Target struct {
	// Name is a short, human-friendly label used in metrics and dashboards
	// (e.g. "cloudflare-dns", "comcast-gw"). Keep it stable over time so
	// historical data lines up.
	Name string `yaml:"name"`
	// Host is the address to probe: an IP or a DNS name.
	Host string `yaml:"host"`
	// Group categorizes the target so the dashboard can separate, say, the
	// first upstream hop from distant anchors (e.g. "gateway", "isp",
	// "anchor"). Optional.
	Group string `yaml:"group"`
	// Trace enables traceroutes to this target. Tracing every target every
	// cycle is noisy; enable it for a representative subset.
	Trace bool `yaml:"trace"`
}

// Config is the top-level prober configuration.
type Config struct {
	// Interval is the length of one probe cycle (smokeping's "step"). All of
	// a target's pings for a cycle are spread across this window and then a
	// single set of summary statistics is published.
	Interval time.Duration `yaml:"interval"`
	// Pings is the number of echo requests sent to each target per cycle. More
	// pings give a smoother latency distribution (the smokeping "smoke") at
	// the cost of more traffic.
	Pings int `yaml:"pings"`
	// Timeout is how long to wait for a single echo reply before counting it
	// as lost. It must be comfortably smaller than Interval.
	Timeout time.Duration `yaml:"timeout"`
	// Privileged selects raw ICMP sockets (CAP_NET_RAW) when true, or
	// unprivileged datagram ICMP when false. Raw sockets are the most
	// portable inside a container that has been granted NET_RAW.
	Privileged bool `yaml:"privileged"`

	// TraceInterval is how often traceroutes run for trace-enabled targets.
	// Traceroutes are heavier and the path changes slowly, so this is usually
	// several multiples of Interval.
	TraceInterval time.Duration `yaml:"trace_interval"`
	// TraceMaxHops bounds how far a traceroute will probe.
	TraceMaxHops int `yaml:"trace_max_hops"`
	// TraceTimeout is the per-hop wait for an ICMP time-exceeded reply.
	TraceTimeout time.Duration `yaml:"trace_timeout"`

	// ListenAddr is the address the Prometheus metrics endpoint listens on.
	ListenAddr string `yaml:"listen_addr"`

	// Targets is the list of hosts to probe.
	Targets []Target `yaml:"targets"`
}

// Default returns a configuration with sane defaults already applied. Loading a
// file layers user values on top of these.
func Default() Config {
	return Config{
		Interval:      10 * time.Second,
		Pings:         20,
		Timeout:       2 * time.Second,
		Privileged:    true,
		TraceInterval: 60 * time.Second,
		TraceMaxHops:  30,
		TraceTimeout:  2 * time.Second,
		ListenAddr:    ":9430",
	}
}

// Load reads configuration from path, applying defaults for any field the file
// leaves unset, then validates the result.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that the configuration is internally consistent and usable.
func (c Config) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("no targets configured")
	}
	if c.Pings < 1 {
		return fmt.Errorf("pings must be >= 1, got %d", c.Pings)
	}
	if c.Interval <= 0 {
		return fmt.Errorf("interval must be > 0")
	}
	if c.Timeout >= c.Interval {
		return fmt.Errorf("timeout (%s) must be smaller than interval (%s)", c.Timeout, c.Interval)
	}
	seen := make(map[string]struct{}, len(c.Targets))
	for i, t := range c.Targets {
		if t.Name == "" {
			return fmt.Errorf("target %d has no name", i)
		}
		if t.Host == "" {
			return fmt.Errorf("target %q has no host", t.Name)
		}
		if _, dup := seen[t.Name]; dup {
			return fmt.Errorf("duplicate target name %q", t.Name)
		}
		seen[t.Name] = struct{}{}
	}
	return nil
}
