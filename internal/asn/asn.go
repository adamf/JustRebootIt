// Package asn resolves an IP address to its origin Autonomous System (ASN and
// name) using Team Cymru's DNS-based IP-to-ASN service — the standard, fast,
// free mechanism that traceroute tools use for bulk lookups. Results are cached
// because AS ownership changes rarely. This lets the prober mark where a path
// crosses an AS boundary (e.g. the home ISP handing off to a transit provider),
// which is where peering/transit congestion and packet loss most often live.
package asn

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Info is the AS that originates a prefix.
type Info struct {
	ASN  string // e.g. "7922" (empty when unknown or a private/bogon address)
	Name string // e.g. "COMCAST-7922" (best-effort; may be empty)
}

type entry struct {
	info Info
	when time.Time
}

// Resolver looks up and caches IP-to-ASN mappings. It is safe for concurrent use.
type Resolver struct {
	res *net.Resolver
	ttl time.Duration

	mu    sync.Mutex
	cache map[string]entry
}

// New builds a Resolver. Lookups are cached for ttl; pass 0 for a 6h default.
func New(ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	return &Resolver{res: &net.Resolver{}, ttl: ttl, cache: make(map[string]entry)}
}

// Lookup returns the origin AS for ip. It returns an empty Info for private,
// loopback, link-local, or CGNAT addresses (which have no public AS), on any
// lookup error, and for non-IPv4 input. now is taken internally; the call is
// bounded by ctx and a short internal deadline.
func (r *Resolver) Lookup(ctx context.Context, ip string) Info {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil || isPrivate(parsed) {
		return Info{}
	}

	r.mu.Lock()
	if e, ok := r.cache[ip]; ok && time.Since(e.when) < r.ttl {
		r.mu.Unlock()
		return e.info
	}
	r.mu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info := Info{ASN: r.originASN(lctx, parsed.To4())}
	if info.ASN != "" {
		info.Name = r.asName(lctx, info.ASN)
	}

	r.mu.Lock()
	r.cache[ip] = entry{info: info, when: time.Now()}
	r.mu.Unlock()
	return info
}

// originASN queries d.c.b.a.origin.asn.cymru.com TXT, whose answer looks like
// "7922 | 73.0.0.0/8 | US | arin | ...". When several ASNs originate the prefix
// (space-separated in the first field) the first is returned.
func (r *Resolver) originASN(ctx context.Context, ip4 net.IP) string {
	txt, err := r.res.LookupTXT(ctx, fmtReversed(ip4)+".origin.asn.cymru.com")
	if err != nil {
		return ""
	}
	return parseOriginTXT(txt)
}

// parseOriginTXT pulls the first ASN from an origin.asn.cymru.com answer.
func parseOriginTXT(txt []string) string {
	if len(txt) == 0 {
		return ""
	}
	fields := strings.Split(txt[0], "|")
	first := strings.Fields(strings.TrimSpace(fields[0]))
	if len(first) == 0 {
		return ""
	}
	return first[0]
}

// asName queries AS<asn>.asn.cymru.com TXT, whose answer looks like
// "7922 | US | arin | 1997-... | COMCAST-7922, US". The handle before the comma
// in the last field is returned.
func (r *Resolver) asName(ctx context.Context, asn string) string {
	txt, err := r.res.LookupTXT(ctx, "AS"+asn+".asn.cymru.com")
	if err != nil {
		return ""
	}
	return parseNameTXT(txt)
}

// parseNameTXT pulls the AS handle (before the country comma) from an
// AS<n>.asn.cymru.com answer.
func parseNameTXT(txt []string) string {
	if len(txt) == 0 {
		return ""
	}
	fields := strings.Split(txt[0], "|")
	last := strings.TrimSpace(fields[len(fields)-1])
	if i := strings.IndexByte(last, ','); i >= 0 {
		last = strings.TrimSpace(last[:i])
	}
	return last
}

// fmtReversed renders an IPv4 address as the reversed dotted-octet label Team
// Cymru expects (a.b.c.d -> d.c.b.a).
func fmtReversed(ip4 net.IP) string {
	return itoa(ip4[3]) + "." + itoa(ip4[2]) + "." + itoa(ip4[1]) + "." + itoa(ip4[0])
}

func itoa(b byte) string {
	if b == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for b > 0 {
		i--
		buf[i] = '0' + b%10
		b /= 10
	}
	return string(buf[i:])
}

// isPrivate reports whether ip has no public AS (private, loopback, link-local,
// or CGNAT space), so a lookup would be pointless.
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
