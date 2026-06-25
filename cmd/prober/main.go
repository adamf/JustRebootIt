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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	"github.com/adamf/justrebootit/internal/underload"
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
			Enabled:        cfg.Diagnostics.AI.Enabled,
			APIKey:         os.Getenv("ANTHROPIC_API_KEY"),
			BaseURL:        os.Getenv("ANTHROPIC_BASE_URL"),
			Model:          cfg.Diagnostics.AI.Model,
			ModelCheap:     cfg.Diagnostics.AI.ModelCheap,
			MaxIterations:  cfg.Diagnostics.AI.MaxIterations,
			PrometheusURL:  env("JRI_PROMETHEUS_URL", "http://prometheus:9090"),
			UDMExporterURL: env("UDM_EXPORTER_URL", "http://udm-exporter:9431"),
			Privileged:     cfg.Privileged,
			TraceMaxHops:   cfg.TraceMaxHops,
			TraceTimeout:   cfg.TraceTimeout,
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
			a.coalescer = aidiag.NewCoalescer(aidiag.CoalescerConfig{
				SharedWindow:    cfg.Diagnostics.AI.SharedWindow,
				SharedThreshold: cfg.Diagnostics.AI.SharedThreshold,
				RepeatTTL:       cfg.Diagnostics.AI.RepeatTTL,
				MinInterval:     cfg.Diagnostics.AI.MinInterval,
				DailyBudget:     cfg.Diagnostics.AI.DailyBudget,
				FarHops:         cfg.Diagnostics.AI.FarHops,
				FarTTL:          cfg.Diagnostics.AI.FarRepeatTTL,
				SkipFar:         cfg.Diagnostics.AI.SkipFar,
			})
			a.selector = aidiag.NewModelSelector(
				analyzer.CheapModel(), analyzer.ExpensiveModel(),
				cfg.Diagnostics.AI.ModelEval, cfg.Diagnostics.AI.EvalSamples)
			log.Printf("ai diagnostics enabled (model=%s, cheap=%s, eval=%t; repeat_ttl=%s far_ttl=%s far_hops=%d daily_budget=%d)",
				cfg.Diagnostics.AI.Model, analyzer.CheapModel(), cfg.Diagnostics.AI.ModelEval,
				cfg.Diagnostics.AI.RepeatTTL, cfg.Diagnostics.AI.FarRepeatTTL, cfg.Diagnostics.AI.FarHops, cfg.Diagnostics.AI.DailyBudget)
		}
	}

	// Always-on targets run for the whole lifetime.
	for _, t := range cfg.Targets {
		a.startTarget(t)
	}

	// Latency-under-load (bufferbloat) probe. Opt-in: it moves real data, so it
	// only runs when explicitly enabled in the config.
	if cfg.Underload.Enabled {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runUnderload(ctx)
		}()
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

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: a.httpHandler(reg)}
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
	// coalescer gates AI investigations so a recurring incident doesn't pay for
	// a fresh analysis every cycle. Nil when AI is disabled.
	coalescer *aidiag.Coalescer
	// selector picks the cheap vs expensive model per problem class. Nil when AI
	// is disabled.
	selector *aidiag.ModelSelector
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
	// reachHops is the last-known hop distance per target, fed by traceroutes
	// and discovery, so an event can be classified near vs far.
	reachHops map[string]int

	// Manual-investigation rate limiting (the dashboard "take a look" button).
	manualMu       sync.Mutex
	manualLast     time.Time
	manualDayStart time.Time
	manualDayCount int

	// On-demand underload rate limiting (the dashboard "run a bufferbloat test"
	// button).
	ulManualMu       sync.Mutex
	ulManualLast     time.Time
	ulManualDayStart time.Time
	ulManualDayCount int
}

// allowManual reports whether a manual investigation may run now, enforcing the
// configured global minimum interval and rolling daily cap (0 = unlimited). It
// records the run on success, so callers must only proceed when it returns true.
func (a *app) allowManual(now time.Time) bool {
	cfg := a.cfg.Diagnostics.Manual
	a.manualMu.Lock()
	defer a.manualMu.Unlock()

	if cfg.DailyCap > 0 {
		if a.manualDayStart.IsZero() || now.Sub(a.manualDayStart) >= 24*time.Hour {
			a.manualDayStart = now
			a.manualDayCount = 0
		}
		if a.manualDayCount >= cfg.DailyCap {
			return false
		}
	}
	if cfg.MinInterval > 0 && !a.manualLast.IsZero() && now.Sub(a.manualLast) < cfg.MinInterval {
		return false
	}
	a.manualLast = now
	a.manualDayCount++
	return true
}

func newApp(ctx context.Context, cfg config.Config, m *metrics.Metrics) *app {
	return &app{
		ctx:        ctx,
		cfg:        cfg,
		m:          m,
		diagJobs:   make(chan diag.Trigger, 64),
		registry:   make(map[string]config.Target),
		discovered: make(map[string]context.CancelFunc),
		reachHops:  make(map[string]int),
	}
}

// recordHops stores the measured hop distance to a target.
func (a *app) recordHops(target string, hops int) {
	if hops <= 0 {
		return
	}
	a.mu.Lock()
	a.reachHops[target] = hops
	a.mu.Unlock()
}

// hops returns the last-known hop distance to a target (0 = unknown).
func (a *app) hops(target string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reachHops[target]
}

// traceHops derives the destination's hop distance from a traceroute result.
func traceHops(r tracer.Result) int {
	if r.Reached && len(r.Hops) > 0 {
		return r.Hops[len(r.Hops)-1].TTL
	}
	return len(r.Hops)
}

func (a *app) httpHandler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// On-demand AI investigation, triggered by the dashboard "take a look"
	// button. Grafana proxies the request here server-side (via the Infinity
	// datasource), so this endpoint stays on the internal network and is never
	// exposed to the host.
	mux.HandleFunc("/api/investigate", a.handleInvestigate)
	// On-demand latency-under-load (bufferbloat) test against a user-supplied IP,
	// triggered by the dashboard button. Same server-side proxying as above.
	mux.HandleFunc("/api/underload", a.handleUnderload)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("justrebootit prober\nsee /metrics\n"))
	})
	return mux
}

// handleInvestigate kicks off an on-demand AI investigation of a target. It is
// the manual counterpart to the automatic anomaly path: it always investigates
// (no coalescing) but is rate-limited because each run makes an LLM call and
// runs active probes. The investigation is asynchronous; the writeup lands as a
// Grafana annotation, so this returns 202 as soon as the job is queued.
func (a *app) handleInvestigate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	if a.analyzer == nil || !a.cfg.Diagnostics.Manual.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "manual AI investigation is not enabled"})
		return
	}
	target := investigateTarget(r)
	if target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing target"})
		return
	}
	if _, ok := a.lookup(target); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown target: " + target})
		return
	}
	if !a.allowManual(time.Now()) {
		a.m.AISuppressed("manual-throttled")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limited; try again shortly"})
		return
	}

	id := a.events.Add(1)
	a.m.DiagTriggered(target, "manual")
	a.m.SetEventID(target, id)
	trig := diag.Trigger{EventID: id, Target: target, Reason: "manual"}
	select {
	case a.diagJobs <- trig:
		log.Printf("manual investigation #%d %s queued", id, target)
		writeJSON(w, http.StatusAccepted, map[string]any{"event_id": id, "target": target})
	default:
		// Undo the rate-limit charge would over-complicate; a full queue is rare
		// and the next click will succeed once a worker drains it.
		log.Printf("manual investigation #%d %s: queue full", id, target)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "diagnostic queue full, try again"})
	}
}

// investigateTarget extracts the target name from the request: a `target` query
// parameter, a JSON body {"target":"..."}, or a form field, in that order.
func investigateTarget(r *http.Request) string {
	if t := r.URL.Query().Get("target"); t != "" {
		return t
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Target string `json:"target"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			return ""
		}
		return strings.TrimSpace(body.Target)
	}
	_ = r.ParseForm()
	return strings.TrimSpace(r.FormValue("target"))
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
			a.recordHops(t.Name, traceHops(res))
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

	// Coalesce: a recurring/shared incident reuses a prior analysis instead of
	// launching another (paid) investigation. The trigger is still recorded
	// above, so the dashboard's per-trigger markers and counts stay honest.
	if a.coalescer != nil {
		hops := a.hops(t.Name)
		dec := a.coalescer.Decide(aidiag.Event{
			ID: trig.EventID, Target: t.Name, Group: t.Group, Reason: trig.Reason, Hops: hops,
		}, time.Now())
		if !dec.Investigate {
			a.m.AISuppressed(dec.Skip)
			if dec.Skip == "repeat" && dec.PriorEventID > 0 {
				log.Printf("diagnostic #%d %s: %s, grouped under #%d (%s, scope=%s)",
					trig.EventID, t.Name, dec.Skip, dec.PriorEventID, dec.Signature, dec.ScopeKind)
			} else {
				log.Printf("diagnostic #%d %s: skipping AI (%s, scope=%s)", trig.EventID, t.Name, dec.Skip, dec.ScopeKind)
			}
			return
		}
		trig.Signature = dec.Signature
		trig.ScopeKind = dec.ScopeKind
		trig.Hops = hops
	}

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
		ID:        trig.EventID,
		Target:    t.Name,
		Host:      t.Host,
		Group:     t.Group,
		Reason:    trig.Reason,
		Median:    trig.Median,
		Baseline:  trig.Baseline,
		Loss:      trig.Loss,
		When:      time.Now(),
		Signature: trig.Signature,
		Hops:      trig.Hops,
		ScopeKind: trig.ScopeKind,
	}

	func() {
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		defer cancel()

		// Fresh traceroute to capture the path while the problem is happening.
		tr := tracer.New(a.cfg.TraceMaxHops, a.cfg.TraceTimeout, a.cfg.Privileged)
		if res, err := tr.Trace(ctx, t.Host); err == nil {
			a.m.ObserveTrace(t.Name, t.Group, res)
			ev.Trace = res
			a.recordHops(t.Name, traceHops(res))
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
// writeup as a Grafana annotation. It picks the model via the selector (and may
// evaluate cheap vs expensive). It is a no-op when the analyzer is disabled.
func (a *app) analyze(ev aidiag.Event) {
	if a.analyzer == nil {
		return
	}

	var res aidiag.Analysis
	var err error
	switch {
	case ev.Reason == "manual":
		// User-triggered: always investigate with the configured model and skip
		// the cheap-vs-expensive evaluation (don't double-spend on a click).
		res, err = a.analyzer.Analyze(a.ctx, ev, a.analyzer.ExpensiveModel())
		if err == nil {
			a.m.AIModelUsed(res.Model)
		}
	default:
		class := ev.Reason + "|" + ev.ScopeKind
		plan := a.selector.Plan(class, ev.ScopeKind)
		if plan.Eval {
			res, err = a.evalModels(ev, class)
		} else {
			res, err = a.analyzer.Analyze(a.ctx, ev, plan.Model)
			if err == nil {
				a.m.AIModelUsed(res.Model)
			}
		}
	}
	if err != nil {
		a.m.AIFailed(ev.Target)
		// Drop the signature so a later event of this kind can retry rather than
		// reusing a non-existent analysis.
		if a.coalescer != nil && ev.Signature != "" {
			a.coalescer.Fail(ev.Signature)
		}
		log.Printf("ai diagnosis #%d %s: %v", ev.ID, ev.Target, err)
		return
	}
	a.m.AIAnalyzed(ev.Target)
	if a.coalescer != nil && ev.Signature != "" {
		a.coalescer.Record(ev.Signature, res)
	}
	log.Printf("ai diagnosis #%d %s [reason=%s, scope=%s, model=%s]: %s",
		ev.ID, ev.Target, ev.Reason, ev.ScopeKind, res.Model, res.Headline)

	annCtx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	tags := []string{
		"justrebootit", "ai-diagnosis",
		fmt.Sprintf("event:%d", ev.ID),
		"target:" + ev.Target,
		"reason:" + ev.Reason,
	}
	if ev.Reason == "manual" {
		tags = append(tags, "manual")
	}
	// res.Text already opens with the one-sentence root cause (res.Headline is a
	// copy of that first line), so don't prepend the headline again — that just
	// duplicated the whole sentence in the annotation.
	ann := grafana.Annotation{
		Time: ev.When,
		Tags: tags,
		Text: fmt.Sprintf("Event #%d (%s) — %s", ev.ID, ev.Target, res.Text),
	}
	if err := a.grafana.Post(annCtx, ann); err != nil {
		log.Printf("ai diagnosis #%d: posting annotation: %v", ev.ID, err)
	}
}

// evalModels investigates an event with BOTH the cheap and expensive models
// concurrently, asks the judge whether the cheap analysis was as good, and
// records that verdict so the class eventually settles on one model. It returns
// the expensive analysis (best quality) to surface, falling back to whichever
// model succeeded if one errored.
func (a *app) evalModels(ev aidiag.Event, class string) (aidiag.Analysis, error) {
	a.m.AIEvalRun()
	type result struct {
		an  aidiag.Analysis
		err error
	}
	expCh, cheapCh := make(chan result, 1), make(chan result, 1)
	go func() {
		an, err := a.analyzer.Analyze(a.ctx, ev, a.analyzer.ExpensiveModel())
		expCh <- result{an, err}
	}()
	go func() {
		an, err := a.analyzer.Analyze(a.ctx, ev, a.analyzer.CheapModel())
		cheapCh <- result{an, err}
	}()
	exp, cheap := <-expCh, <-cheapCh

	for _, r := range []result{exp, cheap} {
		if r.err == nil {
			a.m.AIModelUsed(r.an.Model)
		}
	}
	switch {
	case exp.err != nil && cheap.err != nil:
		return aidiag.Analysis{}, exp.err
	case exp.err != nil: // can't compare; just use cheap
		return cheap.an, nil
	case cheap.err != nil:
		return exp.an, nil
	}

	agree, jerr := a.analyzer.Judge(a.ctx, exp.an, cheap.an)
	if jerr != nil {
		log.Printf("model eval #%d: judge failed, treating as disagreement: %v", ev.ID, jerr)
		agree = false
	}
	if decided, chosen := a.selector.Record(class, agree); decided {
		log.Printf("model eval: class %q decided -> %s (cheap agreed=%t on final sample)", class, chosen, agree)
	}
	return exp.an, nil
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
		// Discovery already traced every candidate — feed those hop counts in so
		// events can be classified near vs far without an extra trace.
		for _, p := range paths {
			a.recordHops(p.Name, p.ReachHops)
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

// newUnderloadProber builds a Prober from the underload transfer settings,
// overriding the host (and, when non-empty, the direction). It backs both the
// scheduled loop and the on-demand button.
func (a *app) newUnderloadProber(host, direction string) *underload.Prober {
	uc := a.cfg.Underload
	if direction == "" {
		direction = uc.Direction
	}
	return underload.New(underload.Config{
		Host:       host,
		Direction:  direction,
		Duration:   uc.Duration,
		Streams:    uc.Streams,
		DownURL:    uc.DownURL,
		UpURL:      uc.UpURL,
		Bytes:      uc.Bytes,
		Pings:      uc.Pings,
		Timeout:    uc.Timeout,
		Privileged: a.cfg.Privileged,
	})
}

// underloadGrafana builds the annotation client. Annotations are best-effort and
// independent of the AI feature; the client is nil (a no-op) when Grafana
// credentials aren't set.
func (a *app) underloadGrafana() *grafana.Client {
	return grafana.New(
		env("GRAFANA_URL", "http://grafana:3000"),
		env("GRAFANA_USER", "admin"),
		os.Getenv("GRAFANA_ADMIN_PASSWORD"),
	)
}

// runUnderload periodically measures latency-under-load (bufferbloat): it
// saturates the link with a controlled transfer while pinging a stable host,
// and publishes the idle-vs-loaded RTT difference. The first run happens
// immediately so the dashboard has a reading at startup. A run with a bad
// bufferbloat grade posts a Grafana annotation so it shows up on the timeline.
func (a *app) runUnderload(ctx context.Context) {
	uc := a.cfg.Underload
	prober := a.newUnderloadProber(uc.Host, uc.Direction)
	gc := a.underloadGrafana()

	log.Printf("latency-under-load probe enabled (target=%s host=%s direction=%s every %s)",
		uc.Target, uc.Host, uc.Direction, uc.Interval)

	run := func() {
		a.reportUnderload(ctx, gc, uc.Target, prober.Run(ctx), false)
	}

	run()
	ticker := time.NewTicker(uc.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// reportUnderload publishes metrics and logs for each phase of an underload
// result and annotates it. A scheduled run annotates only a bad grade; a manual
// run annotates always (the user asked, so show them the answer).
func (a *app) reportUnderload(ctx context.Context, gc *grafana.Client, label string, res underload.Result, manual bool) {
	for _, ph := range res.Phases {
		a.m.ObserveUnderload(label, ph)
		inc := ph.Increase()
		log.Printf("underload %s/%s: idle=%s loaded=%s (+%s, %.1fx, grade %s) throughput=%.1f Mbps loss=%.0f%%",
			label, ph.Direction, ph.Idle.Median.Round(time.Millisecond),
			ph.Loaded.Median.Round(time.Millisecond), inc.Round(time.Millisecond),
			ph.Ratio(), underload.Grade(inc), ph.Bps/1e6, ph.Loaded.Loss*100)

		bad := a.cfg.Underload.BadIncrease > 0 && inc >= a.cfg.Underload.BadIncrease
		if (manual || bad) && ph.Idle.Recv > 0 && ph.Loaded.Recv > 0 {
			a.annotateUnderload(ctx, gc, label, ph, manual)
		}
	}
}

// annotateUnderload posts a Grafana annotation for a latency-under-load result,
// so the spike-under-load is visible on the timeline next to the latency graphs.
// The wording adapts to the bufferbloat grade.
func (a *app) annotateUnderload(ctx context.Context, gc *grafana.Client, label string, ph underload.Phase, manual bool) {
	inc := ph.Increase()
	grade := underload.Grade(inc)
	verdict := "The link buffers under load — enable SQM/Smart Queues on this end to keep the queue short."
	switch grade {
	case "A+", "A", "B":
		verdict = "Latency holds up under load here — this path is not your bufferbloat source."
	}
	tags := []string{"justrebootit", "underload", "bufferbloat", "target:" + label, "direction:" + ph.Direction}
	if manual {
		tags = append(tags, "manual")
	}
	annCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ann := grafana.Annotation{
		Time: time.Now(),
		Tags: tags,
		Text: fmt.Sprintf("Bufferbloat grade %s on %s (%s): latency %s idle → %s under load (+%s) while pushing %.0f Mbps. %s",
			grade, label, ph.Direction,
			ph.Idle.Median.Round(time.Millisecond), ph.Loaded.Median.Round(time.Millisecond),
			inc.Round(time.Millisecond), ph.Bps/1e6, verdict),
	}
	if err := gc.Post(annCtx, ann); err != nil {
		log.Printf("underload %s/%s: posting annotation: %v", label, ph.Direction, err)
	}
}

// handleUnderload kicks off an on-demand latency-under-load test against a
// user-supplied IP/host (the dashboard "run a bufferbloat test" button). Like
// the AI button it is rate-limited and asynchronous: the run takes ~10–25s and
// its result lands as a Grafana annotation and the underload_* metrics, so this
// returns 202 as soon as the job is accepted.
func (a *app) handleUnderload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	if !a.cfg.Underload.Manual.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "on-demand underload test is not enabled"})
		return
	}
	host, direction := underloadRequest(r)
	if host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing host (enter an IP or hostname)"})
		return
	}
	switch direction {
	case "", "down", "up", "both":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "direction must be down, up, or both"})
		return
	}
	if !a.allowUnderloadManual(time.Now()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limited; try again shortly"})
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		prober := a.newUnderloadProber(host, direction)
		log.Printf("manual underload %s (direction=%s) started", host, direction)
		a.reportUnderload(a.ctx, a.underloadGrafana(), host, prober.Run(a.ctx), true)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"host": host, "direction": direction})
}

// underloadRequest extracts host and direction from the request: query params, a
// JSON body {"host":...,"direction":...}, or form fields, in that order.
func underloadRequest(r *http.Request) (host, direction string) {
	host = strings.TrimSpace(r.URL.Query().Get("host"))
	direction = strings.TrimSpace(r.URL.Query().Get("direction"))
	if host != "" {
		return host, direction
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Host      string `json:"host"`
			Direction string `json:"direction"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err == nil {
			return strings.TrimSpace(body.Host), strings.TrimSpace(body.Direction)
		}
		return "", ""
	}
	_ = r.ParseForm()
	return strings.TrimSpace(r.FormValue("host")), strings.TrimSpace(r.FormValue("direction"))
}

// allowUnderloadManual enforces the configured minimum interval and rolling
// daily cap for on-demand underload runs (0 = unlimited), recording the run on
// success so callers must only proceed when it returns true.
func (a *app) allowUnderloadManual(now time.Time) bool {
	cfg := a.cfg.Underload.Manual
	a.ulManualMu.Lock()
	defer a.ulManualMu.Unlock()

	if cfg.DailyCap > 0 {
		if a.ulManualDayStart.IsZero() || now.Sub(a.ulManualDayStart) >= 24*time.Hour {
			a.ulManualDayStart = now
			a.ulManualDayCount = 0
		}
		if a.ulManualDayCount >= cfg.DailyCap {
			return false
		}
	}
	if cfg.MinInterval > 0 && !a.ulManualLast.IsZero() && now.Sub(a.ulManualLast) < cfg.MinInterval {
		return false
	}
	a.ulManualLast = now
	a.ulManualDayCount++
	return true
}

// env returns the value of an environment variable, or def when it is unset.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
