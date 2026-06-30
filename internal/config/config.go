// Package config loads the prober configuration: the targets to probe and the
// timing/behavior knobs that control how probing happens.
package config

import (
	"fmt"
	"os"
	"path/filepath"
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
	// TraceProbes is how many traceroute passes run per cycle for a trace-enabled
	// target. 1 (the default) is a single path snapshot, as before. Set it higher
	// (e.g. 6) to measure PER-HOP PACKET LOSS over the passes — the evidence that
	// localizes where loss begins on a path (e.g. an ISP backbone or peering
	// hop). Heavier, so it is opt-in via this knob.
	TraceProbes int `yaml:"trace_probes"`
	// TraceASN, when true, resolves each hop's origin AS (via Team Cymru DNS) and
	// marks AS-handoff boundaries on the path — peering/transit handoffs are where
	// congestion and loss most often live. Only active when TraceProbes > 1.
	TraceASN bool `yaml:"trace_asn"`

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

	// Underload configures the latency-under-load (bufferbloat) probe. See
	// Underload.
	Underload Underload `yaml:"underload"`
}

// Underload configures the latency-under-load probe. A normal ping measures the
// link while it is idle, which is exactly when bufferbloat is invisible: the
// stutter that wrecks a Plex stream or video call happens only while the link
// is saturated, when an oversized buffer fills and RTT balloons. This probe
// deliberately saturates the link with a controlled transfer while pinging a
// stable host, then publishes the idle-vs-loaded RTT difference — the
// bufferbloat — as a metric. Because it generates real traffic it is opt-in and
// bounded (a byte ceiling and a short duration per run).
type Underload struct {
	// Enabled turns the probe on. Off by default: it moves real data.
	Enabled bool `yaml:"enabled"`
	// Interval is how often a loaded-latency test runs. Keep it generous; each
	// run briefly saturates the link.
	Interval time.Duration `yaml:"interval"`
	// Target is the label used for this probe's metrics (e.g. "uplink"). It is a
	// name only, independent of the Targets list.
	Target string `yaml:"target"`
	// Host is the address pinged while the link is under load. A stable, nearby
	// anchor (your gateway or 1.1.1.1) best isolates your own access link's
	// queue; point it at the stream's far end to measure that whole path.
	Host string `yaml:"host"`
	// Direction selects which way to load the link: "down" (saturate the
	// downlink), "up" (saturate the uplink), or "both" (measure each in turn).
	// For a Plex server pushing video, the server's uplink ("up") is the usual
	// culprit; for a client, the downlink.
	Direction string `yaml:"direction"`
	// Duration is how long each direction holds the link under load while
	// sampling RTT.
	Duration time.Duration `yaml:"duration"`
	// Streams is the number of parallel transfer connections used to saturate
	// the link. A handful is enough to fill a residential link.
	Streams int `yaml:"streams"`
	// DownURL / UpURL are the load endpoints. The defaults use Cloudflare's
	// public speed-test endpoints, which are built for exactly this. DownURL is
	// fetched with a bytes query parameter; UpURL receives a POST body.
	DownURL string `yaml:"down_url"`
	UpURL   string `yaml:"up_url"`
	// Bytes caps the total data moved per direction per run, so a fast link
	// can't run away with your data cap. The transfer also stops at Duration,
	// whichever comes first.
	Bytes int64 `yaml:"bytes"`
	// Pings is the number of RTT samples taken in each phase (idle and loaded).
	Pings int `yaml:"pings"`
	// Timeout is the per-ping reply timeout.
	Timeout time.Duration `yaml:"timeout"`
	// BadIncrease is the loaded-vs-idle median RTT increase at or above which a
	// run posts a Grafana annotation (0 disables annotations). 60ms ≈ a "C"
	// bufferbloat grade — enough to be felt on a stream.
	BadIncrease time.Duration `yaml:"bad_increase"`

	// Manual configures the on-demand "run a bufferbloat test on this IP" button
	// on the dashboard. It reuses the transfer settings above but takes its host
	// from the request, so it works even when the scheduled probe (Enabled) is
	// off. See ManualUnderload.
	Manual ManualUnderload `yaml:"manual"`
}

// ManualUnderload controls the on-demand latency-under-load test a user triggers
// from the dashboard (enter an IP, click "run"). Because each run saturates the
// link with real traffic and the dashboard may be shared, it is rate-limited and
// daily-capped.
type ManualUnderload struct {
	// Enabled turns the on-demand endpoint on. When false, the button's request
	// is rejected with 503.
	Enabled bool `yaml:"enabled"`
	// MinInterval is the minimum time between manual runs; a click within the
	// window is rejected with 429. 0 = unlimited.
	MinInterval time.Duration `yaml:"min_interval"`
	// DailyCap bounds manual runs per rolling 24h. 0 = unlimited.
	DailyCap int `yaml:"daily_cap"`
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
	// Manual configures the on-demand "take a look" investigation triggered from
	// the dashboard button. See ManualInvestigation.
	Manual ManualInvestigation `yaml:"manual"`
}

// ManualInvestigation controls the on-demand AI investigation a user triggers
// from the dashboard (the "take a look" button), as opposed to the automatic,
// anomaly-driven path. Because each run triggers an LLM call plus active probing
// and the dashboard may be shared (anonymous viewing), it is rate-limited and
// daily-capped. The limits are belt-and-suspenders when a local, zero-cost model
// is configured via ANTHROPIC_BASE_URL.
type ManualInvestigation struct {
	// Enabled turns the on-demand endpoint on. When false, the button's request
	// is rejected with 503. It still requires the AI feature to be enabled and an
	// API key (or local base URL) to be present.
	Enabled bool `yaml:"enabled"`
	// MinInterval is the minimum time between manual investigations across all
	// targets; a click within the window is rejected with 429. 0 = unlimited.
	MinInterval time.Duration `yaml:"min_interval"`
	// DailyCap bounds manual investigations per rolling 24h. 0 = unlimited.
	DailyCap int `yaml:"daily_cap"`
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
	// Context is optional free text appended to the agent's system prompt so it
	// knows your specific setup: ISP/plan line rates, gateway model, and what is
	// already configured or tried. Strongly recommended — it stops the agent
	// guessing (e.g. it can compute "90% of your 35 Mbps upload" and know your
	// Smart Queues cap is already correct).
	Context string `yaml:"context"`

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
	// SkipFar, when true (the default), skips the AI entirely for an isolated
	// far event — a single distant target spiking or going dark while every
	// other path is healthy. That's the far end's problem, not your network, so
	// it isn't worth an LLM call. Set false to investigate far problems anyway
	// (with the cheaper model and the longer FarRepeatTTL reuse window).
	SkipFar bool `yaml:"skip_far"`

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
		TraceProbes:   1,
		TraceASN:      true,
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
				SkipFar:         true,
				ModelEval:       true,
				ModelCheap:      "claude-sonnet-4-6",
				EvalSamples:     3,
			},
			Manual: ManualInvestigation{
				Enabled:     true,
				MinInterval: 60 * time.Second,
				DailyCap:    20,
			},
		},
		Underload: Underload{
			Enabled:     false, // opt-in: it moves real data
			Interval:    15 * time.Minute,
			Direction:   "down",
			Duration:    12 * time.Second,
			Streams:     4,
			DownURL:     "https://speed.cloudflare.com/__down",
			UpURL:       "https://speed.cloudflare.com/__up",
			Bytes:       250_000_000, // ~250 MB ceiling per direction per run
			Pings:       20,
			Timeout:     2 * time.Second,
			BadIncrease: 60 * time.Millisecond,
			Manual: ManualUnderload{
				Enabled:     true, // a click is explicit consent; the panel gates it
				MinInterval: 30 * time.Second,
				DailyCap:    20,
			},
		},
	}
}

// Load reads configuration from path, layers an optional gitignored overrides
// file (a sibling "overrides.yml" next to path) on top, applies defaults for any
// field still unset, and validates the result. The overrides file lets a user
// keep local customizations (their gateway IP, extra targets) that a deploy or
// git pull won't clobber, since the shipped targets.yml stays pristine.
func Load(path string) (Config, error) {
	return LoadWithOverrides(path, "")
}

// LoadWithOverrides is Load with an explicit overrides path. An empty
// overridesPath auto-discovers a sibling "overrides.yml" next to path and is a
// no-op when that file is absent; a non-empty overridesPath that cannot be read
// is an error (the user named a file they expect to exist).
func LoadWithOverrides(path, overridesPath string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	auto := overridesPath == ""
	if auto {
		overridesPath = filepath.Join(filepath.Dir(path), "overrides.yml")
	}
	odata, oerr := os.ReadFile(overridesPath)
	switch {
	case oerr == nil:
		if err := mergeOverrides(&cfg, odata); err != nil {
			return Config{}, fmt.Errorf("parse overrides %q: %w", overridesPath, err)
		}
	case auto && os.IsNotExist(oerr):
		// No overrides file — the common case; nothing to layer.
	default:
		return Config{}, fmt.Errorf("read overrides %q: %w", overridesPath, oerr)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// mergeOverrides layers an overrides document on top of an already-loaded
// Config. Scalar and nested fields are overwritten by whatever the overrides
// declare (YAML leaves unmentioned fields untouched). The two target lists —
// targets and discovery.candidates — are merged BY NAME instead of replaced, so
// an override entry with an existing name replaces just that target while new
// names are appended; this is what makes the overrides file additive.
func mergeOverrides(cfg *Config, data []byte) error {
	baseTargets := cfg.Targets
	baseCandidates := cfg.Discovery.Candidates

	// Layer scalars/nested fields. This also overwrites the two slice fields when
	// the overrides mention them, so we re-merge those by name below.
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return err
	}
	// Parse the overrides alone to recover exactly which targets/candidates they
	// declared (independent of what layering left on cfg).
	var ov Config
	if err := yaml.Unmarshal(data, &ov); err != nil {
		return err
	}
	cfg.Targets = mergeTargetsByName(baseTargets, ov.Targets)
	cfg.Discovery.Candidates = mergeTargetsByName(baseCandidates, ov.Discovery.Candidates)
	return nil
}

// mergeTargetsByName returns base with extra layered on top: an extra target
// whose name matches a base target replaces that entry in place; an extra target
// with a new name is appended. Order is stable (base order, then new names).
func mergeTargetsByName(base, extra []Target) []Target {
	if len(extra) == 0 {
		return base
	}
	out := make([]Target, len(base))
	copy(out, base)
	idx := make(map[string]int, len(out))
	for i, t := range out {
		idx[t.Name] = i
	}
	for _, t := range extra {
		if i, ok := idx[t.Name]; ok {
			out[i] = t
		} else {
			idx[t.Name] = len(out)
			out = append(out, t)
		}
	}
	return out
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
	// The transfer settings are shared by the scheduled probe and the on-demand
	// button, so validate them when either is active.
	if c.Underload.Enabled || c.Underload.Manual.Enabled {
		u := c.Underload
		switch u.Direction {
		case "down", "up", "both":
		default:
			return fmt.Errorf("underload.direction must be down|up|both, got %q", u.Direction)
		}
		if u.Streams < 1 {
			return fmt.Errorf("underload.streams must be >= 1, got %d", u.Streams)
		}
		if u.Pings < 1 {
			return fmt.Errorf("underload.pings must be >= 1, got %d", u.Pings)
		}
		// Each load phase needs room to ramp the link, then ping under load with
		// the per-reply timeout as headroom — so the window must comfortably
		// exceed the timeout.
		if u.Timeout <= 0 || u.Timeout*2 >= u.Duration {
			return fmt.Errorf("underload.timeout (%s) must be > 0 and well under underload.duration (%s)", u.Timeout, u.Duration)
		}
		if (u.Direction == "down" || u.Direction == "both") && u.DownURL == "" {
			return fmt.Errorf("underload.down_url is required for direction %q", u.Direction)
		}
		if (u.Direction == "up" || u.Direction == "both") && u.UpURL == "" {
			return fmt.Errorf("underload.up_url is required for direction %q", u.Direction)
		}
	}
	// The scheduled probe additionally needs a fixed host, metric label, and
	// interval (the on-demand button takes its host from the request).
	if c.Underload.Enabled {
		u := c.Underload
		if u.Host == "" {
			return fmt.Errorf("underload.host is required when underload is enabled")
		}
		if u.Target == "" {
			return fmt.Errorf("underload.target (metric label) is required when underload is enabled")
		}
		if u.Interval <= 0 {
			return fmt.Errorf("underload.interval must be > 0 when underload is enabled")
		}
	}
	return nil
}
