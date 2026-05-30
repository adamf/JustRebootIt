package udm

import "context"

// These structs cover only the fields the exporter publishes. UniFi returns
// far more; anything not listed is ignored by the JSON decoder.

type healthResp struct {
	Data []subsystem `json:"data"`
}

// subsystem is one entry from /stat/health. The internet-facing entries
// ("www" on UniFi OS, "wan" on older firmware) carry WAN latency, throughput,
// and the most recent speedtest results.
type subsystem struct {
	Subsystem       string    `json:"subsystem"`
	Status          string    `json:"status"`
	Latency         FlexFloat `json:"latency"`        // ms, gateway's own WAN latency
	Uptime          FlexFloat `json:"uptime"`         // seconds
	RxRate          FlexFloat `json:"rx_bytes-r"`     // bytes/sec, current WAN download
	TxRate          FlexFloat `json:"tx_bytes-r"`     // bytes/sec, current WAN upload
	Drops           FlexFloat `json:"drops"`          // recent dropped packets
	XputUp          FlexFloat `json:"xput_up"`        // Mbps, last speedtest upload
	XputDown        FlexFloat `json:"xput_down"`      // Mbps, last speedtest download
	SpeedtestPing   FlexFloat `json:"speedtest_ping"` // ms, last speedtest latency
	SpeedtestStatus string    `json:"speedtest_status"`
	NumUser         FlexFloat `json:"num_user"`
	NumGuest        FlexFloat `json:"num_guest"`
}

type deviceResp struct {
	Data []device `json:"data"`
}

type device struct {
	Type        string      `json:"type"`
	Model       string      `json:"model"`
	Name        string      `json:"name"`
	SystemStats systemStats `json:"system-stats"`
	Uptime      FlexFloat   `json:"uptime"`
	NumSta      FlexFloat   `json:"num_sta"`
}

type systemStats struct {
	CPU FlexFloat `json:"cpu"` // percent
	Mem FlexFloat `json:"mem"` // percent
}

// gatewayTypes are the UniFi device "type" values that denote a gateway/router
// whose CPU, memory, and uptime we want to track.
var gatewayTypes = map[string]bool{
	"udm": true, // Dream Machine / Dream Machine Pro / SE
	"uxg": true, // Next-Gen Gateway
	"ugw": true, // older UniFi Security Gateway naming
	"usg": true,
}

// Snapshot is a single point-in-time reading of the gateway, flattened to the
// values the exporter publishes. Fields are zero when the source did not
// provide them; Up reports whether the scrape itself succeeded.
type Snapshot struct {
	Up bool

	// WAN / internet subsystem.
	WANStatus         string
	WANLatencyMS      float64
	WANUptimeSec      float64
	WANRxBytesRate    float64
	WANTxBytesRate    float64
	WANDrops          float64
	SpeedtestDownMbps float64
	SpeedtestUpMbps   float64
	SpeedtestPingMS   float64

	// Gateway device.
	GatewayModel   string
	GatewayCPUPct  float64
	GatewayMemPct  float64
	GatewayUptime  float64
	GatewayClients float64

	// Connected clients reported by the health subsystems.
	Clients float64
}

// Snapshot logs in if needed and reads the health and device endpoints,
// returning a flattened view. A non-nil error means the scrape failed; the
// returned Snapshot then has Up == false.
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var snap Snapshot

	var health healthResp
	if err := c.getJSON(ctx, c.path("/stat/health"), &health); err != nil {
		return snap, err
	}
	for _, s := range health.Data {
		switch s.Subsystem {
		case "www", "wan":
			snap.WANStatus = s.Status
			snap.WANLatencyMS = s.Latency.Float()
			snap.WANUptimeSec = s.Uptime.Float()
			snap.WANRxBytesRate = s.RxRate.Float()
			snap.WANTxBytesRate = s.TxRate.Float()
			snap.WANDrops = s.Drops.Float()
			snap.SpeedtestDownMbps = s.XputDown.Float()
			snap.SpeedtestUpMbps = s.XputUp.Float()
			snap.SpeedtestPingMS = s.SpeedtestPing.Float()
		case "wlan", "lan":
			snap.Clients += s.NumUser.Float() + s.NumGuest.Float()
		}
	}

	var devices deviceResp
	if err := c.getJSON(ctx, c.path("/stat/device"), &devices); err != nil {
		return snap, err
	}
	for _, d := range devices.Data {
		if !gatewayTypes[d.Type] {
			continue
		}
		snap.GatewayModel = d.Model
		snap.GatewayCPUPct = d.SystemStats.CPU.Float()
		snap.GatewayMemPct = d.SystemStats.Mem.Float()
		snap.GatewayUptime = d.Uptime.Float()
		snap.GatewayClients = d.NumSta.Float()
		break
	}

	snap.Up = true
	return snap, nil
}

// path builds a site-scoped Network API path.
func (c *Client) path(suffix string) string {
	return "/api/s/" + c.cfg.Site + suffix
}
