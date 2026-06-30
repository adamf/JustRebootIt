package metrics

import (
	"testing"
	"time"

	"github.com/adamf/justrebootit/internal/tracer"
	"github.com/prometheus/client_golang/prometheus"
)

func TestTTLLabel(t *testing.T) {
	cases := map[int]string{1: "01", 9: "09", 12: "12", 30: "30"}
	for in, want := range cases {
		if got := ttlLabel(in); got != want {
			t.Errorf("ttlLabel(%d) = %q, want %q", in, got, want)
		}
	}
	// Zero-padding must make string order match numeric order.
	if !(ttlLabel(2) < ttlLabel(10)) {
		t.Error("ttlLabel must sort 2 before 10 as strings")
	}
}

// TestObserveHopLoss guards the label cardinality of the hop metrics — a
// WithLabelValues arg-count mismatch would panic here, in CI, not in production.
func TestObserveHopLoss(t *testing.T) {
	m := New(prometheus.NewRegistry())
	hops := []tracer.LossHop{
		{TTL: 1, Addr: "192.168.1.1"}, // private gateway, no ASN
		{TTL: 2, Addr: "96.120.0.1", Loss: 0, RTT: 8 * time.Millisecond, ASN: "7922", ASName: "COMCAST-7922"},
		{TTL: 3, Loss: 1}, // timed-out hop: loss only
		{TTL: 4, Addr: "4.2.2.1", Loss: 0.6, ASN: "3356", ASName: "LEVEL3", Handoff: true}, // AS boundary
	}
	// Should not panic.
	m.ObserveHopLoss("t", "anchor", hops)
	m.ClearTarget("t")
}
