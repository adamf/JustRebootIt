package main

import (
	"context"
	"testing"
	"time"

	"github.com/adamf/justrebootit/internal/config"
	"github.com/adamf/justrebootit/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// TestAllowManual checks the manual-investigation rate limiter: the global
// minimum interval blocks rapid repeats, the daily cap bounds total runs, and
// the daily window resets after 24h.
func TestAllowManual(t *testing.T) {
	cfg := config.Default()
	cfg.Diagnostics.Manual = config.ManualInvestigation{
		Enabled:     true,
		MinInterval: 100 * time.Millisecond,
		DailyCap:    2,
	}
	a := newApp(context.Background(), cfg, metrics.New(prometheus.NewRegistry()))

	base := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

	if !a.allowManual(base) {
		t.Fatal("first call should be allowed")
	}
	if a.allowManual(base.Add(50 * time.Millisecond)) {
		t.Error("call within min_interval should be rejected")
	}
	if !a.allowManual(base.Add(150 * time.Millisecond)) {
		t.Error("call after min_interval should be allowed (2nd of the day)")
	}
	if a.allowManual(base.Add(300 * time.Millisecond)) {
		t.Error("call exceeding daily_cap should be rejected")
	}
	// A full day later the daily window resets.
	if !a.allowManual(base.Add(24 * time.Hour)) {
		t.Error("call after the daily window resets should be allowed")
	}
}

// TestAllowManualUnlimited checks that zero values disable each limit.
func TestAllowManualUnlimited(t *testing.T) {
	cfg := config.Default()
	cfg.Diagnostics.Manual = config.ManualInvestigation{Enabled: true} // both limits 0
	a := newApp(context.Background(), cfg, metrics.New(prometheus.NewRegistry()))

	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		if !a.allowManual(now) {
			t.Fatalf("with no limits, call %d should be allowed", i)
		}
	}
}
