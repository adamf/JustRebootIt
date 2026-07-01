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

func TestBoundaries(t *testing.T) {
	// Path: home(no ASN) -> A(7922) -> A(7922) -> B(6939) -> C(15169). Two
	// handoffs: 7922->6939 (near = the second 7922 hop) and 6939->15169.
	hops := []LossHop{
		{TTL: 0, Addr: "203.0.113.7"},                                         // home anchor, no ASN
		{TTL: 2, Addr: "96.110.1.1", ASN: "7922", ASName: "Comcast"},          // first AS hop, no handoff
		{TTL: 3, Addr: "96.110.2.2", ASN: "7922", ASName: "Comcast"},          // near side of first boundary
		{TTL: 4, Addr: "72.14.1.1", ASN: "6939", ASName: "HE", Handoff: true}, // far side of first boundary
		{TTL: 5, Addr: "8.8.1.1", ASN: "15169", ASName: "Google", Handoff: true},
	}
	got := Boundaries(hops)
	if len(got) != 2 {
		t.Fatalf("want 2 boundaries, got %d: %+v", len(got), got)
	}
	if got[0].FromASN != "7922" || got[0].ToASN != "6939" || got[0].NearAddr != "96.110.2.2" || got[0].FarAddr != "72.14.1.1" {
		t.Errorf("boundary 0 wrong: %+v", got[0])
	}
	if got[1].FromASN != "6939" || got[1].ToASN != "15169" || got[1].NearAddr != "72.14.1.1" || got[1].FarAddr != "8.8.1.1" {
		t.Errorf("boundary 1 wrong: %+v", got[1])
	}

	// No handoffs -> no boundaries.
	if b := Boundaries([]LossHop{{TTL: 1, Addr: "96.110.1.1", ASN: "7922"}}); len(b) != 0 {
		t.Errorf("want no boundaries, got %+v", b)
	}
}
