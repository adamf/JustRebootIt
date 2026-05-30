package udm

import (
	"encoding/json"
	"testing"
)

func TestFlexFloatUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{"number", `12.5`, 12.5},
		{"integer", `42`, 42},
		{"quoted-number", `"3.14"`, 3.14},
		{"quoted-integer", `"100"`, 100},
		{"null", `null`, 0},
		{"empty-string", `""`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f FlexFloat
			if err := json.Unmarshal([]byte(tc.in), &f); err != nil {
				t.Fatalf("unmarshal %q: %v", tc.in, err)
			}
			if f.Float() != tc.want {
				t.Fatalf("FlexFloat(%q) = %v, want %v", tc.in, f.Float(), tc.want)
			}
		})
	}
}

// TestSnapshotParsing checks that a representative UniFi-shaped payload, with
// its mix of numeric and string-encoded numbers, decodes into the fields the
// exporter publishes.
func TestSnapshotParsing(t *testing.T) {
	const healthJSON = `{"data":[
		{"subsystem":"www","status":"ok","latency":15,"uptime":123456,
		 "rx_bytes-r":"1048576","tx_bytes-r":524288,"drops":2,
		 "xput_down":940.5,"xput_up":"38.2","speedtest_ping":12},
		{"subsystem":"wlan","num_user":7,"num_guest":1},
		{"subsystem":"lan","num_user":4}
	]}`
	const deviceJSON = `{"data":[
		{"type":"usw","model":"US24","system-stats":{"cpu":"99","mem":"99"}},
		{"type":"udm","model":"UDMPRO","uptime":98765,"num_sta":12,
		 "system-stats":{"cpu":"5.5","mem":"43.2"}}
	]}`

	var health healthResp
	if err := json.Unmarshal([]byte(healthJSON), &health); err != nil {
		t.Fatalf("health: %v", err)
	}
	var devices deviceResp
	if err := json.Unmarshal([]byte(deviceJSON), &devices); err != nil {
		t.Fatalf("devices: %v", err)
	}

	// Replicate the aggregation Snapshot performs so we exercise the same
	// field mapping without needing a live gateway.
	var snap Snapshot
	for _, s := range health.Data {
		switch s.Subsystem {
		case "www", "wan":
			snap.WANLatencyMS = s.Latency.Float()
			snap.WANRxBytesRate = s.RxRate.Float()
			snap.WANTxBytesRate = s.TxRate.Float()
			snap.SpeedtestDownMbps = s.XputDown.Float()
			snap.SpeedtestUpMbps = s.XputUp.Float()
		case "wlan", "lan":
			snap.Clients += s.NumUser.Float() + s.NumGuest.Float()
		}
	}
	for _, d := range devices.Data {
		if gatewayTypes[d.Type] {
			snap.GatewayModel = d.Model
			snap.GatewayCPUPct = d.SystemStats.CPU.Float()
			snap.GatewayClients = d.NumSta.Float()
			break
		}
	}

	if snap.WANLatencyMS != 15 {
		t.Errorf("WANLatencyMS = %v, want 15", snap.WANLatencyMS)
	}
	if snap.WANRxBytesRate != 1048576 {
		t.Errorf("WANRxBytesRate = %v, want 1048576", snap.WANRxBytesRate)
	}
	if snap.WANTxBytesRate != 524288 {
		t.Errorf("WANTxBytesRate = %v, want 524288", snap.WANTxBytesRate)
	}
	if snap.SpeedtestUpMbps != 38.2 {
		t.Errorf("SpeedtestUpMbps = %v, want 38.2", snap.SpeedtestUpMbps)
	}
	if snap.Clients != 12 { // 7+1 + 4
		t.Errorf("Clients = %v, want 12", snap.Clients)
	}
	if snap.GatewayModel != "UDMPRO" {
		t.Errorf("GatewayModel = %q, want UDMPRO", snap.GatewayModel)
	}
	if snap.GatewayCPUPct != 5.5 {
		t.Errorf("GatewayCPUPct = %v, want 5.5 (must pick the gateway, not the switch)", snap.GatewayCPUPct)
	}
	if snap.GatewayClients != 12 {
		t.Errorf("GatewayClients = %v, want 12", snap.GatewayClients)
	}
}
