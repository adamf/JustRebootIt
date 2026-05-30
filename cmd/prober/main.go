// Command prober continuously measures latency and packet loss to a set of
// diverse targets and exposes the results as Prometheus metrics. Each target is
// probed concurrently; within a target, pings are smeared across the cycle the
// way smokeping does so brief spikes are not missed. Trace-enabled targets also
// get periodic ICMP traceroutes for per-hop attribution.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/adamf/justrebootit/internal/config"
	"github.com/adamf/justrebootit/internal/metrics"
	"github.com/adamf/justrebootit/internal/pinger"
	"github.com/adamf/justrebootit/internal/tracer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "/etc/justrebootit/targets.yml", "path to the prober config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("loaded %d targets from %s (interval=%s, pings=%d)",
		len(cfg.Targets), *configPath, cfg.Interval, cfg.Pings)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for _, t := range cfg.Targets {
		wg.Add(1)
		go func(t config.Target) {
			defer wg.Done()
			runPingLoop(ctx, cfg, t, m)
		}(t)

		if t.Trace {
			wg.Add(1)
			go func(t config.Target) {
				defer wg.Done()
				runTraceLoop(ctx, cfg, t, m)
			}(t)
		}
	}

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: metricsHandler(reg)}
	go func() {
		log.Printf("serving metrics on %s/metrics", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("metrics server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	wg.Wait()
}

func metricsHandler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("justrebootit prober\nsee /metrics\n"))
	})
	return mux
}

// runPingLoop probes one target once per cycle until the context is cancelled.
// The very first cycle is jittered so that many targets do not all fire in
// lockstep at startup.
func runPingLoop(ctx context.Context, cfg config.Config, t config.Target, m *metrics.Metrics) {
	p := pinger.New(t.Host, cfg.Pings, cfg.Timeout, cfg.Privileged)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		res := p.Run(ctx, cfg.Interval)
		if res.Err != nil {
			// A hard error (e.g. unresolvable host) — record down with full
			// loss and keep trying; transient DNS or routing failures often
			// recover. Reporting loss=1 (rather than 0) keeps the loss panel
			// honest while probe_up=0 flags the underlying failure.
			log.Printf("ping %s (%s): %v", t.Name, t.Host, res.Err)
			m.ObserveProbe(t.Name, t.Group, pinger.Result{Sent: cfg.Pings, Loss: 1})
		} else {
			m.ObserveProbe(t.Name, t.Group, res)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runTraceLoop traces one target on the trace interval until cancelled.
func runTraceLoop(ctx context.Context, cfg config.Config, t config.Target, m *metrics.Metrics) {
	tr := tracer.New(cfg.TraceMaxHops, cfg.TraceTimeout, cfg.Privileged)

	ticker := time.NewTicker(cfg.TraceInterval)
	defer ticker.Stop()
	for {
		res, err := tr.Trace(ctx, t.Host)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("trace %s (%s): %v", t.Name, t.Host, err)
		} else {
			m.ObserveTrace(t.Name, t.Group, res)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
