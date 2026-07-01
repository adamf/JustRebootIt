// Package tracer implements a small ICMP traceroute for IPv4. It exists to map
// the network path to each target and measure per-hop latency, so a latency
// spike can be attributed to a specific segment (home LAN, ISP access network,
// peering, or the far end) rather than just "the internet is slow".
package tracer

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// idCounter hands each Tracer a distinct ICMP id. Raw ICMP sockets receive a
// copy of every ICMP packet on the host, so concurrent tracers (and the
// pinger) would otherwise be indistinguishable and steal each other's replies.
var idCounter atomic.Uint32

// Hop is the result of probing a single TTL.
type Hop struct {
	// TTL is the time-to-live used for this probe (the hop number).
	TTL int
	// Addr is the responding router's address, or empty if the hop timed out.
	Addr string
	// RTT is the round-trip time to the responding router.
	RTT time.Duration
	// Timeout is true when no reply arrived within the per-hop budget.
	Timeout bool
}

// Result is a full traceroute to a target.
type Result struct {
	// Dest is the resolved destination address that was traced.
	Dest string
	// Hops is the path, ordered by increasing TTL. The final hop reaching the
	// destination has Reached set on the Result.
	Hops []Hop
	// Reached is true if the destination answered before MaxHops was exhausted.
	Reached bool
}

// LossHop is one hop's aggregate over several traceroute passes: how often the
// router at this TTL answered, used to localize where packet loss begins on a
// path. Loss is a fraction in [0,1] over the passes that probed this TTL.
//
// IMPORTANT interpretation: loss at a single mid-path hop that does NOT persist
// to later hops is almost always ICMP rate-limiting by that router, not real
// path loss — routers deprioritize generating TTL-exceeded replies. Real packet
// loss shows up as loss that PERSISTS across consecutive hops toward the
// destination. Correlate with the destination's own probe_loss_ratio.
type LossHop struct {
	TTL  int
	Addr string
	Loss float64
	RTT  time.Duration // mean RTT over the passes that answered
	// ASN and ASName are filled in by the caller (via an AS resolver) from Addr.
	// Handoff is true when this hop's ASN differs from the previous responding
	// hop's ASN — a peering/transit boundary, where congestion often lives.
	ASN     string
	ASName  string
	Handoff bool
}

// Boundary is an AS handoff on a path: the last responding hop in one AS (Near)
// and the first in the next (Far). Probing both sides over time reveals
// congestion building at the interconnect between them (the TSLP technique).
type Boundary struct {
	FromASN  string
	FromName string
	ToASN    string
	ToName   string
	NearAddr string
	FarAddr  string
	TTL      int // the far hop's TTL, for reference
}

// Boundaries extracts the AS-handoff boundaries from an enriched path, pairing
// each hop marked Handoff with the preceding hop that still carries the prior
// AS. Hops with no address or no ASN (private/unresponsive) are skipped, so the
// pairing matches how Handoff was assigned in the first place.
func Boundaries(hops []LossHop) []Boundary {
	var out []Boundary
	prev := -1
	for i := range hops {
		if hops[i].Addr == "" || hops[i].ASN == "" {
			continue
		}
		if hops[i].Handoff && prev >= 0 {
			p, h := hops[prev], hops[i]
			out = append(out, Boundary{
				FromASN:  p.ASN,
				FromName: p.ASName,
				ToASN:    h.ASN,
				ToName:   h.ASName,
				NearAddr: p.Addr,
				FarAddr:  h.Addr,
				TTL:      h.TTL,
			})
		}
		prev = i
	}
	return out
}

// AggregateLoss reduces several traceroute passes to per-TTL loss/RTT. Passes
// can stop at different TTLs (a pass that reached the destination is shorter), so
// a TTL's loss is computed only over the passes that actually probed it.
func AggregateLoss(results []Result) []LossHop {
	maxLen := 0
	for _, r := range results {
		if len(r.Hops) > maxLen {
			maxLen = len(r.Hops)
		}
	}
	out := make([]LossHop, 0, maxLen)
	for ttl := 1; ttl <= maxLen; ttl++ {
		var attempts, replies, n int
		var rttSum time.Duration
		addr := ""
		for _, r := range results {
			if len(r.Hops) < ttl {
				continue // this pass stopped before reaching this TTL
			}
			attempts++
			h := r.Hops[ttl-1]
			if h.Addr != "" && !h.Timeout {
				replies++
				rttSum += h.RTT
				n++
				addr = h.Addr
			}
		}
		if attempts == 0 {
			continue
		}
		hop := LossHop{
			TTL:  ttl,
			Addr: addr,
			Loss: float64(attempts-replies) / float64(attempts),
		}
		if n > 0 {
			hop.RTT = rttSum / time.Duration(n)
		}
		out = append(out, hop)
	}
	return out
}

// Tracer performs ICMP traceroutes. A single Tracer serializes its probes (one
// outstanding TTL at a time), which keeps reply matching simple and the extra
// traffic negligible.
type Tracer struct {
	maxHops    int
	timeout    time.Duration
	privileged bool
	id         int
}

// New constructs a Tracer. privileged selects raw ICMP sockets (CAP_NET_RAW),
// which is the reliable choice inside a container granted NET_RAW.
func New(maxHops int, timeout time.Duration, privileged bool) *Tracer {
	return &Tracer{
		maxHops:    maxHops,
		timeout:    timeout,
		privileged: privileged,
		// Combine the PID with a per-instance counter so each Tracer has a
		// distinct 16-bit ICMP id and concurrent traces don't claim each
		// other's replies on the shared raw socket.
		id: (os.Getpid() + int(idCounter.Add(1))) & 0xffff,
	}
}

// network and listenAddr describe the socket to open for the configured mode.
func (t *Tracer) network() (proto, addr string) {
	if t.privileged {
		return "ip4:icmp", "0.0.0.0"
	}
	return "udp4", "0.0.0.0"
}

// Trace runs a traceroute to host, returning the discovered path. It stops
// early once the destination replies. ctx cancellation aborts the trace.
func (t *Tracer) Trace(ctx context.Context, host string) (Result, error) {
	dst, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		return Result{}, fmt.Errorf("resolve %q: %w", host, err)
	}

	proto, laddr := t.network()
	conn, err := icmp.ListenPacket(proto, laddr)
	if err != nil {
		return Result{}, fmt.Errorf("listen icmp: %w", err)
	}
	defer conn.Close()
	p4 := conn.IPv4PacketConn()

	res := Result{Dest: dst.String()}
	for ttl := 1; ttl <= t.maxHops; ttl++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		hop, reached := t.probeHop(ctx, conn, p4, dst, ttl)
		res.Hops = append(res.Hops, hop)
		if reached {
			res.Reached = true
			break
		}
	}
	return res, nil
}

// probeHop sends one echo request at the given TTL and waits for either an
// ICMP time-exceeded (an intermediate router) or an echo reply (the
// destination). The bool return reports whether the destination was reached.
func (t *Tracer) probeHop(ctx context.Context, conn *icmp.PacketConn, p4 *ipv4.PacketConn, dst *net.IPAddr, ttl int) (Hop, bool) {
	hop := Hop{TTL: ttl}

	if err := p4.SetTTL(ttl); err != nil {
		hop.Timeout = true
		return hop, false
	}

	seq := ttl
	data := []byte("justrebootit")
	if t.privileged {
		// Paris-style probing: hold the ICMP checksum constant across every probe
		// (all TTLs, all passes) so per-flow ECMP load-balancers route them down a
		// single path. Classic traceroute varies the sequence number — and thus
		// the checksum — per probe, scattering probes across parallel links and
		// inventing phantom loss / duplicate hops. Only meaningful on raw sockets,
		// where our bytes reach the wire unaltered (datagram ICMP lets the kernel
		// rewrite the id and checksum).
		data = parisPayload(t.id, seq)
	}
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: t.id, Seq: seq, Data: data},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		hop.Timeout = true
		return hop, false
	}

	start := time.Now()
	if _, err := conn.WriteTo(wb, dst); err != nil {
		hop.Timeout = true
		return hop, false
	}

	deadline := start.Add(t.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			hop.Timeout = true
			return hop, false
		}
		rtt := time.Since(start)

		msg, err := icmp.ParseMessage(1 /* ICMPv4 */, buf[:n])
		if err != nil {
			continue
		}
		switch body := msg.Body.(type) {
		case *icmp.TimeExceeded:
			if !t.matchesEmbedded(body.Data, seq) {
				continue // some other flow's TTL expiry; ignore
			}
			hop.Addr = peer.String()
			hop.RTT = rtt
			return hop, false
		case *icmp.Echo:
			// An echo reply means we reached a host: it must be the
			// destination, not some other target's reply landing on our shared
			// socket. Datagram sockets rewrite the id, so only trust seq there.
			if body.Seq != seq || (t.privileged && body.ID != t.id) {
				continue
			}
			if !sameIP(peer, dst) {
				continue
			}
			hop.Addr = peer.String()
			hop.RTT = rtt
			return hop, true
		default:
			continue
		}
	}
}

// parisPayload builds the echo payload for Paris-style tracing: a fixed tag plus
// two compensation words that cancel the id and sequence number out of the ICMP
// checksum. A 16-bit word W and its complement ^W sum to 0xFFFF, which is a
// no-op (negative zero) in the ones-complement Internet checksum — so embedding
// ^seq and ^id makes the checksum independent of both, i.e. constant across
// every probe. With the checksum fixed, per-flow ECMP hashing sends all probes
// down one path. The id and seq still travel in the ICMP header (and are quoted
// back in TTL-exceeded errors), so reply matching is unchanged.
func parisPayload(id, seq int) []byte {
	b := make([]byte, 12, 16)
	copy(b, "justrebootit")
	var comp [4]byte
	binary.BigEndian.PutUint16(comp[0:2], ^uint16(seq))
	binary.BigEndian.PutUint16(comp[2:4], ^uint16(id))
	return append(b, comp[:]...)
}

// sameIP reports whether a reply's source address is the traced destination.
func sameIP(peer net.Addr, dst *net.IPAddr) bool {
	switch p := peer.(type) {
	case *net.IPAddr:
		return p.IP.Equal(dst.IP)
	case *net.UDPAddr:
		return p.IP.Equal(dst.IP)
	default:
		return peer.String() == dst.String()
	}
}

// matchesEmbedded reports whether the quoted packet inside an ICMP error is the
// echo request we sent with the given sequence number. ICMP errors quote the
// original IP header plus at least the first 8 bytes of its payload, which for
// our echo covers the id and sequence fields.
func (t *Tracer) matchesEmbedded(data []byte, seq int) bool {
	if len(data) < 1 {
		return false
	}
	ihl := int(data[0]&0x0f) * 4
	// Need the original IP header plus 8 bytes of ICMP (type,code,cksum,id,seq).
	if len(data) < ihl+8 {
		return false
	}
	orig := data[ihl:]
	embSeq := int(binary.BigEndian.Uint16(orig[6:8]))
	if embSeq != seq {
		return false
	}
	if t.privileged {
		embID := int(binary.BigEndian.Uint16(orig[4:6]))
		return embID == t.id
	}
	return true
}
