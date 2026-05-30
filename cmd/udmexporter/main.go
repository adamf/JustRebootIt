// Command udmexporter exposes UniFi Dream Machine Pro statistics as Prometheus
// metrics so WAN throughput, gateway load, and the gateway's own latency can be
// graphed alongside the externally measured latency from the prober. It is
// configured entirely through environment variables so credentials never land
// in a config file or image layer.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("udm exporter scraping %s, serving metrics on %s/metrics", cfg.BaseURL, listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("metrics server: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
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
