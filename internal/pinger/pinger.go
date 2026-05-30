// Package pinger runs a cycle of ICMP echo probes against a single target and
// reduces the round-trip times to the summary statistics a smokeping-style
// dashboard needs: best/worst/median latency, the spread between percentiles
// (the "smoke"), jitter, and packet loss.
package pinger

import (
	"context"
	"sort"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

// Result is the outcome of one probe cycle against one target.
type Result struct {
	// Sent and Recv count the echo requests issued and replies received.
	Sent int
	Recv int
	// Loss is the fraction of packets lost in [0,1].
	Loss float64

	// Best/Worst/Median/Mean/StdDev summarize the received RTTs. They are only
	// meaningful when Recv > 0; callers should gate on that.
	Best   time.Duration
	Worst  time.Duration
	Median time.Duration
	Mean   time.Duration
	StdDev time.Duration

	// Percentiles holds selected RTT percentiles used to draw the smoke band.
	// Keys are the percentile (e.g. 10, 25, 75, 90); values are the RTT.
	Percentiles map[int]time.Duration

	// Err is set when the probe could not run at all (e.g. DNS failure). A
	// cycle where packets were simply lost is not an error — that shows up as
	// Loss == 1 with Err == nil.
	Err error
}

// SmokePercentiles are the percentiles published for the smoke band. They are
// symmetric around the median so the band reads naturally above and below it.
var SmokePercentiles = []int{10, 25, 75, 90}

// Pinger probes one target. It is safe to reuse across cycles; each Run builds
// a fresh underlying ICMP session.
type Pinger struct {
	host       string
	count      int
	timeout    time.Duration
	privileged bool
}

// New constructs a Pinger. count echo requests are sent per cycle; each reply
// must arrive within timeout. privileged selects raw vs. datagram ICMP.
func New(host string, count int, timeout time.Duration, privileged bool) *Pinger {
	return &Pinger{host: host, count: count, timeout: timeout, privileged: privileged}
}

// Run executes one probe cycle, spreading the echo requests across window so
// the samples are smeared over the interval rather than sent in a burst (this
// matches smokeping and catches intermittent spikes better). It returns once
// all replies are in, all are lost, or ctx is cancelled.
func (p *Pinger) Run(ctx context.Context, window time.Duration) Result {
	pb, err := probing.NewPinger(p.host)
	if err != nil {
		return Result{Err: err}
	}
	pb.SetPrivileged(p.privileged)
	pb.Count = p.count
	pb.Timeout = window
	// Spread the requests evenly across the window, leaving the final timeout
	// as headroom for the last reply to come back.
	usable := window - p.timeout
	if usable <= 0 {
		usable = window / 2
	}
	if p.count > 1 {
		pb.Interval = usable / time.Duration(p.count-1)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		err = pb.RunWithContext(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		pb.Stop()
		<-done
	}
	if err != nil && ctx.Err() == nil {
		return Result{Err: err}
	}

	st := pb.Statistics()
	res := Result{
		Sent:        st.PacketsSent,
		Recv:        st.PacketsRecv,
		Loss:        st.PacketLoss / 100.0, // pro-bing reports a percentage
		Best:        st.MinRtt,
		Worst:       st.MaxRtt,
		Mean:        st.AvgRtt,
		StdDev:      st.StdDevRtt,
		Percentiles: map[int]time.Duration{},
	}
	res.Median = percentile(st.Rtts, 50)
	for _, q := range SmokePercentiles {
		res.Percentiles[q] = percentile(st.Rtts, q)
	}
	return res
}

// percentile returns the q-th percentile (0..100) of the round-trip times
// using nearest-rank on a sorted copy. It returns 0 when there are no samples.
func percentile(rtts []time.Duration, q int) time.Duration {
	if len(rtts) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(rtts))
	copy(sorted, rtts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Nearest-rank: rank = ceil(q/100 * N), clamped to [1, N].
	rank := (q*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
