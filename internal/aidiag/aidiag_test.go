package aidiag

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/adamf/justrebootit/internal/tracer"
)

func TestNewDisabledReturnsNil(t *testing.T) {
	// Disabled, or enabled-but-keyless, must yield a nil Analyzer so callers can
	// safely skip analysis.
	if a, err := New(Config{Enabled: false, APIKey: "sk-test"}); err != nil || a != nil {
		t.Errorf("disabled: got (%v, %v), want (nil, nil)", a, err)
	}
	if a, err := New(Config{Enabled: true, APIKey: ""}); err != nil || a != nil {
		t.Errorf("no key: got (%v, %v), want (nil, nil)", a, err)
	}
	if a, err := New(Config{Enabled: true, APIKey: "sk-test"}); err != nil || a == nil {
		t.Errorf("enabled+key: got (%v, %v), want non-nil analyzer", a, err)
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Congestion at hop 3.\nMore detail.", "Congestion at hop 3."},
		{"# Heading\nbody", "Heading"},
		{"\n\n  Leading blanks then text", "Leading blanks then text"},
		{"single", "single"},
	}
	for _, tc := range tests {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUserPromptIncludesEventDetails(t *testing.T) {
	ev := Event{
		ID:       7,
		Target:   "comcast-dns-1",
		Host:     "75.75.75.75",
		Group:    "isp",
		Reason:   "latency",
		Median:   120 * time.Millisecond,
		Baseline: 20 * time.Millisecond,
		Loss:     0,
		When:     time.Date(2026, 6, 10, 4, 0, 0, 0, time.UTC),
		Trace: tracer.Result{
			Reached: true,
			Hops: []tracer.Hop{
				{TTL: 1, Addr: "192.168.1.1", RTT: time.Millisecond},
				{TTL: 2, Timeout: true},
				{TTL: 3, Addr: "96.120.0.1", RTT: 110 * time.Millisecond},
			},
		},
		TCPConnect: 95 * time.Millisecond,
		TCPOK:      true,
		DNSOK:      false,
	}
	p := userPrompt(ev)
	for _, want := range []string{"event #7", "comcast-dns-1", "75.75.75.75", "6.0x baseline", "hop 3: 96.120.0.1", "hop 2: *", "DNS resolution probe: FAILED"} {
		if !strings.Contains(p, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, p)
		}
	}
}

func TestRDAPOrgExtraction(t *testing.T) {
	// A trimmed RDAP IP response with a registrant entity carrying an fn vCard.
	raw := `{
      "name": "COMCAST-72",
      "handle": "NET-73-0-0-0-1",
      "country": "US",
      "entities": [
        {"roles": ["registrant"],
         "vcardArray": ["vcard", [
            ["version", {}, "text", "4.0"],
            ["fn", {}, "text", "Comcast Cable Communications, LLC"]
         ]]}
      ]
    }`
	var r rdapResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := rdapOrg(r); got != "Comcast Cable Communications, LLC" {
		t.Errorf("rdapOrg = %q, want the Comcast org name", got)
	}
}
