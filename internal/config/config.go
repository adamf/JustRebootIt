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

	// Targets is the list of always-on hosts to probe. These are never dropped
	// by path discovery (your gateway and ISP belong here).
	Targets []Target `yaml:"targets"`

	// Discovery configures automatic selection of additional, path-diverse
	// probe targets from a candidate pool. See Discovery.
	Discovery Discovery `yaml:"discovery"`

	// Diagnostics configures the extra tests that fire when a latency spike or
	// loss event is detected. See Diagnostics.
	Diagnostics Diagnostics `yaml:"diagnostics"`
}

// Discovery automatically promotes a path-diverse subset of a candidate pool to
// active probing. Probing many targets that share the same first few hops is
// redundant — a spike on the shared segment shows up everywhere and localizes
// nothing. Discovery traces the candidates, then keeps the ones whose 2nd/3rd
// hops differ and that reach their destination in the fewest hops, so each
// active path is short and distinct (and a spike pins to a specific segment).
type Discovery struct {
	// Enabled turns discovery on. When off, only Targets are probed.
	Enabled bool `yaml:"enabled"`
	// Interval is how often the candidate pool is re-traced and re-selected.
	// Paths change slowly, so this is typically minutes.
	Interval time.Duration `yaml:"interval"`
	// MaxTargets caps how many discovered targets are promoted to active
	// probing (on top of the always-on Targets).
	MaxTargets int `yaml:"max_targets"`
	// MaxHops bounds the (short) traceroutes used during discovery. Discovery
	// only needs the first few hops plus reachability, so this is much smaller
	// than TraceMaxHops.
	MaxHops int `yaml:"max_hops"`
	// MaxReachHops drops candidates that take more than this many hops to
	// reach; far-away targets dilute localization. Zero disables the limit.
	MaxReachHops int `yaml:"max_reach_hops"`
	// Candidates is the pool discovery selects from.
	Candidates []Target `yaml:"candidates"`
}

// Diagnostics controls the deeper, on-demand tests that run the moment a target
// looks unhealthy, capturing extra signal while the problem is still happening
// (spikes are often gone by the time a human looks).
type Diagnostics struct {
	// Enabled turns latency/loss-triggered diagnostics on.
	Enabled bool `yaml:"enabled"`
	// LatencyFactor triggers diagnostics when a cycle's median RTT exceeds the
	// target's rolling baseline by this multiple (e.g. 3.0 == 3x normal).
	LatencyFactor float64 `yaml:"latency_factor"`
	// LatencyAbsMargin is an additional floor: the median must also exceed the
	// baseline by at least this much, so tiny absolute jumps on a low-latency
	// target don't trip the trigger.
	LatencyAbsMargin time.Duration `yaml:"latency_abs_margin"`
	// LossThreshold triggers diagnostics when the loss ratio meets or exceeds
	// this value, in [0,1].
	LossThreshold float64 `yaml:"loss_threshold"`
	// Cooldown is the minimum time between diagnostic runs for a single target,
	// so a sustained event doesn't launch a storm of tests.
	Cooldown time.Duration `yaml:"cooldown"`
	// BaselineAlpha is the EWMA smoothing factor (0,1] for the rolling latency
	// baseline; smaller reacts more slowly and is more robust to noise.
	BaselineAlpha float64 `yaml:"baseline_alpha"`
	// TCPPort is the port used for the TCP-handshake latency test. Many ISPs
	// deprioritize ICMP, so a TCP connect corroborates whether real traffic is
	// affected. 443 is a safe default.
	TCPPort int `yaml:"tcp_port"`
	// DNSProbe is a hostname resolved (and timed) during a diagnostic run to
	// catch DNS resolution latency, a common real-world culprit.
	DNSProbe string `yaml:"dns_probe"`
	// Workers bounds how many diagnostic runs happen concurrently.
	Workers int `yaml:"workers"`
	// AI enables an LLM-driven root-cause writeup for each event. It is fully
	// optional and only activates when an API key is provided via the
	// ANTHROPIC_API_KEY environment variable.
	AI AIDiagnostics `yaml:"ai"`
}

// AIDiagnostics configures the optional Claude-driven analysis of an event.
// Secrets and service URLs (the API key, Prometheus URL, and Grafana
// credentials) come from environment variables, not this file, so they never
// land in a committed config or image layer.
type AIDiagnostics struct {
	// Enabled turns the feature on. Even when true, it stays dormant unless
	// ANTHROPIC_API_KEY is set in the environment.
	Enabled bool `yaml:"enabled"`
	// Model is the Claude model ID. Defaults to claude-opus-4-8; set
	// claude-sonnet-4-6 for a cheaper, faster analysis.
	Model string `yaml:"model"`
	// MaxIterations bounds the agent's tool-use loop per event.
	MaxIterations int `yaml:"max_iterations"`

	// The following knobs coalesce events so a recurring incident doesn't
	// trigger (and pay for) a fresh investigation every cycle.

	// RepeatTTL: a repeat of the same event "signature" within this window
	// reuses the prior analysis instead of calling the AI again.
	RepeatTTL time.Duration `yaml:"repeat_ttl"`
	// MinInterval is the global minimum time between full investigations.
	MinInterval time.Duration `yaml:"min_interval"`
	// DailyBudget caps full investigations per rolling 24h (0 = unlimited).
	DailyBudget int `yaml:"daily_budget"`
	// SharedWindow / SharedThreshold detect a single shared upstream incident:
	// when at least SharedThreshold distinct targets trip within SharedWindow,
	// they collapse to one investigation instead of one per target.
	SharedWindow    time.Duration `yaml:"shared_window"`
	SharedThreshold int           `yaml:"shared_threshold"`

	// FarHops marks a target as "far" (not our network) when it's reached in
	// more than this many hops. Far problems are reused for FarRepeatTTL and
	// investigated with the cheaper model.
	FarHops int `yaml:"far_hops"`
	// FarRepeatTTL is the (longer) reuse window for far problems.
	FarRepeatTTL time.Duration `yaml:"far_repeat_ttl"`

	// ModelEval, when on, evaluates the cheap model against the expensive one
	// per problem class and locks in whichever is good enough. When off, near/
	// shared problems use Model (expensive) and far problems use ModelCheap.
	ModelEval bool `yaml:"model_eval"`
	// ModelCheap is the cheaper model (default claude-sonnet-4-6).
	ModelCheap string `yaml:"model_cheap"`
	// EvalSamples is how many dual-model evaluations to run per class before
	// deciding which model to use.
	EvalSamples int `yaml:"eval_samples"`
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
		Discovery: Discovery{
			Enabled:      true,
			Interval:     15 * time.Minute,
			MaxTargets:   6,
			MaxHops:      8,
			MaxReachHops: 12,
		},
		Diagnostics: Diagnostics{
			Enabled:          true,
			LatencyFactor:    3.0,
			LatencyAbsMargin: 30 * time.Millisecond,
			LossThreshold:    0.1,
			Cooldown:         60 * time.Second,
			BaselineAlpha:    0.2,
			TCPPort:          443,
			DNSProbe:         "www.google.com",
			Workers:          2,
			AI: AIDiagnostics{
				Enabled:         true,
				Model:           "claude-opus-4-8",
				MaxIterations:   12,
				RepeatTTL:       1 * time.Hour,
				MinInterval:     3 * time.Minute,
				DailyBudget:     50,
				SharedWindow:    2 * time.Minute,
				SharedThreshold: 3,
				FarHops:         3,
				FarRepeatTTL:    12 * time.Hour,
				ModelEval:       true,
				ModelCheap:      "claude-sonnet-4-6",
				EvalSamples:     3,
			},
		},
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
	// Names form metric labels and must be unique across both the always-on
	// targets and the discovery candidate pool, since a promoted candidate is
	// probed under the same name.
	seen := make(map[string]struct{}, len(c.Targets)+len(c.Discovery.Candidates))
	checkTarget := func(kind string, i int, t Target) error {
		if t.Name == "" {
			return fmt.Errorf("%s %d has no name", kind, i)
		}
		if t.Host == "" {
			return fmt.Errorf("%s %q has no host", kind, t.Name)
		}
		if _, dup := seen[t.Name]; dup {
			return fmt.Errorf("duplicate target name %q", t.Name)
		}
		seen[t.Name] = struct{}{}
		return nil
	}
	for i, t := range c.Targets {
		if err := checkTarget("target", i, t); err != nil {
			return err
		}
	}
	for i, t := range c.Discovery.Candidates {
		if err := checkTarget("candidate", i, t); err != nil {
			return err
		}
	}

	// Discovery with no candidates is simply inactive (not an error), so a
	// minimal config that only lists always-on targets still loads. The
	// interval/max bounds are only meaningful once there are candidates to act
	// on.
	if c.Discovery.Enabled && len(c.Discovery.Candidates) > 0 {
		if c.Discovery.Interval <= 0 {
			return fmt.Errorf("discovery.interval must be > 0 when discovery is enabled")
		}
		if c.Discovery.MaxTargets < 1 {
			return fmt.Errorf("discovery.max_targets must be >= 1 when discovery is enabled")
		}
	}
	if c.Diagnostics.Enabled {
		if c.Diagnostics.BaselineAlpha <= 0 || c.Diagnostics.BaselineAlpha > 1 {
			return fmt.Errorf("diagnostics.baseline_alpha must be in (0,1], got %v", c.Diagnostics.BaselineAlpha)
		}
		if c.Diagnostics.LatencyFactor < 1 {
			return fmt.Errorf("diagnostics.latency_factor must be >= 1, got %v", c.Diagnostics.LatencyFactor)
		}
		if c.Diagnostics.Workers < 1 {
			return fmt.Errorf("diagnostics.workers must be >= 1, got %d", c.Diagnostics.Workers)
		}
	}
	return nil
}
