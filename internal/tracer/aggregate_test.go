package tracer

import (
	"testing"
	"time"
)

func TestAggregateLoss(t *testing.T) {
	// 4 passes. TTL1 answers every time (0 loss). TTL2 answers 2/4 (50% loss).
	// TTL3 is the destination, reached in 3 passes; the 4th pass stopped at TTL2.
	pass := func(h ...Hop) Result { return Result{Hops: h} }
	ok := func(ttl int, ms int) Hop {
		return Hop{TTL: ttl, Addr: "10.0.0." + itoa(ttl), RTT: time.Duration(ms) * time.Millisecond}
	}
	miss := func(ttl int) Hop { return Hop{TTL: ttl, Timeout: true} }

	results := []Result{
		pass(ok(1, 1), ok(2, 5), ok(3, 9)),
		pass(ok(1, 1), miss(2), ok(3, 9)),
		pass(ok(1, 1), ok(2, 5), ok(3, 9)),
		pass(ok(1, 1), miss(2)), // stopped at ttl2 (no ttl3 attempted)
	}
	hops := AggregateLoss(results)
	if len(hops) != 3 {
		t.Fatalf("got %d hops, want 3", len(hops))
	}
	if hops[0].Loss != 0 {
		t.Errorf("ttl1 loss = %v, want 0", hops[0].Loss)
	}
	if hops[1].Loss != 0.5 { // 2 of 4 passes lost ttl2
		t.Errorf("ttl2 loss = %v, want 0.5", hops[1].Loss)
	}
	// ttl3 was probed by 3 passes (the 4th stopped earlier) and answered all 3.
	if hops[2].Loss != 0 {
		t.Errorf("ttl3 loss = %v, want 0 (only counts passes that probed it)", hops[2].Loss)
	}
	if hops[2].Addr != "10.0.0.3" {
		t.Errorf("ttl3 addr = %q, want 10.0.0.3", hops[2].Addr)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
