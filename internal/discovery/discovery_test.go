package discovery

import (
	"testing"

	"github.com/adamf/justrebootit/internal/tracer"
)

func TestPrefixKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"1.2.3.4", "1.2.3.0"},
		{"1.2.3.250", "1.2.3.0"},
		{"10.0.0.1", "10.0.0.0"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tc := range tests {
		if got := prefixKey(tc.in); got != tc.want {
			t.Errorf("prefixKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSelectPrefersDiverseEarlyHops(t *testing.T) {
	// a and b share the same hop2 /24 (10.0.0.x) — they are redundant. c and d
	// each introduce a distinct hop2. With max=2 we want two *distinct* paths,
	// not the two that share a hop.
	paths := []PathInfo{
		{Name: "a", Reached: true, ReachHops: 5, Hop2: "10.0.0.1", Hop3: "20.0.0.1"},
		{Name: "b", Reached: true, ReachHops: 5, Hop2: "10.0.0.2", Hop3: "20.0.0.2"},
		{Name: "c", Reached: true, ReachHops: 5, Hop2: "30.0.0.1", Hop3: "40.0.0.1"},
		{Name: "d", Reached: true, ReachHops: 5, Hop2: "50.0.0.1", Hop3: "60.0.0.1"},
	}
	got := selectedNames(Select(paths, 2, 0))

	// a is picked first (best-first tie broken by name). The second pick must
	// not be b (shares 10.0.0.0/24 and 20.0.0.0/24 with a); it should be c or d.
	if len(got) != 2 {
		t.Fatalf("got %d selected, want 2: %v", len(got), got)
	}
	if got[0] != "a" {
		t.Errorf("first pick = %q, want a", got[0])
	}
	if got[1] == "b" {
		t.Errorf("second pick = b, which shares a's path; want a diverse one (c or d)")
	}
}

func TestSelectPrefersFewerHops(t *testing.T) {
	// All distinct paths; with a single slot the shortest path wins.
	paths := []PathInfo{
		{Name: "far", Reached: true, ReachHops: 12, Hop2: "10.0.0.1"},
		{Name: "near", Reached: true, ReachHops: 3, Hop2: "20.0.0.1"},
		{Name: "mid", Reached: true, ReachHops: 7, Hop2: "30.0.0.1"},
	}
	got := selectedNames(Select(paths, 1, 0))
	if len(got) != 1 || got[0] != "near" {
		t.Fatalf("got %v, want [near] (fewest hops)", got)
	}
}

func TestSelectDropsTooFar(t *testing.T) {
	paths := []PathInfo{
		{Name: "near", Reached: true, ReachHops: 4, Hop2: "10.0.0.1"},
		{Name: "far", Reached: true, ReachHops: 20, Hop2: "20.0.0.1"},
	}
	got := selectedNames(Select(paths, 5, 12)) // maxReachHops=12 drops "far"
	if len(got) != 1 || got[0] != "near" {
		t.Fatalf("got %v, want [near] (far should be filtered by max_reach_hops)", got)
	}
}

func TestSelectFillsRemainingBudget(t *testing.T) {
	// Only one distinct path among reachable; pass 2 should still fill the
	// budget with the next-best leftover rather than returning a single pick.
	paths := []PathInfo{
		{Name: "a", Reached: true, ReachHops: 5, Hop2: "10.0.0.1"},
		{Name: "b", Reached: true, ReachHops: 6, Hop2: "10.0.0.2"}, // same /24 as a
	}
	got := selectedNames(Select(paths, 2, 0))
	if len(got) != 2 {
		t.Fatalf("got %v, want both targets to fill the budget", got)
	}
}

func TestFromTrace(t *testing.T) {
	r := tracer.Result{
		Reached: true,
		Hops: []tracer.Hop{
			{TTL: 1, Addr: "192.168.1.1"},
			{TTL: 2, Addr: "100.64.0.1"},
			{TTL: 3, Addr: "203.0.113.5"},
			{TTL: 4, Addr: "8.8.8.8"},
		},
	}
	p := FromTrace("g", "8.8.8.8", r)
	if p.Hop2 != "100.64.0.1" || p.Hop3 != "203.0.113.5" {
		t.Errorf("hop2/hop3 = %q/%q, want 100.64.0.1/203.0.113.5", p.Hop2, p.Hop3)
	}
	if p.ReachHops != 4 {
		t.Errorf("ReachHops = %d, want 4", p.ReachHops)
	}
	if !p.Reached {
		t.Error("Reached = false, want true")
	}
}

func selectedNames(paths []PathInfo) []string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = p.Name
	}
	return names
}
