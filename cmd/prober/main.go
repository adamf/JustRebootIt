// Command prober continuously measures latency and packet loss to a set of
// diverse targets and exposes the results as Prometheus metrics. Each target is
// probed concurrently; within a target, pings are smeared across the cycle the
// way smokeping does so brief spikes are not missed. Trace-enabled targets also
// get periodic ICMP traceroutes for per-hop attribution.
//
// On top of the always-on targets, two systems keep the data interesting:
//   - path discovery promotes a path-diverse subset of a candidate pool, so we
//     probe short, distinct routes rather than many redundant ones; and
//   - diagnostics fire deeper tests (fresh traceroute, TCP handshake, DNS
//     timing) the instant a target's latency or loss looks anomalous.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/adamf/justrebootit/internal/aidiag"
	"github.com/adamf/justrebootit/internal/config"
	"github.com/adamf/justrebootit/internal/diag"
	"github.com/adamf/justrebootit/internal/discovery"
	"github.com/adamf/justrebootit/internal/grafana"
	"github.com/adamf/justrebootit/internal/metrics"
	"github.com/adamf/justrebootit/internal/pinger"
	"github.com/adamf/justrebootit/internal/tracer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "/etc/justrebootit/targets.yml", "path to the prober config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("loaded %d always-on targets from %s (interval=%s, pings=%d; discovery=%t, diagnostics=%t)",
		len(cfg.Targets), *configPath, cfg.Interval, cfg.Pings, cfg.Discovery.Enabled, cfg.Diagnostics.Enabled)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a := newApp(ctx, cfg, m)

	// Diagnostics: detector + worker pool that runs deeper tests on triggers.
	if cfg.Diagnostics.Enabled {
		a.detector = diag.NewDetector(diag.DetectorConfig{
			Factor:        cfg.Diagnostics.LatencyFactor,
			AbsMargin:     cfg.Diagnostics.LatencyAbsMargin,
			LossThreshold: cfg.Diagnostics.LossThreshold,
			Cooldown:      cfg.Diagnostics.Cooldown,
			Alpha:         cfg.Diagnostics.BaselineAlpha,
		})
		a.startDiagWorkers()

		// Optional AI root-cause analysis. Stays disabled unless enabled in the
		// config AND an API key is present; secrets/URLs come from the
		// environment so they never land in the config file.
		analyzer, err := aidiag.New(aidiag.Config{
			Enabled:       cfg.Diagnostics.AI.Enabled,
			APIKey:        os.Getenv("ANTHROPIC_API_KEY"),
			Model:         cfg.Diagnostics.AI.Model,
			MaxIterations: cfg.Diagnostics.AI.MaxIterations,
			PrometheusURL: env("JRI_PROMETHEUS_URL", "http://prometheus:9090"),
			Privileged:    cfg.Privileged,
			TraceMaxHops:  cfg.TraceMaxHops,
			TraceTimeout:  cfg.TraceTimeout,
		})
		if err != nil {
			log.Fatalf("ai diagnostics: %v", err)
		}
		a.analyzer = analyzer
		if a.analyzer != nil {
			a.grafana = grafana.New(
				env("GRAFANA_URL", "http://grafana:3000"),
				env("GRAFANA_USER", "admin"),
				os.Getenv("GRAFANA_ADMIN_PASSWORD"),
			)
			log.Printf("ai diagnostics enabled (model=%s)", cfg.Diagnostics.AI.Model)
		}
	}

	// Always-on targets run for the whole lifetime.
	for _, t := range cfg.Targets {
		a.startTarget(t)
	}

	// Path discovery promotes a diverse subset of the candidate pool. With no
	// candidates it has nothing to do, so it stays dormant.
	if cfg.Discovery.Enabled && len(cfg.Discovery.Candidates) > 0 {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runDiscovery(ctx)
		}()
	}

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: metricsHandler(reg)}
	go func() {
		log.Printf("serving metrics on %s/metrics", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("metrics server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	a.wg.Wait()
}

// app holds the prober's shared state and coordinates the target loops,
// discovery, and diagnostics.
type app struct {
	ctx context.Context
	cfg config.Config
	m   *metrics.Metrics

	detector *diag.Detector
	diagJobs chan diag.Trigger

	// analyzer and grafana are optional: both stay nil unless the AI
	// diagnostics are enabled and credentials are present.
	analyzer *aidiag.Analyzer
	grafana  *grafana.Client
	// events assigns each detected anomaly a monotonic id.
	events atomic.Int64

	wg sync.WaitGroup

	mu sync.Mutex
	// registry maps a probed target's name to its full definition, so a
	// diagnostic worker can look up the host/group for a triggered target.
	registry map[string]config.Target
	// discovered tracks the cancel func for each discovery-promoted target so
	// they can be stopped when no longer selected.
	discovered map[string]context.CancelFunc
}

func newApp(ctx context.Context, cfg config.Config, m *metrics.Metrics) *app {
	return &app{
		ctx:        ctx,
		cfg:        cfg,
		m:          m,
		diagJobs:   make(chan diag.Trigger, 64),
		registry:   make(map[string]config.Target),
		discovered: make(map[string]context.CancelFunc),
	}
}

func metricsHandler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("justrebootit prober\nsee /metrics\n"))
	})
	return mux
}

// startTarget launches the ping loop (and, if the target is trace-enabled, the
// trace loop) for a target that lives for the whole process lifetime.
func (a *app) startTarget(t config.Target) {
	a.register(t)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.pingLoop(a.ctx, t)
	}()
	if t.Trace {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.traceLoop(a.ctx, t)
		}()
	}
}

func (a *app) register(t config.Target) {
	a.mu.Lock()
	a.registry[t.Name] = t
	a.mu.Unlock()
}

func (a *app) lookup(name string) (config.Target, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.registry[name]
	return t, ok
}

// pingLoop probes one target once per cycle until ctx is cancelled, publishing
// the per-cycle statistics and feeding the anomaly detector.
func (a *app) pingLoop(ctx context.Context, t config.Target) {
	p := pinger.New(t.Host, a.cfg.Pings, a.cfg.Timeout, a.cfg.Privileged)

	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()
	for {
		res := p.Run(ctx, a.cfg.Interval)
		if res.Err != nil {
			// A hard error (e.g. unresolvable host) — record down with full
			// loss and keep trying; transient DNS or routing failures often
			// recover. Reporting loss=1 (rather than 0) keeps the loss panel
			// honest while probe_up=0 flags the underlying failure.
			log.Printf("ping %s (%s): %v", t.Name, t.Host, res.Err)
			res = pinger.Result{Sent: a.cfg.Pings, Loss: 1}
		}
		a.m.ObserveProbe(t.Name, t.Group, res)
		a.maybeDiagnose(t, res)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// traceLoop traces one target on the trace interval until cancelled.
func (a *app) traceLoop(ctx context.Context, t config.Target) {
	tr := tracer.New(a.cfg.TraceMaxHops, a.cfg.TraceTimeout, a.cfg.Privileged)

	ticker := time.NewTicker(a.cfg.TraceInterval)
	defer ticker.Stop()
	for {
		res, err := tr.Trace(ctx, t.Host)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("trace %s (%s): %v", t.Name, t.Host, err)
		} else {
			a.m.ObserveTrace(t.Name, t.Group, res)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// maybeDiagnose feeds a cycle's result to the detector and, on an anomaly,
// enqueues a diagnostic run (dropping it if the queue is full rather than
// blocking the probe loop).
func (a *app) maybeDiagnose(t config.Target, res pinger.Result) {
	if a.detector == nil {
		return
	}
	trig, fired := a.detector.Observe(t.Name, res.Median, res.Loss, res.Recv > 0, time.Now())
	if !fired {
		return
	}
	trig.EventID = a.events.Add(1)
	a.m.DiagTriggered(t.Name, trig.Reason)
	a.m.SetEventID(t.Name, trig.EventID)
	log.Printf("diagnostic trigger #%d %s: reason=%s median=%s baseline=%s loss=%.0f%%",
		trig.EventID, t.Name, trig.Reason, trig.Median, trig.Baseline, trig.Loss*100)
	select {
	case a.diagJobs <- trig:
	default:
		log.Printf("diagnostic queue full, dropping run for %s", t.Name)
	}
}

// startDiagWorkers launches the diagnostic worker pool.
func (a *app) startDiagWorkers() {
	for i := 0; i < a.cfg.Diagnostics.Workers; i++ {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			for {
				select {
				case <-a.ctx.Done():
					return
				case trig := <-a.diagJobs:
					a.runDiagnostics(trig)
				}
			}
		}()
	}
}

// runDiagnostics performs the deeper tests for a triggered target: a fresh
// traceroute (path snapshot during the event), a TCP handshake (real-traffic
// latency independent of ICMP treatment), and a DNS resolution timing.
func (a *app) runDiagnostics(trig diag.Trigger) {
	t, ok := a.lookup(trig.Target)
	if !ok {
		return
	}

	ev := aidiag.Event{
		ID:       trig.EventID,
		Target:   t.Name,
		Host:     t.Host,
		Group:    t.Group,
		Reason:   trig.Reason,
		Median:   trig.Median,
		Baseline: trig.Baseline,
		Loss:     trig.Loss,
		When:     time.Now(),
	}

	func() {
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		defer cancel()

		// Fresh traceroute to capture the path while the problem is happening.
		tr := tracer.New(a.cfg.TraceMaxHops, a.cfg.TraceTimeout, a.cfg.Privileged)
		if res, err := tr.Trace(ctx, t.Host); err == nil {
			a.m.ObserveTrace(t.Name, t.Group, res)
			ev.Trace = res
		}

		// TCP handshake latency to the same host.
		if a.cfg.Diagnostics.TCPPort > 0 {
			addr := net.JoinHostPort(t.Host, strconv.Itoa(a.cfg.Diagnostics.TCPPort))
			d, err := diag.TCPConnect(ctx, addr, 5*time.Second)
			a.m.ObserveTCPConnect(t.Name, d, err == nil)
			ev.TCPConnect, ev.TCPOK = d, err == nil
			if err != nil {
				log.Printf("diagnostic tcp %s (%s): %v", t.Name, addr, err)
			}
		}

		// DNS resolution timing.
		if a.cfg.Diagnostics.DNSProbe != "" {
			d, err := diag.DNSLookup(ctx, a.cfg.Diagnostics.DNSProbe, 5*time.Second)
			a.m.ObserveDNSLookup(t.Name, d, err == nil)
			ev.DNSLookup, ev.DNSOK = d, err == nil
			if err != nil {
				log.Printf("diagnostic dns %s (%s): %v", t.Name, a.cfg.Diagnostics.DNSProbe, err)
			}
		}
	}()

	// Optional LLM root-cause analysis, using the mechanical results above as
	// its starting context. Runs on its own (longer) timeout inside Analyze.
	a.analyze(ev)
}

// analyze runs the optional AI investigation for an event and surfaces its
// writeup as a Grafana annotation. It is a no-op when the analyzer is disabled.
func (a *app) analyze(ev aidiag.Event) {
	if a.analyzer == nil {
		return
	}
	res, err := a.analyzer.Analyze(a.ctx, ev)
	if err != nil {
		a.m.AIFailed(ev.Target)
		log.Printf("ai diagnosis #%d %s: %v", ev.ID, ev.Target, err)
		return
	}
	a.m.AIAnalyzed(ev.Target)
	log.Printf("ai diagnosis #%d %s: %s", ev.ID, ev.Target, res.Headline)

	annCtx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	ann := grafana.Annotation{
		Time: ev.When,
		Tags: []string{
			"justrebootit", "ai-diagnosis",
			fmt.Sprintf("event:%d", ev.ID),
			"target:" + ev.Target,
			"reason:" + ev.Reason,
		},
		Text: fmt.Sprintf("**Event #%d — %s** (target %s)\n\n%s",
			ev.ID, res.Headline, ev.Target, res.Text),
	}
	if err := a.grafana.Post(annCtx, ann); err != nil {
		log.Printf("ai diagnosis #%d: posting annotation: %v", ev.ID, err)
	}
}

// runDiscovery periodically traces the candidate pool, selects a path-diverse
// subset, and reconciles the set of discovery-promoted probe targets. The first
// pass runs immediately so discovered targets come online at startup.
func (a *app) runDiscovery(ctx context.Context) {
	d := discovery.NewDiscoverer(a.cfg.Discovery.MaxHops, a.cfg.TraceTimeout, a.cfg.Privileged)

	pass := func() {
		paths := d.Probe(ctx, a.cfg.Discovery.Candidates)
		selected := discovery.Select(paths, a.cfg.Discovery.MaxTargets, a.cfg.Discovery.MaxReachHops)

		selectedSet := make(map[string]bool, len(selected))
		for _, p := range selected {
			selectedSet[p.Name] = true
		}
		a.m.ObserveDiscovery(paths, selectedSet)
		a.reconcileDiscovered(selected)

		names := make([]string, 0, len(selected))
		for _, p := range selected {
			names = append(names, p.Name)
		}
		log.Printf("discovery: traced %d candidates, active set: %v", len(paths), names)
	}

	pass()
	ticker := time.NewTicker(a.cfg.Discovery.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass()
		}
	}
}

// reconcileDiscovered starts probing newly selected candidates and stops the
// ones no longer selected, leaving always-on targets untouched.
func (a *app) reconcileDiscovered(selected []discovery.PathInfo) {
	want := make(map[string]config.Target, len(selected))
	candByName := make(map[string]config.Target, len(a.cfg.Discovery.Candidates))
	for _, c := range a.cfg.Discovery.Candidates {
		candByName[c.Name] = c
	}
	for _, p := range selected {
		c := candByName[p.Name]
		c.Group = "discovered"
		c.Trace = true // the whole point of a discovered path is to trace it
		want[p.Name] = c
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Stop targets that are no longer selected.
	for name, cancel := range a.discovered {
		if _, keep := want[name]; !keep {
			cancel()
			delete(a.discovered, name)
			delete(a.registry, name)
			a.m.ClearTarget(name)
		}
	}
	// Start newly selected targets.
	for name, t := range want {
		if _, running := a.discovered[name]; running {
			continue
		}
		tctx, cancel := context.WithCancel(a.ctx)
		a.discovered[name] = cancel
		a.registry[name] = t

		a.wg.Add(1)
		go func(t config.Target) {
			defer a.wg.Done()
			a.pingLoop(tctx, t)
		}(t)
		a.wg.Add(1)
		go func(t config.Target) {
			defer a.wg.Done()
			a.traceLoop(tctx, t)
		}(t)
	}
}

// env returns the value of an environment variable, or def when it is unset.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
