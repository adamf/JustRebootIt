package diag

import (
	"context"
	"net"
	"testing"
	"time"
)

func newTestDetector() *Detector {
	return NewDetector(DetectorConfig{
		Factor:        3.0,
		AbsMargin:     30 * time.Millisecond,
		LossThreshold: 0.1,
		Cooldown:      60 * time.Second,
		Alpha:         0.5,
	})
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestDetectorLatencySpike(t *testing.T) {
	d := newTestDetector()
	now := time.Now()

	// Establish a ~20ms baseline over a few healthy cycles.
	for i := 0; i < 5; i++ {
		if _, fired := d.Observe("x", ms(20), 0, true, now); fired {
			t.Fatalf("unexpected trigger while establishing baseline")
		}
		now = now.Add(time.Second)
	}

	// 100ms is 5x baseline and >30ms above it: must fire.
	trig, fired := d.Observe("x", ms(100), 0, true, now)
	if !fired {
		t.Fatalf("expected latency trigger at 100ms over ~20ms baseline")
	}
	if trig.Reason != "latency" {
		t.Errorf("reason = %q, want latency", trig.Reason)
	}
}

func TestDetectorAbsMarginGuardsLowLatency(t *testing.T) {
	d := newTestDetector()
	now := time.Now()

	// Baseline ~2ms. A jump to 8ms is 4x (factor hit) but only +6ms, below the
	// 30ms absolute margin — it should NOT fire.
	for i := 0; i < 5; i++ {
		d.Observe("y", ms(2), 0, true, now)
		now = now.Add(time.Second)
	}
	if _, fired := d.Observe("y", ms(8), 0, true, now); fired {
		t.Errorf("fired on a tiny absolute jump (8ms vs 2ms); abs margin should suppress it")
	}
}

func TestDetectorLoss(t *testing.T) {
	d := newTestDetector()
	now := time.Now()

	trig, fired := d.Observe("z", 0, 0.5, false, now)
	if !fired {
		t.Fatalf("expected loss trigger at 50%% loss")
	}
	if trig.Reason != "loss" {
		t.Errorf("reason = %q, want loss", trig.Reason)
	}
}

func TestDetectorCooldown(t *testing.T) {
	d := newTestDetector()
	now := time.Now()
	for i := 0; i < 5; i++ {
		d.Observe("c", ms(20), 0, true, now)
		now = now.Add(time.Second)
	}

	if _, fired := d.Observe("c", ms(200), 0, true, now); !fired {
		t.Fatalf("first spike should fire")
	}
	// Within the cooldown window: must not fire again.
	now = now.Add(10 * time.Second)
	if _, fired := d.Observe("c", ms(200), 0, true, now); fired {
		t.Errorf("fired again within cooldown")
	}
	// After the cooldown: fires again.
	now = now.Add(60 * time.Second)
	if _, fired := d.Observe("c", ms(200), 0, true, now); !fired {
		t.Errorf("should fire again after cooldown elapsed")
	}
}

func TestDetectorBaselineNotPollutedBySpike(t *testing.T) {
	d := newTestDetector()
	now := time.Now()
	for i := 0; i < 5; i++ {
		d.Observe("b", ms(20), 0, true, now)
		now = now.Add(time.Second)
	}
	// A spike fires but must not drag the baseline up...
	d.Observe("b", ms(300), 0, true, now)
	now = now.Add(90 * time.Second) // past cooldown
	// ...so a second, equally large spike still reads as anomalous.
	if _, fired := d.Observe("b", ms(300), 0, true, now); !fired {
		t.Errorf("baseline was polluted by the spike; second spike no longer detected")
	}
}

func TestTCPConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	d, err := TCPConnect(context.Background(), ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("TCPConnect: %v", err)
	}
	if d <= 0 {
		t.Errorf("connect duration = %v, want > 0", d)
	}
}

func TestTCPConnectRefused(t *testing.T) {
	// Reserve a port, then close it so the connection is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if _, err := TCPConnect(context.Background(), addr, 1*time.Second); err == nil {
		t.Errorf("expected error connecting to a closed port")
	}
}
