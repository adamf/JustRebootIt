package tracer

import (
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// checksum marshals an ICMP echo with the given id/seq/data and returns the
// checksum field (bytes 2:3) that Marshal computed.
func checksum(t *testing.T, id, seq int, data []byte) uint16 {
	t.Helper()
	msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Code: 0, Body: &icmp.Echo{ID: id, Seq: seq, Data: data}}
	b, err := msg.Marshal(nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return uint16(b[2])<<8 | uint16(b[3])
}

func TestParisPayloadHoldsChecksumConstant(t *testing.T) {
	// With the compensation payload, every (id, seq) pair yields the SAME ICMP
	// checksum, so per-flow ECMP routes every probe down one path.
	want := checksum(t, 0x1234, 1, parisPayload(0x1234, 1))
	for _, c := range []struct{ id, seq int }{{0x1234, 2}, {0x1234, 30}, {0xABCD, 1}, {0x0001, 15}} {
		if got := checksum(t, c.id, c.seq, parisPayload(c.id, c.seq)); got != want {
			t.Errorf("checksum for id=%#x seq=%d = %#x, want constant %#x", c.id, c.seq, got, want)
		}
	}

	// Sanity: without compensation (classic traceroute), varying seq changes the
	// checksum — that is exactly the ECMP-scattering behaviour Paris avoids.
	plain := []byte("justrebootit")
	if checksum(t, 0x1234, 1, plain) == checksum(t, 0x1234, 2, plain) {
		t.Error("expected classic (uncompensated) checksums to differ across seq")
	}
}
