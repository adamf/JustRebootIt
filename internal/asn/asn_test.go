package asn

import (
	"context"
	"net"
	"testing"
)

func TestFmtReversed(t *testing.T) {
	if got := fmtReversed(net.ParseIP("73.0.0.1").To4()); got != "1.0.0.73" {
		t.Errorf("fmtReversed = %q, want 1.0.0.73", got)
	}
	if got := fmtReversed(net.ParseIP("8.8.4.4").To4()); got != "4.4.8.8" {
		t.Errorf("fmtReversed = %q, want 4.4.8.8", got)
	}
}

func TestParseOriginTXT(t *testing.T) {
	cases := map[string]string{
		"7922 | 73.0.0.0/8 | US | arin | 1997-09-19": "7922",
		"7922 13335 | 1.0.0.0/24 | US | arin":        "7922", // multi-origin → first
		"":                                           "",
	}
	for in, want := range cases {
		if got := parseOriginTXT([]string{in}); got != want {
			t.Errorf("parseOriginTXT(%q) = %q, want %q", in, got, want)
		}
	}
	if got := parseOriginTXT(nil); got != "" {
		t.Errorf("parseOriginTXT(nil) = %q, want empty", got)
	}
}

func TestParseNameTXT(t *testing.T) {
	in := "7922 | US | arin | 1997-09-19 | COMCAST-7922, US"
	if got := parseNameTXT([]string{in}); got != "COMCAST-7922" {
		t.Errorf("parseNameTXT = %q, want COMCAST-7922", got)
	}
}

func TestIsPrivate(t *testing.T) {
	priv := []string{"10.1.2.3", "192.168.1.1", "172.16.0.1", "100.64.0.1", "127.0.0.1"}
	for _, ip := range priv {
		if !isPrivate(net.ParseIP(ip)) {
			t.Errorf("%s should be private", ip)
		}
	}
	pub := []string{"73.0.0.1", "8.8.8.8", "1.1.1.1"}
	for _, ip := range pub {
		if isPrivate(net.ParseIP(ip)) {
			t.Errorf("%s should be public", ip)
		}
	}
}

func TestLookupSkipsPrivate(t *testing.T) {
	// A private address never hits the network — returns empty without a lookup.
	r := New(0)
	if got := r.Lookup(context.Background(), "192.168.1.1"); got != (Info{}) {
		t.Errorf("private lookup = %+v, want empty", got)
	}
}
