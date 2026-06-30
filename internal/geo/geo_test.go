package geo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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
		`{"status":"success","lat":5.0,"lon":0,"city":"gulf of guinea longitude-0 artifact"}`,
		`{"status":"success","lat":0,"lon":-71.0,"city":"equator artifact"}`,
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

func TestIsPrivate(t *testing.T) {
	for _, ip := range []string{"192.168.1.1", "10.0.0.1", "127.0.0.1", "100.64.0.1", "172.16.5.4"} {
		if !IsPrivate(ip) {
			t.Errorf("%s should be private", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "", "not-an-ip"} {
		if IsPrivate(ip) {
			t.Errorf("%s should not be private", ip)
		}
	}
}

func TestSelfReadsOwnIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","lat":42.3601,"lon":-71.0589,"city":"Boston","query":"203.0.113.7"}`))
	}))
	defer srv.Close()
	r := New(0)
	r.base = srv.URL + "/"

	loc := r.Self(context.Background())
	if !loc.OK || loc.IP != "203.0.113.7" || loc.City != "Boston" {
		t.Fatalf("Self = %+v, want Boston/203.0.113.7", loc)
	}
}

func TestLookupRetriesAfterTransientFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests) // first call: rate-limited
			return
		}
		w.Write([]byte(`{"status":"success","lat":42.3601,"lon":-71.0589,"city":"Boston"}`))
	}))
	defer srv.Close()
	r := New(0)
	r.base = srv.URL + "/"

	// First lookup hits the 429 and must not be cached as a permanent no-fix.
	if loc := r.Lookup(context.Background(), "8.8.8.8"); loc.OK {
		t.Fatalf("first lookup should fail, got %+v", loc)
	}
	if e := r.cache["8.8.8.8"]; e.resolved {
		t.Fatal("a transient failure must be cached as unresolved (retryable)")
	}
	// Age the failed entry past negTTL; the next lookup should retry and succeed.
	r.cache["8.8.8.8"] = entry{when: time.Now().Add(-negTTL - time.Second)}
	if loc := r.Lookup(context.Background(), "8.8.8.8"); !loc.OK || loc.City != "Boston" {
		t.Fatalf("retry should succeed, got %+v", loc)
	}
}
