// Command udmexporter exposes UniFi Dream Machine Pro statistics as Prometheus
// metrics so WAN throughput, gateway load, and the gateway's own latency can be
// graphed alongside the externally measured latency from the prober. It is
// configured entirely through environment variables so credentials never land
// in a config file or image layer.
//
// It also serves the gateway's (secret-redacted) WAN configuration at /config —
// which the prober's AI analysis reads so it can tell whether, say, Smart Queues
// is already enabled before recommending it — and watches that config for
// changes, posting a Grafana annotation whenever a setting is altered so config
// changes line up against the latency timeline.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/adamf/justrebootit/internal/grafana"
	"github.com/adamf/justrebootit/internal/udm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	cfg := udm.Config{
		BaseURL:            env("UDM_URL", "https://192.168.1.1"),
		Username:           os.Getenv("UDM_USERNAME"),
		Password:           os.Getenv("UDM_PASSWORD"),
		Site:               env("UDM_SITE", "default"),
		InsecureSkipVerify: envBool("UDM_INSECURE", true),
		Timeout:            envDuration("UDM_TIMEOUT", 10*time.Second),
	}
	listenAddr := env("UDM_LISTEN_ADDR", ":9431")

	if cfg.Username == "" || cfg.Password == "" {
		log.Fatalf("UDM_USERNAME and UDM_PASSWORD must be set")
	}

	client, err := udm.NewClient(cfg)
	if err != nil {
		log.Fatalf("udm client: %v", err)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(udm.NewCollector(client, cfg.Timeout))
	configChanges := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "udm_config_change_total",
		Help: "Number of detected changes to the gateway's WAN configuration.",
	})
	reg.MustRegister(configChanges)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/config", configHandler(client, cfg.Timeout))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Config-change watcher: poll the WAN config, diff it, and annotate Grafana
	// when it changes. Best-effort — no Grafana credentials means it still
	// counts changes, it just can't draw the annotation.
	gfx := grafana.New(
		env("GRAFANA_URL", "http://grafana:3000"),
		env("GRAFANA_USER", "admin"),
		os.Getenv("GRAFANA_ADMIN_PASSWORD"),
	)
	go watchConfig(ctx, client, gfx, configChanges, envDuration("UDM_CONFIG_INTERVAL", 5*time.Minute), cfg.Timeout)

	go func() {
		log.Printf("udm exporter scraping %s, serving metrics on %s/metrics (config at /config)", cfg.BaseURL, listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("metrics server: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// configHandler serves the gateway's redacted WAN configuration as JSON. The
// prober's AI analysis fetches this to reason about already-applied settings.
func configHandler(client *udm.Client, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		wans, err := client.WANConfig(ctx)
		if err != nil {
			http.Error(w, "config fetch failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"wan": wans})
	}
}

// watchConfig polls the WAN config on an interval and annotates Grafana on any
// change. The first successful poll only establishes a baseline.
func watchConfig(ctx context.Context, client *udm.Client, gfx *grafana.Client, changes prometheus.Counter, interval, timeout time.Duration) {
	var prev map[string]string

	poll := func() {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		wans, err := client.WANConfig(pctx)
		if err != nil {
			log.Printf("config watch: %v", err)
			return
		}
		current := udm.Flatten(wans)
		diff := udm.DiffConfig(prev, current)
		prev = current
		if len(diff) == 0 {
			return
		}
		changes.Add(float64(len(diff)))
		log.Printf("udm config changed (%d): %v", len(diff), diff)
		text := "UDM WAN configuration changed:\n- " + joinLines(diff)
		actx, acancel := context.WithTimeout(ctx, 10*time.Second)
		defer acancel()
		if err := gfx.Post(actx, grafana.Annotation{
			Time: time.Now(),
			Tags: []string{"justrebootit", "config-change"},
			Text: text,
		}); err != nil {
			log.Printf("config watch: posting annotation: %v", err)
		}
	}

	poll() // establish baseline immediately
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n- "
		}
		out += l
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
