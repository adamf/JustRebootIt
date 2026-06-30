// Package geo resolves an IP address to an approximate latitude/longitude (and
// city) using the free, keyless ip-api.com service, with caching. It is used to
// plot traceroute paths on a map. IP geolocation — especially of backbone
// routers — is approximate; this is for a "where on the map is the loss" view,
// not precision.
package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// negTTL is how long a failed lookup is remembered before we retry it. Kept
	// short (vs the multi-hour success TTL) so a transient ip-api.com error — a
	// timeout, or a 429 from its free 45-req/min rate limit — doesn't blank a
	// hop's location for the whole success TTL. Without this, one rate-limited
	// burst at startup poisons the cache for a day and the path map goes empty.
	negTTL = 2 * time.Minute
	// minInterval paces requests to stay under ip-api.com's free-tier limit
	// (45/min). At ~1.4s apart we do ~42/min, so a cold-start burst across many
	// hops fills in over a couple of minutes instead of tripping 429s.
	minInterval = 1400 * time.Millisecond
)

// Loc is an approximate location for an IP.
type Loc struct {
	Lat  float64
	Lon  float64
	City string
	IP   string // the resolved address; set by Self (which discovers our public IP)
	OK   bool   // false for private/bogon addresses or a failed lookup
}

type entry struct {
	loc  Loc
	when time.Time
	// resolved is true when ip-api.com gave a definitive answer (success or a
	// clean "no geolocation here"); false when the lookup failed transiently
	// (network error, rate limit) and should be retried after negTTL.
	resolved bool
}

// Resolver looks up and caches IP-to-location mappings. Safe for concurrent use.
type Resolver struct {
	base string
	http *http.Client
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]entry

	// gate paces outbound requests (see minInterval) and rate-limits the failure
	// log so a sustained outage doesn't spam.
	gate    sync.Mutex
	nextAt  time.Time
	lastLog time.Time
}

// New builds a Resolver. ttl is the cache lifetime (0 = 24h default). The
// ip-api.com base URL can be overridden for testing.
func New(ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Resolver{
		base:  "http://ip-api.com/json/",
		http:  &http.Client{Timeout: 5 * time.Second},
		ttl:   ttl,
		cache: make(map[string]entry),
	}
}

// Lookup returns the approximate location of ip. It returns an unset Loc (OK
// false) for private/bogon/non-IPv4 addresses and on any lookup error.
func (r *Resolver) Lookup(ctx context.Context, ip string) Loc {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil || isPrivate(parsed) {
		return Loc{}
	}

	r.mu.Lock()
	if e, ok := r.cache[ip]; ok {
		ttl := r.ttl
		if !e.resolved {
			ttl = negTTL // retry a failed lookup soon, not in 24h
		}
		if time.Since(e.when) < ttl {
			r.mu.Unlock()
			return e.loc
		}
	}
	r.mu.Unlock()

	loc, resolved := r.fetch(ctx, ip)

	r.mu.Lock()
	r.cache[ip] = entry{loc: loc, when: time.Now(), resolved: resolved}
	r.mu.Unlock()
	return loc
}

// Self geolocates the prober's own public IP. ip-api.com geolocates the caller
// when queried with no address, so this discovers both our external IP and where
// it is — used to anchor a mapped path's start at "home" even though the first
// hops are private. Cached like any other lookup, under a fixed key.
func (r *Resolver) Self(ctx context.Context) Loc {
	const key = "@self"
	r.mu.Lock()
	if e, ok := r.cache[key]; ok {
		ttl := r.ttl
		if !e.resolved {
			ttl = negTTL
		}
		if time.Since(e.when) < ttl {
			r.mu.Unlock()
			return e.loc
		}
	}
	r.mu.Unlock()

	loc, resolved := r.do(ctx, "self", r.base+"?fields=status,lat,lon,city,query")
	r.mu.Lock()
	r.cache[key] = entry{loc: loc, when: time.Now(), resolved: resolved}
	r.mu.Unlock()
	return loc
}

// fetch queries ip-api.com for one IP's location.
func (r *Resolver) fetch(ctx context.Context, ip string) (Loc, bool) {
	return r.do(ctx, ip, r.base+ip+"?fields=status,lat,lon,city")
}

// do performs one paced ip-api.com request. The second return is false when the
// request failed transiently (network error, non-200 such as a 429 rate limit,
// or an unreadable body) — the caller caches those only briefly and retries. A
// clean 200 is definitive even when it carries no usable fix.
func (r *Resolver) do(ctx context.Context, label, url string) (Loc, bool) {
	r.wait(ctx)

	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(lctx, http.MethodGet, url, nil)
	if err != nil {
		return Loc{}, false
	}
	resp, err := r.http.Do(req)
	if err != nil {
		r.logFailure("geo: lookup for %s failed: %v", label, err)
		return Loc{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 429 (rate limit) and 5xx are transient — keep the entry retryable.
		r.logFailure("geo: lookup for %s got HTTP %d (rate-limited?); will retry", label, resp.StatusCode)
		return Loc{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return Loc{}, false
	}
	return parseResponse(body), true
}

// wait paces requests to roughly minInterval apart so a cold-start burst stays
// under ip-api.com's free-tier rate limit. It blocks the caller, respecting ctx.
func (r *Resolver) wait(ctx context.Context) {
	r.gate.Lock()
	now := time.Now()
	at := r.nextAt
	if at.Before(now) {
		at = now
	}
	r.nextAt = at.Add(minInterval)
	r.gate.Unlock()

	d := time.Until(at)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// logFailure emits a lookup failure at most once per 30s so a sustained ip-api
// outage is visible without flooding the log.
func (r *Resolver) logFailure(format string, args ...any) {
	r.gate.Lock()
	defer r.gate.Unlock()
	if time.Since(r.lastLog) < 30*time.Second {
		return
	}
	r.lastLog = time.Now()
	log.Printf(format, args...)
}

// parseResponse decodes an ip-api.com JSON reply into a Loc.
func parseResponse(body []byte) Loc {
	var out struct {
		Status string  `json:"status"`
		Lat    float64 `json:"lat"`
		Lon    float64 `json:"lon"`
		City   string  `json:"city"`
		Query  string  `json:"query"` // the resolved IP (only requested by Self)
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Status != "success" {
		return Loc{}
	}
	// Reject bad fixes. The "null island" (~0,0 in the Gulf of Guinea) is where
	// IPs with no real fix get dumped; an exact 0 in either coordinate is likewise
	// "unknown" (a router sitting precisely on the equator or prime meridian is a
	// geolocation artifact, not a real hop), which catches the mid-ocean dots
	// strung along longitude 0.
	if (math.Abs(out.Lat) < 1 && math.Abs(out.Lon) < 1) || out.Lat == 0 || out.Lon == 0 {
		return Loc{}
	}
	return Loc{Lat: out.Lat, Lon: out.Lon, City: strings.TrimSpace(out.City), IP: strings.TrimSpace(out.Query), OK: true}
}

// LatString / LonString render a coordinate as a fixed-precision string for a
// metric label, or "" when there is no fix.
func (l Loc) LatString() string { return coord(l.OK, l.Lat) }
func (l Loc) LonString() string { return coord(l.OK, l.Lon) }

func coord(ok bool, v float64) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf("%.4f", v)
}

// IsPrivate reports whether a textual address is private/bogon/loopback and so
// has no useful public geolocation (your gateway, CGNAT). Unparseable input —
// e.g. an unresponsive hop with no address — is treated as not private.
func IsPrivate(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && isPrivate(ip)
}

// isPrivate reports whether ip has no useful public geolocation.
func isPrivate(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return true
	}
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"} {
		_, n, _ := net.ParseCIDR(c)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
