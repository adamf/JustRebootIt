// Package metrics defines and publishes the Prometheus time series produced by
// the prober: per-target latency summaries and packet loss, plus per-hop
// traceroute latency. The metric and label names are kept stable because the
// Grafana dashboard and any shared historical data depend on them.
package metrics

import (
	"fmt"
	"strconv"
	"time"

	"github.com/adamf/justrebootit/internal/discovery"
	"github.com/adamf/justrebootit/internal/pinger"
	"github.com/adamf/justrebootit/internal/tracer"
	"github.com/adamf/justrebootit/internal/underload"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds every collector the prober updates. Construct it with New and
// register it against a prometheus.Registerer.
type Metrics struct {
	up        *prometheus.GaugeVec
	sent      *prometheus.CounterVec
	recv      *prometheus.CounterVec
	loss      *prometheus.GaugeVec
	rttBest   *prometheus.GaugeVec
	rttWorst  *prometheus.GaugeVec
	rttMedian *prometheus.GaugeVec
	rttMean   *prometheus.GaugeVec
	rttStdDev *prometheus.GaugeVec
	rttPct    *prometheus.GaugeVec

	hopRTT    *prometheus.GaugeVec
	hopInfo   *prometheus.GaugeVec
	hopLoss   *prometheus.GaugeVec
	asHandoff *prometheus.GaugeVec
	pathLen   *prometheus.GaugeVec
	reached   *prometheus.GaugeVec

	discSelected  *prometheus.GaugeVec
	discReachHops *prometheus.GaugeVec
	discReached   *prometheus.GaugeVec

	diagTriggered  *prometheus.CounterVec
	diagTCPConnect *prometheus.GaugeVec
	diagTCPUp      *prometheus.GaugeVec
	diagDNSLookup  *prometheus.GaugeVec
	diagDNSUp      *prometheus.GaugeVec
	diagEventID    *prometheus.GaugeVec
	aiAnalyzed     *prometheus.CounterVec
	aiFailed       *prometheus.CounterVec
	aiSuppressed   *prometheus.CounterVec
	aiModelUsed    *prometheus.CounterVec
	aiEvalRuns     prometheus.Counter
	aiTokens       *prometheus.CounterVec

	ulIdle       *prometheus.GaugeVec
	ulLoaded     *prometheus.GaugeVec
	ulIncrease   *prometheus.GaugeVec
	ulRatio      *prometheus.GaugeVec
	ulThroughput *prometheus.GaugeVec
	ulLoss       *prometheus.GaugeVec

	// On-demand-button status, so the dashboard can show what a manual run is
	// doing. ulManualRunning is 1 while a button-triggered test is in flight; the
	// ulManualLast* gauges hold the most recent button result (cleared each run
	// so only the latest tested host's series remain).
	ulManualRunning    prometheus.Gauge
	ulManualLastInc    *prometheus.GaugeVec
	ulManualLastIdle   *prometheus.GaugeVec
	ulManualLastLoaded *prometheus.GaugeVec
	ulManualLastBps    *prometheus.GaugeVec
	ulManualLastLoss   *prometheus.GaugeVec

	// buildInfo carries the running prober's git commit and build time in its
	// labels (value is always 1), so the dashboard can show exactly what code is
	// deployed and whether a redeploy actually landed.
	buildInfo *prometheus.GaugeVec
}

// New constructs the collectors and registers them with reg.
func New(reg prometheus.Registerer) *Metrics {
	probeLabels := []string{"target", "group"}
	m := &Metrics{
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_up",
			Help: "1 if the most recent probe cycle ran (received at least one reply), else 0.",
		}, probeLabels),
		sent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_packets_sent_total",
			Help: "Total ICMP echo requests sent.",
		}, probeLabels),
		recv: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_packets_received_total",
			Help: "Total ICMP echo replies received.",
		}, probeLabels),
		loss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_loss_ratio",
			Help: "Fraction of packets lost in the most recent cycle, in [0,1].",
		}, probeLabels),
		rttBest: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_rtt_best_seconds",
			Help: "Minimum round-trip time observed in the most recent cycle.",
		}, probeLabels),
		rttWorst: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_rtt_worst_seconds",
			Help: "Maximum round-trip time observed in the most recent cycle.",
		}, probeLabels),
		rttMedian: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_rtt_median_seconds",
			Help: "Median round-trip time in the most recent cycle.",
		}, probeLabels),
		rttMean: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_rtt_mean_seconds",
			Help: "Mean round-trip time in the most recent cycle.",
		}, probeLabels),
		rttStdDev: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_rtt_stddev_seconds",
			Help: "Standard deviation of round-trip time (jitter) in the most recent cycle.",
		}, probeLabels),
		rttPct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_rtt_percentile_seconds",
			Help: "Selected RTT percentiles used to draw the smokeping-style band.",
		}, []string{"target", "group", "percentile"}),

		hopRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_hop_rtt_seconds",
			Help: "Round-trip time to the router answering at the given TTL.",
		}, []string{"target", "group", "ttl"}),
		hopInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_hop_info",
			Help: "1 for the router address observed at the given TTL on the last trace.",
		}, []string{"target", "ttl", "addr"}),
		hopLoss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_hop_loss_ratio",
			Help: "Fraction of probes lost at the hop at the given TTL, over the last multi-pass trace (trace_probes > 1), in [0,1]. Labels carry the hop's address, origin AS, and approximate lat/lon so one query describes (and maps) the whole path. NOTE: loss at a single mid-path hop that does not persist to later hops is usually ICMP rate-limiting, not real loss — trust loss that persists across consecutive hops toward the destination. TTL is the hop number (1 = first hop / your gateway).",
		}, []string{"target", "group", "ttl", "addr", "asn", "as_name", "lat", "lon"}),
		asHandoff: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_as_handoff",
			Help: "1 at a TTL where the path crosses an AS boundary (this hop's ASN differs from the previous responding hop's) — a peering/transit handoff, where congestion and loss often live.",
		}, []string{"target", "ttl", "from_asn", "to_asn"}),
		pathLen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_path_length",
			Help: "Number of hops in the most recent traceroute.",
		}, probeLabels),
		reached: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_reached",
			Help: "1 if the most recent traceroute reached the destination, else 0.",
		}, probeLabels),

		discSelected: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "discovery_selected",
			Help: "1 if the candidate is currently promoted to active probing by path discovery, else 0.",
		}, []string{"target"}),
		discReachHops: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "discovery_reach_hops",
			Help: "Hop count to the candidate's destination measured during the last discovery pass.",
		}, []string{"target"}),
		discReached: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "discovery_reached",
			Help: "1 if the candidate was reachable during the last discovery pass, else 0.",
		}, []string{"target"}),

		diagTriggered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diagnostic_triggered_total",
			Help: "Count of latency/loss-triggered diagnostic runs, by target and reason.",
		}, []string{"target", "reason"}),
		diagTCPConnect: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "diagnostic_tcp_connect_seconds",
			Help: "TCP handshake time to the target measured during the last diagnostic run.",
		}, []string{"target"}),
		diagTCPUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "diagnostic_tcp_connect_up",
			Help: "1 if the last diagnostic TCP handshake to the target succeeded, else 0.",
		}, []string{"target"}),
		diagDNSLookup: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "diagnostic_dns_lookup_seconds",
			Help: "DNS resolution time for the configured probe name during the last diagnostic run.",
		}, []string{"target"}),
		diagDNSUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "diagnostic_dns_lookup_up",
			Help: "1 if the last diagnostic DNS lookup succeeded, else 0.",
		}, []string{"target"}),
		diagEventID: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "diagnostic_event_id",
			Help: "Monotonic id of the most recent diagnostic event for the target; maps a dashboard annotation to its diagnostic run.",
		}, []string{"target"}),
		aiAnalyzed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diagnostic_ai_analyzed_total",
			Help: "Count of events for which an AI root-cause analysis completed.",
		}, []string{"target"}),
		aiFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diagnostic_ai_failed_total",
			Help: "Count of events for which an AI root-cause analysis failed.",
		}, []string{"target"}),
		aiSuppressed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diagnostic_ai_suppressed_total",
			Help: "Count of events that reused a prior analysis, were throttled, or were classified as exogenous instead of triggering a new AI investigation, by reason (repeat|rate-limited|budget|exogenous).",
		}, []string{"reason"}),
		aiModelUsed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diagnostic_ai_model_used_total",
			Help: "Count of AI investigations by the Claude model that produced the analysis.",
		}, []string{"model"}),
		aiEvalRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diagnostic_ai_eval_runs_total",
			Help: "Count of dual-model evaluation runs (cheap vs expensive + judge).",
		}),
		aiTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diagnostic_ai_tokens_total",
			Help: "AI investigation tokens by model and kind (input|cache_read|cache_write|output). cache_read is billed ~0.1x, cache_write ~1.25-2x.",
		}, []string{"model", "kind"}),

		ulIdle: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_rtt_idle_seconds",
			Help: "Median RTT to the host with the link idle, from the last latency-under-load run.",
		}, []string{"target", "direction"}),
		ulLoaded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_rtt_loaded_seconds",
			Help: "Median RTT to the host while the link is saturated, from the last latency-under-load run.",
		}, []string{"target", "direction"}),
		ulIncrease: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_rtt_increase_seconds",
			Help: "Loaded minus idle median RTT — the bufferbloat — from the last latency-under-load run.",
		}, []string{"target", "direction"}),
		ulRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_bufferbloat_ratio",
			Help: "Loaded median RTT as a multiple of the idle median, from the last latency-under-load run.",
		}, []string{"target", "direction"}),
		ulThroughput: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_throughput_bits_per_second",
			Help: "Throughput achieved while saturating the link during the last latency-under-load run.",
		}, []string{"target", "direction"}),
		ulLoss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_loaded_loss_ratio",
			Help: "Packet loss to the host while the link was saturated, in [0,1], from the last latency-under-load run.",
		}, []string{"target", "direction"}),

		ulManualRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "underload_manual_running",
			Help: "1 while an on-demand (button-triggered) latency-under-load test is in progress, else 0.",
		}),
		ulManualLastInc: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_manual_last_increase_seconds",
			Help: "Loaded minus idle median RTT (the bufferbloat) from the most recent on-demand test, by host and direction.",
		}, []string{"host", "direction"}),
		ulManualLastIdle: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_manual_last_idle_seconds",
			Help: "Idle median RTT from the most recent on-demand test, by host and direction.",
		}, []string{"host", "direction"}),
		ulManualLastLoaded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_manual_last_loaded_seconds",
			Help: "Loaded median RTT from the most recent on-demand test, by host and direction.",
		}, []string{"host", "direction"}),
		ulManualLastBps: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_manual_last_throughput_bits_per_second",
			Help: "Throughput achieved during the most recent on-demand test, by host and direction.",
		}, []string{"host", "direction"}),
		ulManualLastLoss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "underload_manual_last_loaded_loss_ratio",
			Help: "Loaded packet loss from the most recent on-demand test, in [0,1], by host and direction.",
		}, []string{"host", "direction"}),

		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "justrebootit_build_info",
			Help: "Always 1; the running prober's git commit and build time are carried as labels so the dashboard can show what code is deployed.",
		}, []string{"commit", "built"}),
	}

	reg.MustRegister(
		m.up, m.sent, m.recv, m.loss,
		m.rttBest, m.rttWorst, m.rttMedian, m.rttMean, m.rttStdDev, m.rttPct,
		m.hopRTT, m.hopInfo, m.hopLoss, m.asHandoff, m.pathLen, m.reached,
		m.discSelected, m.discReachHops, m.discReached,
		m.diagTriggered, m.diagTCPConnect, m.diagTCPUp, m.diagDNSLookup, m.diagDNSUp,
		m.diagEventID, m.aiAnalyzed, m.aiFailed, m.aiSuppressed,
		m.aiModelUsed, m.aiEvalRuns, m.aiTokens,
		m.ulIdle, m.ulLoaded, m.ulIncrease, m.ulRatio, m.ulThroughput, m.ulLoss,
		m.ulManualRunning, m.ulManualLastInc, m.ulManualLastIdle,
		m.ulManualLastLoaded, m.ulManualLastBps, m.ulManualLastLoss,
		m.buildInfo,
	)
	return m
}

// SetBuildInfo records the running prober's git commit and build time as a
// constant 1-valued series, so the dashboard can display what's deployed.
func (m *Metrics) SetBuildInfo(commit, built string) {
	m.buildInfo.WithLabelValues(commit, built).Set(1)
}

// ObserveProbe publishes the statistics from one ping cycle.
func (m *Metrics) ObserveProbe(target, group string, r pinger.Result) {
	// Even on a fully-lost cycle we still record what we sent so loss is
	// computed correctly; counters only ever increase.
	m.sent.WithLabelValues(target, group).Add(float64(r.Sent))
	m.recv.WithLabelValues(target, group).Add(float64(r.Recv))
	m.loss.WithLabelValues(target, group).Set(r.Loss)

	if r.Recv == 0 {
		// No samples: mark down and leave the latency gauges at their previous
		// value rather than zeroing them (zero would read as "0ms", a lie).
		m.up.WithLabelValues(target, group).Set(0)
		return
	}
	m.up.WithLabelValues(target, group).Set(1)
	m.rttBest.WithLabelValues(target, group).Set(r.Best.Seconds())
	m.rttWorst.WithLabelValues(target, group).Set(r.Worst.Seconds())
	m.rttMedian.WithLabelValues(target, group).Set(r.Median.Seconds())
	m.rttMean.WithLabelValues(target, group).Set(r.Mean.Seconds())
	m.rttStdDev.WithLabelValues(target, group).Set(r.StdDev.Seconds())
	for q, v := range r.Percentiles {
		m.rttPct.WithLabelValues(target, group, strconv.Itoa(q)).Set(v.Seconds())
	}
}

// ObserveTrace publishes one traceroute. It first clears the previous path for
// this target so that hops which disappear (a shorter path, or a router that
// stopped replying) don't linger as stale series.
func (m *Metrics) ObserveTrace(target, group string, r tracer.Result) {
	m.hopRTT.DeletePartialMatch(prometheus.Labels{"target": target})
	m.hopInfo.DeletePartialMatch(prometheus.Labels{"target": target})

	for _, h := range r.Hops {
		ttl := ttlLabel(h.TTL)
		if h.Timeout || h.Addr == "" {
			continue
		}
		m.hopRTT.WithLabelValues(target, group, ttl).Set(h.RTT.Seconds())
		m.hopInfo.WithLabelValues(target, ttl, h.Addr).Set(1)
	}
	m.pathLen.WithLabelValues(target, group).Set(float64(len(r.Hops)))
	reached := 0.0
	if r.Reached {
		reached = 1
	}
	m.reached.WithLabelValues(target, group).Set(reached)
}

// ObserveHopLoss publishes the per-hop loss (and AS attribution / handoff
// markers) from a multi-pass trace. It first clears the target's prior hop
// series so hops that disappear (a shorter path, a router that stopped
// answering, an AS reassignment) don't linger as stale data.
func (m *Metrics) ObserveHopLoss(target, group string, hops []tracer.LossHop) {
	for _, v := range []*prometheus.GaugeVec{m.hopRTT, m.hopInfo, m.hopLoss, m.asHandoff} {
		v.DeletePartialMatch(prometheus.Labels{"target": target})
	}
	prevASN := ""
	for _, h := range hops {
		ttl := ttlLabel(h.TTL)
		// One enriched series per hop carries loss + address + AS + coordinates,
		// so a single query describes (and maps) the whole path, including private
		// gateway hops (which have an address but no public AS or geolocation).
		lat, lon := "", ""
		if h.GeoOK {
			lat = ftoa(h.Lat)
			lon = ftoa(h.Lon)
		}
		m.hopLoss.WithLabelValues(target, group, ttl, h.Addr, h.ASN, h.ASName, lat, lon).Set(h.Loss)
		if h.Addr == "" {
			continue // unresponsive hop: record only its loss
		}
		m.hopInfo.WithLabelValues(target, ttl, h.Addr).Set(1)
		if h.RTT > 0 {
			m.hopRTT.WithLabelValues(target, group, ttl).Set(h.RTT.Seconds())
		}
		if h.ASN != "" {
			if h.Handoff && prevASN != "" {
				m.asHandoff.WithLabelValues(target, ttl, prevASN, h.ASN).Set(1)
			}
			prevASN = h.ASN
		}
	}
	m.pathLen.WithLabelValues(target, group).Set(float64(len(hops)))
}

// ftoa renders a coordinate as a fixed-precision metric-label string.
func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

// ttlLabel renders a hop's TTL as a fixed-width, zero-padded label ("01".."30")
// so it sorts numerically as a string in Grafana legends and tables, where a
// plain "1","10","2" sorts wrong.
func ttlLabel(ttl int) string {
	return fmt.Sprintf("%02d", ttl)
}

// ClearTarget removes all probe, traceroute, and diagnostic series for a
// target. It is called when a discovery-promoted target is demoted, so its
// stale data does not linger on the dashboard. Discovery series are left alone:
// the candidate still exists and its selected=0 state is refreshed each pass.
func (m *Metrics) ClearTarget(target string) {
	l := prometheus.Labels{"target": target}
	for _, v := range []*prometheus.GaugeVec{
		m.up, m.loss, m.rttBest, m.rttWorst, m.rttMedian, m.rttMean, m.rttStdDev, m.rttPct,
		m.hopRTT, m.hopInfo, m.hopLoss, m.asHandoff, m.pathLen, m.reached,
		m.diagTCPConnect, m.diagTCPUp, m.diagDNSLookup, m.diagDNSUp, m.diagEventID,
	} {
		v.DeletePartialMatch(l)
	}
	m.sent.DeletePartialMatch(l)
	m.recv.DeletePartialMatch(l)
	m.diagTriggered.DeletePartialMatch(l)
	m.aiAnalyzed.DeletePartialMatch(l)
	m.aiFailed.DeletePartialMatch(l)
}

// SetEventID records the id of the most recent diagnostic event for a target,
// so a dashboard annotation tagged "event:N" can be tied back to its run.
func (m *Metrics) SetEventID(target string, id int64) {
	m.diagEventID.WithLabelValues(target).Set(float64(id))
}

// AIAnalyzed records that an AI analysis completed for a target's event.
func (m *Metrics) AIAnalyzed(target string) { m.aiAnalyzed.WithLabelValues(target).Inc() }

// AIFailed records that an AI analysis failed for a target's event.
func (m *Metrics) AIFailed(target string) { m.aiFailed.WithLabelValues(target).Inc() }

// AISuppressed records that an event reused a prior analysis or was throttled
// instead of triggering a new investigation.
func (m *Metrics) AISuppressed(reason string) { m.aiSuppressed.WithLabelValues(reason).Inc() }

// AIModelUsed records which model produced an analysis.
func (m *Metrics) AIModelUsed(model string) { m.aiModelUsed.WithLabelValues(model).Inc() }

// AIEvalRun records a dual-model evaluation run.
func (m *Metrics) AIEvalRun() { m.aiEvalRuns.Inc() }

// AITokens records the token usage of one investigation, by model and kind, so
// the dashboard can graph AI spend per model.
func (m *Metrics) AITokens(model string, input, cacheRead, cacheWrite, output int64) {
	m.aiTokens.WithLabelValues(model, "input").Add(float64(input))
	m.aiTokens.WithLabelValues(model, "cache_read").Add(float64(cacheRead))
	m.aiTokens.WithLabelValues(model, "cache_write").Add(float64(cacheWrite))
	m.aiTokens.WithLabelValues(model, "output").Add(float64(output))
}

// ObserveUnderload publishes one direction's latency-under-load result. The
// idle/loaded medians and throughput are only set when the loaded phase
// received replies; on a fully-lost loaded phase only the loss flag is updated,
// leaving the latency gauges at their prior value rather than reading 0ms.
func (m *Metrics) ObserveUnderload(target string, ph underload.Phase) {
	dir := ph.Direction
	m.ulThroughput.WithLabelValues(target, dir).Set(ph.Bps)
	if ph.Loaded.Recv > 0 {
		m.ulLoss.WithLabelValues(target, dir).Set(ph.Loaded.Loss)
	} else {
		m.ulLoss.WithLabelValues(target, dir).Set(1)
		return
	}
	if ph.Idle.Recv == 0 {
		return
	}
	m.ulIdle.WithLabelValues(target, dir).Set(ph.Idle.Median.Seconds())
	m.ulLoaded.WithLabelValues(target, dir).Set(ph.Loaded.Median.Seconds())
	m.ulIncrease.WithLabelValues(target, dir).Set(ph.Increase().Seconds())
	m.ulRatio.WithLabelValues(target, dir).Set(ph.Ratio())
}

// UnderloadManualRunning flags whether an on-demand (button) test is in flight,
// so the dashboard can show a live "Running…" status.
func (m *Metrics) UnderloadManualRunning(on bool) {
	if on {
		m.ulManualRunning.Set(1)
		return
	}
	m.ulManualRunning.Set(0)
}

// ClearUnderloadManualLast drops the previous on-demand result series so the
// "last test" panels only show the host just tested, not a stale one.
func (m *Metrics) ClearUnderloadManualLast() {
	for _, v := range []*prometheus.GaugeVec{
		m.ulManualLastInc, m.ulManualLastIdle, m.ulManualLastLoaded,
		m.ulManualLastBps, m.ulManualLastLoss,
	} {
		v.Reset()
	}
}

// ObserveUnderloadManualLast records one direction's on-demand result under
// host/direction labels for the "last test" status panels.
func (m *Metrics) ObserveUnderloadManualLast(host string, ph underload.Phase) {
	dir := ph.Direction
	m.ulManualLastBps.WithLabelValues(host, dir).Set(ph.Bps)
	if ph.Loaded.Recv > 0 {
		m.ulManualLastLoss.WithLabelValues(host, dir).Set(ph.Loaded.Loss)
	} else {
		m.ulManualLastLoss.WithLabelValues(host, dir).Set(1)
		return
	}
	if ph.Idle.Recv == 0 {
		return
	}
	m.ulManualLastIdle.WithLabelValues(host, dir).Set(ph.Idle.Median.Seconds())
	m.ulManualLastLoaded.WithLabelValues(host, dir).Set(ph.Loaded.Median.Seconds())
	m.ulManualLastInc.WithLabelValues(host, dir).Set(ph.Increase().Seconds())
}

// ObserveDiscovery publishes the result of one discovery pass: every candidate's
// reachability/hop count, and whether it was selected for active probing.
func (m *Metrics) ObserveDiscovery(paths []discovery.PathInfo, selected map[string]bool) {
	for _, p := range paths {
		sel := 0.0
		if selected[p.Name] {
			sel = 1
		}
		m.discSelected.WithLabelValues(p.Name).Set(sel)
		m.discReachHops.WithLabelValues(p.Name).Set(float64(p.ReachHops))
		reached := 0.0
		if p.Reached {
			reached = 1
		}
		m.discReached.WithLabelValues(p.Name).Set(reached)
	}
}

// DiagTriggered records that a diagnostic run started for target due to reason.
func (m *Metrics) DiagTriggered(target, reason string) {
	m.diagTriggered.WithLabelValues(target, reason).Inc()
}

// ObserveTCPConnect publishes the TCP-handshake result from a diagnostic run.
// On failure only the up=0 flag is set, leaving the latency gauge untouched.
func (m *Metrics) ObserveTCPConnect(target string, d time.Duration, ok bool) {
	if ok {
		m.diagTCPConnect.WithLabelValues(target).Set(d.Seconds())
		m.diagTCPUp.WithLabelValues(target).Set(1)
		return
	}
	m.diagTCPUp.WithLabelValues(target).Set(0)
}

// ObserveDNSLookup publishes the DNS-resolution result from a diagnostic run.
func (m *Metrics) ObserveDNSLookup(target string, d time.Duration, ok bool) {
	if ok {
		m.diagDNSLookup.WithLabelValues(target).Set(d.Seconds())
		m.diagDNSUp.WithLabelValues(target).Set(1)
		return
	}
	m.diagDNSUp.WithLabelValues(target).Set(0)
}
