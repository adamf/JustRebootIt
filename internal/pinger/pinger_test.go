package pinger

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }

	tests := []struct {
		name string
		in   []time.Duration
		q    int
		want time.Duration
	}{
		{"empty", nil, 50, 0},
		{"single", []time.Duration{ms(10)}, 50, ms(10)},
		{"single-p90", []time.Duration{ms(10)}, 90, ms(10)},
		// Sorted: 10,20,30,40,50. Nearest-rank p50 => ceil(0.5*5)=3 => 30ms.
		{"median-odd", []time.Duration{ms(50), ms(10), ms(30), ms(20), ms(40)}, 50, ms(30)},
		// p90 => ceil(0.9*5)=5 => 50ms (the max).
		{"p90", []time.Duration{ms(50), ms(10), ms(30), ms(20), ms(40)}, 90, ms(50)},
		// p10 => ceil(0.1*5)=1 => 10ms (the min).
		{"p10", []time.Duration{ms(50), ms(10), ms(30), ms(20), ms(40)}, 10, ms(10)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := percentile(tc.in, tc.q)
			if got != tc.want {
				t.Fatalf("percentile(%v, %d) = %v, want %v", tc.in, tc.q, got, tc.want)
			}
		})
	}
}

func TestPercentileDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{3, 1, 2}
	_ = percentile(in, 50)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Fatalf("percentile mutated its input: %v", in)
	}
}
