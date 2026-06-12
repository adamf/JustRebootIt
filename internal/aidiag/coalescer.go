package aidiag

import (
	"fmt"
	"sync"
	"time"
)

// CoalescerConfig tunes how aggressively repeated/similar events are collapsed
// so they don't each trigger a (paid) AI investigation.
type CoalescerConfig struct {
	// SharedWindow is the window used to decide whether many targets spiked
	// together — i.e. one shared upstream incident rather than per-path faults.
	SharedWindow time.Duration
	// SharedThreshold is how many distinct targets must trip within SharedWindow
	// for an event to be treated as a single "shared" incident.
	SharedThreshold int
	// RepeatTTL is how long a signature's analysis is reused. A repeat of the
	// same signature within this window skips the AI entirely.
	RepeatTTL time.Duration
	// MinInterval is the global minimum time between full investigations, to
	// stop a burst of distinct signatures from each calling the AI at once.
	MinInterval time.Duration
	// DailyBudget caps full investigations per rolling 24h (0 = unlimited).
	DailyBudget int
}

// Decision is the outcome of evaluating an event for investigation.
type Decision struct {
	// Investigate is true when a full AI investigation should run.
	Investigate bool
	// Signature is the event's computed signature.
	Signature string
	// Skip explains why no investigation runs ("repeat", "rate-limited",
	// "budget"); empty when Investigate is true.
	Skip string
	// PriorEventID / PriorHeadline reference the incident this event was folded
	// into, when Skip == "repeat".
	PriorEventID  int64
	PriorHeadline string
}

type sigEntry struct {
	firstEventID int64
	headline     string
	lastSeen     time.Time
	count        int
}

type recentTrig struct {
	target string
	when   time.Time
}

// Coalescer decides which events warrant a fresh AI investigation and which can
// reuse a recent one. It is safe for concurrent use.
type Coalescer struct {
	cfg CoalescerConfig

	mu                sync.Mutex
	sigs              map[string]*sigEntry
	recent            []recentTrig
	lastInvestigation time.Time
	dayStart          time.Time
	dayCount          int
}

// NewCoalescer builds a Coalescer.
func NewCoalescer(cfg CoalescerConfig) *Coalescer {
	return &Coalescer{cfg: cfg, sigs: make(map[string]*sigEntry)}
}

// Decide evaluates an event and returns whether to investigate it. now is
// injected for deterministic testing.
func (c *Coalescer) Decide(ev Event, now time.Time) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Maintain a window of recent triggers to detect a shared (cross-target)
	// incident, and count distinct targets within it.
	c.recent = append(c.recent, recentTrig{ev.Target, now})
	cutoff := now.Add(-c.cfg.SharedWindow)
	kept := c.recent[:0]
	distinct := make(map[string]struct{})
	for _, r := range c.recent {
		if r.when.After(cutoff) {
			kept = append(kept, r)
			distinct[r.target] = struct{}{}
		}
	}
	c.recent = kept

	sig := c.signature(ev, len(distinct))

	// A repeat of a recently-analyzed signature reuses that analysis — no AI.
	if e, ok := c.sigs[sig]; ok && now.Sub(e.lastSeen) < c.cfg.RepeatTTL {
		e.lastSeen = now
		e.count++
		return Decision{
			Signature:     sig,
			Skip:          "repeat",
			PriorEventID:  e.firstEventID,
			PriorHeadline: e.headline,
		}
	}

	// Global throttle: don't start a new investigation too soon after the last.
	if !c.lastInvestigation.IsZero() && now.Sub(c.lastInvestigation) < c.cfg.MinInterval {
		return Decision{Signature: sig, Skip: "rate-limited"}
	}

	// Daily budget ceiling.
	if c.cfg.DailyBudget > 0 {
		if c.dayStart.IsZero() || now.Sub(c.dayStart) >= 24*time.Hour {
			c.dayStart = now
			c.dayCount = 0
		}
		if c.dayCount >= c.cfg.DailyBudget {
			return Decision{Signature: sig, Skip: "budget"}
		}
	}

	// Approve. Mark optimistically (under the lock) so concurrent triggers are
	// throttled; Record fills the headline on success, Fail reverts on failure.
	c.lastInvestigation = now
	c.dayCount++
	c.sigs[sig] = &sigEntry{firstEventID: ev.ID, lastSeen: now, count: 1}
	return Decision{Investigate: true, Signature: sig}
}

// signature is a coarse, cheap fingerprint. Shared incidents collapse to one
// signature regardless of which target tripped; isolated faults stay per-path.
func (c *Coalescer) signature(ev Event, distinctTargets int) string {
	if distinctTargets >= c.cfg.SharedThreshold {
		return ev.Reason + "|shared"
	}
	return fmt.Sprintf("%s|isolated|%s|%s", ev.Reason, ev.Group, ev.Target)
}

// Record stores the analysis headline for a signature so later repeats can
// reference it.
func (c *Coalescer) Record(sig string, a Analysis) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.sigs[sig]; ok {
		e.headline = a.Headline
	}
}

// Fail drops a signature whose investigation failed so a later event can retry
// it (rather than reusing a non-existent analysis).
func (c *Coalescer) Fail(sig string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sigs, sig)
}
