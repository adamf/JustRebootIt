package udm

import (
	"context"
	"log"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector adapts a Client to the Prometheus collector interface: each scrape
// triggers one live read of the gateway. Scraping on demand keeps the exporter
// stateless and means the metrics always reflect the moment Prometheus asked.
type Collector struct {
	client  *Client
	timeout time.Duration

	up             *prometheus.Desc
	wanLatency     *prometheus.Desc
	wanUptime      *prometheus.Desc
	wanRxRate      *prometheus.Desc
	wanTxRate      *prometheus.Desc
	wanDrops       *prometheus.Desc
	speedtestDown  *prometheus.Desc
	speedtestUp    *prometheus.Desc
	speedtestPing  *prometheus.Desc
	gatewayCPU     *prometheus.Desc
	gatewayMem     *prometheus.Desc
	gatewayUptime  *prometheus.Desc
	gatewayClients *prometheus.Desc
	clients        *prometheus.Desc
}

// NewCollector builds a Collector. timeout bounds each scrape's total work.
func NewCollector(client *Client, timeout time.Duration) *Collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	return &Collector{
		client:         client,
		timeout:        timeout,
		up:             d("udm_up", "1 if the last UDM scrape succeeded, else 0."),
		wanLatency:     d("udm_wan_latency_ms", "Gateway-measured WAN latency in milliseconds."),
		wanUptime:      d("udm_wan_uptime_seconds", "WAN connection uptime in seconds."),
		wanRxRate:      d("udm_wan_rx_bytes_per_second", "Current WAN download rate in bytes per second."),
		wanTxRate:      d("udm_wan_tx_bytes_per_second", "Current WAN upload rate in bytes per second."),
		wanDrops:       d("udm_wan_drops", "Recently dropped packets on the WAN as reported by the gateway."),
		speedtestDown:  d("udm_speedtest_download_mbps", "Download throughput from the most recent gateway speedtest, in Mbps."),
		speedtestUp:    d("udm_speedtest_upload_mbps", "Upload throughput from the most recent gateway speedtest, in Mbps."),
		speedtestPing:  d("udm_speedtest_ping_ms", "Latency from the most recent gateway speedtest, in milliseconds."),
		gatewayCPU:     d("udm_gateway_cpu_percent", "Gateway CPU utilization, percent."),
		gatewayMem:     d("udm_gateway_memory_percent", "Gateway memory utilization, percent."),
		gatewayUptime:  d("udm_gateway_uptime_seconds", "Gateway uptime in seconds."),
		gatewayClients: d("udm_gateway_clients", "Clients associated with the gateway device."),
		clients:        d("udm_clients", "Total connected clients reported across LAN and WLAN subsystems."),
	}
}

// Describe sends every metric descriptor to ch.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.wanLatency
	ch <- c.wanUptime
	ch <- c.wanRxRate
	ch <- c.wanTxRate
	ch <- c.wanDrops
	ch <- c.speedtestDown
	ch <- c.speedtestUp
	ch <- c.speedtestPing
	ch <- c.gatewayCPU
	ch <- c.gatewayMem
	ch <- c.gatewayUptime
	ch <- c.gatewayClients
	ch <- c.clients
}

// Collect performs a live scrape and emits the resulting samples. On failure it
// emits only udm_up=0 so the dashboard can show the exporter as down without
// publishing stale or zero latency values.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	snap, err := c.client.Snapshot(ctx)
	if err != nil {
		log.Printf("udm scrape failed: %v", err)
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}

	g := func(desc *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
	}
	g(c.up, 1)
	g(c.wanLatency, snap.WANLatencyMS)
	g(c.wanUptime, snap.WANUptimeSec)
	g(c.wanRxRate, snap.WANRxBytesRate)
	g(c.wanTxRate, snap.WANTxBytesRate)
	g(c.wanDrops, snap.WANDrops)
	g(c.speedtestDown, snap.SpeedtestDownMbps)
	g(c.speedtestUp, snap.SpeedtestUpMbps)
	g(c.speedtestPing, snap.SpeedtestPingMS)
	g(c.gatewayCPU, snap.GatewayCPUPct)
	g(c.gatewayMem, snap.GatewayMemPct)
	g(c.gatewayUptime, snap.GatewayUptime)
	g(c.gatewayClients, snap.GatewayClients)
	g(c.clients, snap.Clients)
}
