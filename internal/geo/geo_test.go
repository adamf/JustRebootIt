package geo

import (
	"context"
	"testing"
)

func TestParseResponse(t *testing.T) {
	ok := parseResponse([]byte(`{"status":"success","lat":42.3601,"lon":-71.0589,"city":"Boston"}`))
	if !ok.OK || ok.City != "Boston" || ok.Lat != 42.3601 || ok.Lon != -71.0589 {
		t.Errorf("unexpected parse: %+v", ok)
	}
	// A failed lookup, malformed JSON, and the null island all yield no fix.
	for _, body := range []string{
		`{"status":"fail","message":"private range"}`,
		`not json`,
		`{"status":"success","lat":0,"lon":0,"city":""}`,
		`{"status":"success","lat":0.5,"lon":-0.2,"city":"null island"}`,
	} {
		if got := parseResponse([]byte(body)); got.OK {
			t.Errorf("parseResponse(%q) should be no-fix, got %+v", body, got)
		}
	}
}

func TestCoordStrings(t *testing.T) {
	l := Loc{Lat: 42.3601, Lon: -71.0589, OK: true}
	if l.LatString() != "42.3601" || l.LonString() != "-71.0589" {
		t.Errorf("coord strings = %q,%q", l.LatString(), l.LonString())
	}
	if (Loc{}).LatString() != "" {
		t.Error("no-fix LatString should be empty")
	}
}

func TestLookupSkipsPrivate(t *testing.T) {
	// Private/loopback addresses never hit the network.
	r := New(0)
	for _, ip := range []string{"192.168.1.1", "10.0.0.1", "127.0.0.1", "100.64.0.1"} {
		if got := r.Lookup(context.Background(), ip); got.OK {
			t.Errorf("%s should have no geolocation, got %+v", ip, got)
		}
	}
}
