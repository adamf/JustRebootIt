// Package discovery finds a path-diverse subset of candidate targets to probe.
//
// Probing many destinations that share the same first few hops wastes effort:
// when the shared segment hiccups, every target spikes at once and nothing is
// localized. Discovery traces the candidate pool, then keeps the destinations
// whose 2nd/3rd hops differ from one another and that are reachable in the
// fewest hops. The result is a set of short, distinct paths, so a future spike
// pins to a specific segment instead of smearing across redundant routes.
package discovery

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/adamf/justrebootit/internal/config"
	"github.com/adamf/justrebootit/internal/tracer"
)

// PathInfo summarizes one candidate's measured path: how far away it is and the
// early hops used to judge path diversity.
type PathInfo struct {
	Name string
	Host string
	// Reached is true if the traceroute reached the destination.
	Reached bool
	// ReachHops is the hop count to the destination when Reached, otherwise the
	// number of responding hops observed (used only as a tie-break).
	ReachHops int
	// Hop2 and Hop3 are the router addresses at TTL 2 and 3 ("" if that hop did
	// not answer). These are the segments most useful for distinguishing one
	// ISP-side path from another.
	Hop2 string
	Hop3 string
}

// hop2Key and hop3Key return the /24 (or /64 for IPv6) prefix of the early
// hops. Grouping by prefix rather than exact address treats routers in the same
// block as "the same path", which is what we want for diversity.
func (p PathInfo) hop2Key() string { return prefixKey(p.Hop2) }
func (p PathInfo) hop3Key() string { return prefixKey(p.Hop3) }

// FromTrace distills a traceroute into a PathInfo.
func FromTrace(name, host string, r tracer.Result) PathInfo {
	p := PathInfo{Name: name, Host: host, Reached: r.Reached}
	for _, h := range r.Hops {
		switch h.TTL {
		case 2:
			p.Hop2 = h.Addr
		case 3:
			p.Hop3 = h.Addr
		}
	}
	if r.Reached && len(r.Hops) > 0 {
		p.ReachHops = r.Hops[len(r.Hops)-1].TTL
	} else {
		p.ReachHops = len(r.Hops)
	}
	return p
}

// Select chooses up to max path-diverse candidates. Candidates that take more
// than maxReachHops to reach are dropped (maxReachHops <= 0 disables that
// filter). Selection is deterministic given the same input.
//
// The algorithm is a two-pass greedy:
//  1. Walk candidates best-first (reachable, then fewest hops, then name) and
//     take any whose 2nd- or 3rd-hop prefix has not been seen yet — i.e. each
//     pick adds genuinely new path diversity.
//  2. If budget remains, fill it with the best leftover candidates so the
//     probe budget isn't wasted.
func Select(paths []PathInfo, max, maxReachHops int) []PathInfo {
	if max <= 0 {
		return nil
	}

	eligible := make([]PathInfo, 0, len(paths))
	for _, p := range paths {
		if maxReachHops > 0 && p.Reached && p.ReachHops > maxReachHops {
			continue
		}
		eligible = append(eligible, p)
	}
	sort.SliceStable(eligible, func(i, j int) bool { return less(eligible[i], eligible[j]) })

	selected := make([]PathInfo, 0, max)
	picked := make(map[string]bool, len(eligible))
	seen2 := make(map[string]bool)
	seen3 := make(map[string]bool)

	// Pass 1: maximize diversity of the early hops.
	for _, p := range eligible {
		if len(selected) >= max {
			break
		}
		k2, k3 := p.hop2Key(), p.hop3Key()
		newPath := (k2 != "" && !seen2[k2]) || (k3 != "" && !seen3[k3])
		// A candidate whose early hops never answered carries no diversity
		// signal; defer it to pass 2 rather than spending a diverse slot on it.
		if k2 == "" && k3 == "" {
			continue
		}
		if !newPath {
			continue
		}
		selected = append(selected, p)
		picked[p.Name] = true
		if k2 != "" {
			seen2[k2] = true
		}
		if k3 != "" {
			seen3[k3] = true
		}
	}

	// Pass 2: spend any remaining budget on the best leftovers.
	for _, p := range eligible {
		if len(selected) >= max {
			break
		}
		if picked[p.Name] {
			continue
		}
		selected = append(selected, p)
		picked[p.Name] = true
	}
	return selected
}

// less orders candidates best-first: reachable before unreachable, then fewer
// hops, then name for stability.
func less(a, b PathInfo) bool {
	if a.Reached != b.Reached {
		return a.Reached // reachable first
	}
	if a.ReachHops != b.ReachHops {
		return a.ReachHops < b.ReachHops
	}
	return a.Name < b.Name
}

// prefixKey returns a coarse network key for an address: the /24 for IPv4 and
// the /64 for IPv6. Empty input yields an empty key.
func prefixKey(addr string) string {
	if addr == "" {
		return ""
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return ip.Mask(net.CIDRMask(24, 32)).String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// Discoverer traces candidate hosts to build PathInfo for them.
type Discoverer struct {
	maxHops    int
	timeout    time.Duration
	privileged bool
	// concurrency bounds how many candidates are traced at once.
	concurrency int
}

// NewDiscoverer constructs a Discoverer using short traceroutes (maxHops).
func NewDiscoverer(maxHops int, timeout time.Duration, privileged bool) *Discoverer {
	return &Discoverer{maxHops: maxHops, timeout: timeout, privileged: privileged, concurrency: 8}
}

// Probe traces every candidate concurrently and returns their PathInfo. Traces
// that error out are reported as unreachable rather than dropped, so a failing
// candidate is simply ranked last.
func (d *Discoverer) Probe(ctx context.Context, candidates []config.Target) []PathInfo {
	out := make([]PathInfo, len(candidates))
	sem := make(chan struct{}, d.concurrency)
	var wg sync.WaitGroup

	for i, c := range candidates {
		wg.Add(1)
		go func(i int, c config.Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			tr := tracer.New(d.maxHops, d.timeout, d.privileged)
			res, err := tr.Trace(ctx, c.Host)
			if err != nil {
				out[i] = PathInfo{Name: c.Name, Host: c.Host}
				return
			}
			out[i] = FromTrace(c.Name, c.Host, res)
		}(i, c)
	}
	wg.Wait()
	return out
}
