// Package metrics defines and publishes the Prometheus time series produced by
// the prober: per-target latency summaries and packet loss, plus per-hop
// traceroute latency. The metric and label names are kept stable because the
// Grafana dashboard and any shared historical data depend on them.
package metrics

import (
	"strconv"

	"github.com/adamf/justrebootit/internal/pinger"
	"github.com/adamf/justrebootit/internal/tracer"
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

	hopRTT  *prometheus.GaugeVec
	hopInfo *prometheus.GaugeVec
	pathLen *prometheus.GaugeVec
	reached *prometheus.GaugeVec
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
		pathLen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_path_length",
			Help: "Number of hops in the most recent traceroute.",
		}, probeLabels),
		reached: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "traceroute_reached",
			Help: "1 if the most recent traceroute reached the destination, else 0.",
		}, probeLabels),
	}

	reg.MustRegister(
		m.up, m.sent, m.recv, m.loss,
		m.rttBest, m.rttWorst, m.rttMedian, m.rttMean, m.rttStdDev, m.rttPct,
		m.hopRTT, m.hopInfo, m.pathLen, m.reached,
	)
	return m
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
		ttl := strconv.Itoa(h.TTL)
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
