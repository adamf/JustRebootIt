package underload

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adamf/justrebootit/internal/pinger"
)

// TestGenerateDownloadCountsBytes drives the download load generator against a
// local server and checks it moves data, respects the byte ceiling, and stops
// when the context ends. It exercises the load path without ICMP, so it runs in
// CI where raw sockets aren't available.
func TestGenerateDownloadCountsBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
		_, _ = io.Copy(w, io.LimitReader(zeroReader{}, int64(n)))
	}))
	defer srv.Close()

	p := New(Config{Streams: 3, DownURL: srv.URL, Bytes: 5 * chunkBytes})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var moved atomic.Int64
	p.generate(ctx, "down", &moved)

	if moved.Load() == 0 {
		t.Fatal("download moved no bytes")
	}
	// The ceiling is enforced per-iteration across streams, so the total may
	// overshoot by up to (streams-1) in-flight chunks but must stay bounded.
	if max := p.cfg.Bytes + int64(p.cfg.Streams)*chunkBytes; moved.Load() > max {
		t.Errorf("download moved %d bytes, want <= %d (ceiling honored)", moved.Load(), max)
	}
}

// TestGenerateUploadCountsBytes does the same for the upload generator.
func TestGenerateUploadCountsBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Config{Streams: 2, UpURL: srv.URL, Bytes: 3 * chunkBytes})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var moved atomic.Int64
	p.generate(ctx, "up", &moved)

	if moved.Load() == 0 {
		t.Fatal("upload moved no bytes")
	}
}

// TestGenerateStopsOnContext checks the generator returns promptly once the
// context is cancelled even though the server would happily keep streaming.
func TestGenerateStopsOnContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
		_, _ = io.Copy(w, io.LimitReader(zeroReader{}, int64(n)))
	}))
	defer srv.Close()

	// A huge ceiling so only the context bounds the run.
	p := New(Config{Streams: 2, DownURL: srv.URL, Bytes: 1 << 60})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	var moved atomic.Int64
	p.generate(ctx, "down", &moved)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("generate ran %s after a 300ms context, expected prompt stop", elapsed)
	}
}

func TestGrade(t *testing.T) {
	cases := []struct {
		inc  time.Duration
		want string
	}{
		{2 * time.Millisecond, "A+"},
		{20 * time.Millisecond, "A"},
		{45 * time.Millisecond, "B"},
		{80 * time.Millisecond, "C"},
		{150 * time.Millisecond, "D"},
		{400 * time.Millisecond, "F"},
	}
	for _, c := range cases {
		if got := Grade(c.inc); got != c.want {
			t.Errorf("Grade(%s) = %q, want %q", c.inc, got, c.want)
		}
	}
}

func TestPhaseIncreaseAndRatio(t *testing.T) {
	ph := Phase{
		Idle:   pinger.Result{Recv: 5, Median: 20 * time.Millisecond},
		Loaded: pinger.Result{Recv: 5, Median: 80 * time.Millisecond},
	}
	if got := ph.Increase(); got != 60*time.Millisecond {
		t.Errorf("Increase() = %s, want 60ms", got)
	}
	if got := ph.Ratio(); got != 4 {
		t.Errorf("Ratio() = %v, want 4", got)
	}

	// A lost phase yields zero, not a bogus negative/huge value.
	lost := Phase{Idle: pinger.Result{Recv: 0}, Loaded: pinger.Result{Recv: 0}}
	if got := lost.Increase(); got != 0 {
		t.Errorf("Increase() with no replies = %s, want 0", got)
	}
	if got := lost.Ratio(); got != 0 {
		t.Errorf("Ratio() with no replies = %v, want 0", got)
	}
}
