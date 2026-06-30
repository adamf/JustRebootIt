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
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Loc is an approximate location for an IP.
type Loc struct {
	Lat  float64
	Lon  float64
	City string
	OK   bool // false for private/bogon addresses or a failed lookup
}

type entry struct {
	loc  Loc
	when time.Time
}

// Resolver looks up and caches IP-to-location mappings. Safe for concurrent use.
type Resolver struct {
	base string
	http *http.Client
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]entry
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
	if e, ok := r.cache[ip]; ok && time.Since(e.when) < r.ttl {
		r.mu.Unlock()
		return e.loc
	}
	r.mu.Unlock()

	loc := r.fetch(ctx, ip)

	r.mu.Lock()
	r.cache[ip] = entry{loc: loc, when: time.Now()}
	r.mu.Unlock()
	return loc
}

func (r *Resolver) fetch(ctx context.Context, ip string) Loc {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(lctx, http.MethodGet, r.base+ip+"?fields=status,lat,lon,city", nil)
	if err != nil {
		return Loc{}
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return Loc{}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return Loc{}
	}
	return parseResponse(body)
}

// parseResponse decodes an ip-api.com JSON reply into a Loc.
func parseResponse(body []byte) Loc {
	var out struct {
		Status string  `json:"status"`
		Lat    float64 `json:"lat"`
		Lon    float64 `json:"lon"`
		City   string  `json:"city"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Status != "success" {
		return Loc{}
	}
	// Reject the "null island" (~0,0 in the Gulf of Guinea), where IPs with no
	// real fix get dumped — no legitimate hop on a residential path is there, so
	// a near-zero coordinate is a bad geolocation, not a mid-ocean router.
	if math.Abs(out.Lat) < 1 && math.Abs(out.Lon) < 1 {
		return Loc{}
	}
	return Loc{Lat: out.Lat, Lon: out.Lon, City: strings.TrimSpace(out.City), OK: true}
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
