// Package underload measures latency-under-load — bufferbloat. A plain ping
// samples the link while it is idle, which is precisely when an oversized buffer
// is empty and hides the problem. The stutter that breaks a Plex stream or a
// video call appears only while the link is saturated: a bulk transfer fills the
// buffer, every other packet queues behind it, and round-trip time jumps from a
// few milliseconds to hundreds. This package reproduces that condition on
// purpose — it saturates the link with a controlled transfer while pinging a
// stable host — and reports the idle-vs-loaded RTT difference, the bufferbloat,
// as a measurement the dashboard can graph.
//
// It moves real data, so the caller bounds it: a byte ceiling and a short
// duration per run, run on a generous interval and only when explicitly enabled.
package underload

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adamf/justrebootit/internal/pinger"
)

// Config controls a single latency-under-load probe.
type Config struct {
	// Host is the address pinged while the link is loaded.
	Host string
	// Direction is "down", "up", or "both".
	Direction string
	// Duration is how long each direction holds the link under load.
	Duration time.Duration
	// Streams is the number of parallel transfer connections.
	Streams int
	// DownURL is fetched (with a bytes query parameter) to load the downlink.
	DownURL string
	// UpURL receives a POST body to load the uplink.
	UpURL string
	// Bytes caps the total data moved per direction per run.
	Bytes int64
	// Pings is the RTT samples per phase (idle and loaded).
	Pings int
	// Timeout is the per-ping reply timeout.
	Timeout time.Duration
	// Privileged selects raw vs datagram ICMP for the ping.
	Privileged bool
}

// Phase is the result of loading the link in one direction.
type Phase struct {
	Direction string
	// Idle and Loaded are the ping statistics before and during the load.
	Idle   pinger.Result
	Loaded pinger.Result
	// Bytes is the data moved and Elapsed the time it took; Bps is the achieved
	// throughput in bits per second.
	Bytes   int64
	Elapsed time.Duration
	Bps     float64
}

// Increase is the loaded-minus-idle median RTT — the bufferbloat. It is only
// meaningful when both phases received replies.
func (p Phase) Increase() time.Duration {
	if p.Idle.Recv == 0 || p.Loaded.Recv == 0 {
		return 0
	}
	return p.Loaded.Median - p.Idle.Median
}

// Ratio is the loaded median RTT as a multiple of the idle median (0 when not
// measurable).
func (p Phase) Ratio() float64 {
	if p.Idle.Recv == 0 || p.Loaded.Recv == 0 || p.Idle.Median <= 0 {
		return 0
	}
	return float64(p.Loaded.Median) / float64(p.Idle.Median)
}

// Result holds one or both direction phases from a single run.
type Result struct {
	Phases []Phase
}

// Prober runs latency-under-load tests against a configured host.
type Prober struct {
	cfg  Config
	http *http.Client
}

// New builds a Prober.
func New(cfg Config) *Prober {
	// A transport that allows several concurrent connections to the load
	// endpoint; no client-level timeout because each transfer is bounded by the
	// caller's context (the load window) instead.
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        cfg.Streams * 2,
		MaxIdleConnsPerHost: cfg.Streams * 2,
		MaxConnsPerHost:     cfg.Streams * 2,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Prober{cfg: cfg, http: &http.Client{Transport: tr}}
}

// Run executes the configured directions and returns their phases. It honors
// ctx for early cancellation.
func (p *Prober) Run(ctx context.Context) Result {
	var res Result
	for _, dir := range p.directions() {
		if ctx.Err() != nil {
			break
		}
		res.Phases = append(res.Phases, p.runPhase(ctx, dir))
	}
	return res
}

func (p *Prober) directions() []string {
	if p.cfg.Direction == "both" {
		return []string{"down", "up"}
	}
	return []string{p.cfg.Direction}
}

// runPhase measures the idle RTT, then saturates the link in dir while
// re-measuring RTT, and reports both with the achieved throughput.
func (p *Prober) runPhase(ctx context.Context, dir string) Phase {
	pg := pinger.New(p.cfg.Host, p.cfg.Pings, p.cfg.Timeout, p.cfg.Privileged)

	// Idle baseline: a quick cycle with the link unloaded.
	idleWin := p.cfg.Timeout + time.Duration(p.cfg.Pings-1)*100*time.Millisecond
	idle := p.ping(ctx, pg, idleWin)

	// Start the load and let the buffer fill before we start timing RTT, so the
	// loaded samples reflect a steady-state full queue rather than the ramp.
	loadCtx, cancelLoad := context.WithTimeout(ctx, p.cfg.Duration)
	var moved atomic.Int64
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.generate(loadCtx, dir, &moved)
	}()

	ramp := p.cfg.Duration / 4
	if ramp > 2*time.Second {
		ramp = 2 * time.Second
	}
	sleep(ctx, ramp)

	// Ping under load over the rest of the window, leaving a small tail so the
	// transfer is still running through the final samples.
	loadedWin := p.cfg.Duration - ramp - 300*time.Millisecond
	if min := p.cfg.Timeout + 300*time.Millisecond; loadedWin < min {
		loadedWin = min
	}
	loaded := p.ping(ctx, pg, loadedWin)

	cancelLoad()
	<-done
	elapsed := time.Since(start)
	bytes := moved.Load()
	bps := 0.0
	if elapsed > 0 {
		bps = float64(bytes) * 8 / elapsed.Seconds()
	}
	return Phase{
		Direction: dir,
		Idle:      idle,
		Loaded:    loaded,
		Bytes:     bytes,
		Elapsed:   elapsed,
		Bps:       bps,
	}
}

// ping runs one bounded ping cycle, giving Run a hard end via its own timeout.
func (p *Prober) ping(ctx context.Context, pg *pinger.Pinger, window time.Duration) pinger.Result {
	pctx, cancel := context.WithTimeout(ctx, window+time.Second)
	defer cancel()
	return pg.Run(pctx, window)
}

// generate saturates the link in dir until ctx is cancelled (the load window)
// or the byte ceiling is reached, accumulating bytes moved into moved.
func (p *Prober) generate(ctx context.Context, dir string, moved *atomic.Int64) {
	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil && moved.Load() < p.cfg.Bytes {
				if dir == "up" {
					p.uploadChunk(ctx, moved)
				} else {
					p.downloadChunk(ctx, moved)
				}
			}
		}()
	}
	wg.Wait()
}

// chunkBytes is the size of a single transfer request. It is large enough to
// keep a stream busy but small enough that streams re-check the byte ceiling
// often.
const chunkBytes int64 = 25_000_000

// downloadChunk fetches up to chunkBytes from the download endpoint, counting
// the bytes received. Transfer errors end the chunk quietly; the next loop
// iteration (or the context) decides whether to continue.
func (p *Prober) downloadChunk(ctx context.Context, moved *atomic.Int64) {
	u, err := url.Parse(p.cfg.DownURL)
	if err != nil {
		return
	}
	q := u.Query()
	q.Set("bytes", strconv.FormatInt(chunkBytes, 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(countWriter{moved}, resp.Body)
}

// uploadChunk POSTs up to chunkBytes to the upload endpoint, counting the bytes
// sent.
func (p *Prober) uploadChunk(ctx context.Context, moved *atomic.Int64) {
	body := &countReader{r: io.LimitReader(zeroReader{}, chunkBytes), n: moved}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.UpURL, body)
	if err != nil {
		return
	}
	req.ContentLength = chunkBytes
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := p.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// Grade maps a loaded-vs-idle RTT increase to a bufferbloat letter grade, the
// same A–F scale the Waveform/DSLReports bufferbloat tests use.
func Grade(increase time.Duration) string {
	switch ms := increase.Milliseconds(); {
	case ms < 5:
		return "A+"
	case ms < 30:
		return "A"
	case ms < 60:
		return "B"
	case ms < 100:
		return "C"
	case ms < 200:
		return "D"
	default:
		return "F"
	}
}

// sleep waits for d or until ctx is done, whichever comes first.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// countWriter counts bytes written through it and discards them.
type countWriter struct{ n *atomic.Int64 }

func (c countWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// countReader counts bytes read through it.
type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// zeroReader is an infinite source of zero bytes for upload bodies.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
