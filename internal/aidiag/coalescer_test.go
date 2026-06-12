package aidiag

import (
	"testing"
	"time"
)

func testCoalescer() *Coalescer {
	return NewCoalescer(CoalescerConfig{
		SharedWindow:    2 * time.Minute,
		SharedThreshold: 3,
		RepeatTTL:       time.Hour,
		MinInterval:     3 * time.Minute,
		DailyBudget:     50,
	})
}

func ev(id int64, target, group, reason string) Event {
	return Event{ID: id, Target: target, Group: group, Reason: reason}
}

func TestCoalescerRepeatIsReused(t *testing.T) {
	c := testCoalescer()
	now := time.Now()

	d1 := c.Decide(ev(1, "cloudflare-dns", "anchor", "latency"), now)
	if !d1.Investigate {
		t.Fatalf("first event of a signature should investigate")
	}
	c.Record(d1.Signature, Analysis{Headline: "bufferbloat"})

	// Same target/signature 5 min later (past MinInterval, within RepeatTTL):
	// reused, not investigated.
	d2 := c.Decide(ev(2, "cloudflare-dns", "anchor", "latency"), now.Add(5*time.Minute))
	if d2.Investigate {
		t.Fatalf("repeat within RepeatTTL should be reused, not investigated")
	}
	if d2.Skip != "repeat" || d2.PriorEventID != 1 || d2.PriorHeadline != "bufferbloat" {
		t.Errorf("repeat decision = %+v, want skip=repeat prior=1 headline=bufferbloat", d2)
	}
}

func TestCoalescerSharedIncidentCollapses(t *testing.T) {
	// MinInterval=0 here to isolate the shared-signature reuse path from the
	// global throttle (which would otherwise rate-limit the tight burst).
	c := NewCoalescer(CoalescerConfig{
		SharedWindow: 2 * time.Minute, SharedThreshold: 3,
		RepeatTTL: time.Hour, MinInterval: 0, DailyBudget: 50,
	})
	now := time.Now()

	// Three different targets spike together → the 3rd is classified "shared",
	// collapsing the incident regardless of which target tripped.
	c.Decide(ev(1, "a", "anchor", "latency"), now)
	c.Decide(ev(2, "b", "anchor", "latency"), now.Add(time.Second))
	d3 := c.Decide(ev(3, "c", "isp", "latency"), now.Add(2*time.Second))
	if d3.Signature != "latency|shared" || !d3.Investigate {
		t.Fatalf("3rd simultaneous target should be a shared investigation, got %+v", d3)
	}
	c.Record(d3.Signature, Analysis{Headline: "shared upstream congestion"})

	// A 4th distinct target moments later is the same shared incident → reused.
	d4 := c.Decide(ev(4, "d", "content", "latency"), now.Add(3*time.Second))
	if d4.Investigate || d4.Skip != "repeat" || d4.PriorEventID != 3 {
		t.Errorf("4th shared event should be reused under #3, got %+v", d4)
	}
}

func TestCoalescerMinIntervalThrottles(t *testing.T) {
	c := testCoalescer()
	now := time.Now()

	// First isolated signature investigates.
	if d := c.Decide(ev(1, "a", "anchor", "loss"), now); !d.Investigate {
		t.Fatal("first should investigate")
	}
	// A different isolated signature 1 min later (< MinInterval 3m) is throttled.
	d := c.Decide(ev(2, "b", "isp", "loss"), now.Add(time.Minute))
	if d.Investigate || d.Skip != "rate-limited" {
		t.Errorf("second distinct signature within MinInterval should be rate-limited, got %+v", d)
	}
	// After MinInterval, a new distinct signature investigates again.
	if d := c.Decide(ev(3, "c", "content", "loss"), now.Add(4*time.Minute)); !d.Investigate {
		t.Errorf("after MinInterval a new signature should investigate, got %+v", d)
	}
}

func TestCoalescerDailyBudget(t *testing.T) {
	c := NewCoalescer(CoalescerConfig{
		SharedThreshold: 99, // never shared, so each target is its own signature
		RepeatTTL:       time.Hour,
		MinInterval:     0,
		DailyBudget:     2,
	})
	base := time.Now()
	approved := 0
	for i := 0; i < 5; i++ {
		// Distinct target each time, spaced out so none are "repeat".
		d := c.Decide(ev(int64(i), string(rune('a'+i)), "anchor", "latency"), base.Add(time.Duration(i)*time.Minute))
		if d.Investigate {
			approved++
		} else if d.Skip != "budget" && d.Skip != "" {
			// only "budget" is expected once exhausted
		}
	}
	if approved != 2 {
		t.Errorf("daily budget=2 should approve exactly 2 investigations, got %d", approved)
	}
}

func TestCoalescerFailAllowsRetry(t *testing.T) {
	c := testCoalescer()
	now := time.Now()

	d1 := c.Decide(ev(1, "a", "anchor", "latency"), now)
	if !d1.Investigate {
		t.Fatal("first should investigate")
	}
	c.Fail(d1.Signature) // investigation failed → signature dropped

	// Same signature after MinInterval should investigate again (not reuse).
	d2 := c.Decide(ev(2, "a", "anchor", "latency"), now.Add(4*time.Minute))
	if !d2.Investigate {
		t.Errorf("after Fail, a later event of the same signature should retry, got %+v", d2)
	}
}
