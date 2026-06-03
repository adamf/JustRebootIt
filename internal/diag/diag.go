// Package diag detects latency/loss anomalies and runs deeper, on-demand tests
// when one fires. Intermittent home-internet problems are usually gone before a
// human can react, so the moment a probe cycle looks unhealthy we capture extra
// signal automatically: a fresh traceroute (driven by the caller), a TCP
// handshake time (ICMP is often deprioritized, so this corroborates real-app
// impact), and a DNS resolution time (a common hidden culprit).
package diag

import (
	"context"
	"net"
	"sync"
	"time"
)

// Trigger describes a detected anomaly worth investigating.
type Trigger struct {
	Target   string
	Reason   string // "latency" or "loss"
	Median   time.Duration
	Baseline time.Duration
	Loss     float64
}

// DetectorConfig tunes the anomaly detector.
type DetectorConfig struct {
	// Factor: trigger when median > Factor * baseline.
	Factor float64
	// AbsMargin: and median must also exceed baseline by at least this much.
	AbsMargin time.Duration
	// LossThreshold: trigger when loss >= this (in [0,1]).
	LossThreshold float64
	// Cooldown: minimum time between triggers for the same target.
	Cooldown time.Duration
	// Alpha: EWMA smoothing for the rolling baseline, in (0,1].
	Alpha float64
}

// Detector tracks a per-target rolling latency baseline and decides when a
// cycle is anomalous. It is safe for concurrent use.
type Detector struct {
	cfg DetectorConfig

	mu    sync.Mutex
	state map[string]*targetState
}

type targetState struct {
	baseline  time.Duration
	haveBase  bool
	lastFired time.Time
}

// NewDetector builds a Detector from cfg.
func NewDetector(cfg DetectorConfig) *Detector {
	return &Detector{cfg: cfg, state: make(map[string]*targetState)}
}

// Observe feeds one probe cycle's median RTT and loss to the detector and
// reports whether it constitutes an anomaly. The baseline is updated from
// healthy cycles only, so a sustained spike doesn't quietly become the new
// "normal". now is passed in for deterministic testing.
//
// recv is whether the cycle received any replies; a fully-lost cycle has no
// meaningful median, so latency is judged on loss alone.
func (d *Detector) Observe(target string, median time.Duration, loss float64, recv bool, now time.Time) (Trigger, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	st := d.state[target]
	if st == nil {
		st = &targetState{}
		d.state[target] = st
	}

	reason := ""
	switch {
	case loss >= d.cfg.LossThreshold && d.cfg.LossThreshold > 0:
		reason = "loss"
	case recv && st.haveBase && d.isLatencySpike(median, st.baseline):
		reason = "latency"
	}

	anomalous := reason != ""

	// Update the baseline from healthy cycles only (and only when we have a
	// real sample), so spikes never pollute it.
	if recv && !anomalous {
		if !st.haveBase {
			st.baseline = median
			st.haveBase = true
		} else {
			st.baseline = ewma(st.baseline, median, d.cfg.Alpha)
		}
	}

	if !anomalous {
		return Trigger{}, false
	}
	// Debounce: respect the cooldown so a sustained event fires once, not every
	// cycle.
	if !st.lastFired.IsZero() && now.Sub(st.lastFired) < d.cfg.Cooldown {
		return Trigger{}, false
	}
	st.lastFired = now

	return Trigger{
		Target:   target,
		Reason:   reason,
		Median:   median,
		Baseline: st.baseline,
		Loss:     loss,
	}, true
}

// isLatencySpike reports whether median is high enough above baseline to count,
// requiring both the multiplicative factor and the absolute margin so neither a
// tiny jitter on a low-latency target nor a small relative bump on a high one
// trips it alone.
func (d *Detector) isLatencySpike(median, baseline time.Duration) bool {
	factorHit := float64(median) > d.cfg.Factor*float64(baseline)
	marginHit := median-baseline >= d.cfg.AbsMargin
	return factorHit && marginHit
}

// ewma returns the exponentially weighted moving average update.
func ewma(prev, sample time.Duration, alpha float64) time.Duration {
	return time.Duration(alpha*float64(sample) + (1-alpha)*float64(prev))
}

// TCPConnect measures how long a TCP handshake to addr ("host:port") takes.
// Because it exercises the same path as real traffic but does not depend on
// ICMP, it distinguishes genuine latency from a router merely deprioritizing
// pings.
func TCPConnect(ctx context.Context, addr string, timeout time.Duration) (time.Duration, error) {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dialer net.Dialer
	start := time.Now()
	conn, err := dialer.DialContext(dctx, "tcp", addr)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return elapsed, nil
}

// DNSLookup measures how long it takes to resolve name with the system
// resolver. Slow or failing resolution often masquerades as "the internet is
// slow", so timing it during an incident is valuable.
func DNSLookup(ctx context.Context, name string, timeout time.Duration) (time.Duration, error) {
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var resolver net.Resolver
	start := time.Now()
	_, err := resolver.LookupHost(lctx, name)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	return elapsed, nil
}
