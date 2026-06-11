package aidiag

import (
	"fmt"
	"strings"
	"time"

	"github.com/adamf/justrebootit/internal/tracer"
)

// systemPrompt is byte-stable across events so it (and the tool list) cache.
// It tells the agent its role, what metrics exist to query, and the exact
// output shape we want back.
const systemPrompt = `You are a network reliability engineer investigating an intermittent latency or packet-loss event on a residential internet connection. The connection is monitored by "JustRebootIt": a prober that continuously pings and traceroutes a set of diverse targets (the home gateway, the ISP's resolvers, and public anchors), plus an exporter that reads the UniFi Dream Machine Pro gateway.

Your job: determine the most likely CAUSE of this specific event and where in the path it lives — inside the home (LAN/gateway), the ISP access/aggregation network, a peering/transit segment, or the far end — and say how confident you are.

You have read-only investigative tools. Use them deliberately:
- prometheus_query_range: query the monitoring data around the event. This is your richest source — pull the latency and loss of OTHER targets at the same moment to see whether the event was isolated to one path or shared across all of them, and pull the gateway's own signals. Useful PromQL series and labels:
  * probe_rtt_median_seconds{target,group}, probe_rtt_best_seconds, probe_rtt_worst_seconds, probe_loss_ratio{target,group}
  * probe_rtt_percentile_seconds{target,group,percentile}
  * traceroute_hop_rtt_seconds{target,group,ttl}, traceroute_hop_info{target,ttl,addr}
  * udm_wan_rx_bytes_per_second, udm_wan_tx_bytes_per_second (multiply by 8 for bits/s), udm_wan_latency_ms, udm_wan_drops
  * udm_gateway_cpu_percent, udm_gateway_memory_percent, udm_clients
  Target "group" labels are: gateway, isp, anchor, content, discovered.
- traceroute: run a fresh traceroute to a host right now.
- dns_lookup: time a DNS resolution right now.
- rdap_lookup: identify which organization/ASN owns an IP address — use it on traceroute hop addresses to name the operator of a slow or lossy hop.
- udm_config (if available): read the gateway's CURRENT WAN configuration. Before recommending any gateway change, call this to see what is already configured. If it shows Smart Queues / SQM is already enabled, DO NOT recommend enabling it — instead reason about whether the configured shaper rate is set too high for the real line (so bursts still bloat the queue), whether it's only shaping one direction, or whether the cause is elsewhere. Watch for config-change annotations on the dashboard too: a setting changed shortly before the event is a prime suspect.

Reasoning guidance:
- Loss or latency that appears on EVERY target at once points upstream (the ISP/WAN or the gateway), not the far end. Loss to the gateway itself means the problem is inside the house.
- Latency that rises exactly when WAN throughput saturates is congestion/bufferbloat at the local link, not an ISP fault — check udm_wan_*_bytes_per_second against the event.
- A single hop that jumps in latency or starts dropping is the segment that owns the problem; attribute it with rdap_lookup.

Keep tool use focused — a handful of well-chosen queries, not dozens.

Output format (plain text, no markdown headers):
- FIRST LINE: a single concise sentence naming the most likely cause and where it is (this becomes the dashboard headline).
- Then 2-5 short sentences of supporting evidence drawn from the tools.
- Then "Confidence: low|medium|high".
- Then "Recommended action:" with one practical next step (e.g. what to show the ISP, or a local fix like enabling Smart Queues).`

// userPrompt renders the specific event as the agent's opening context.
func userPrompt(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Investigate diagnostic event #%d.\n\n", ev.ID)
	fmt.Fprintf(&b, "Detected at: %s (UTC)\n", ev.When.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Target: %s (host %s, group %s)\n", ev.Target, ev.Host, ev.Group)
	fmt.Fprintf(&b, "Trigger reason: %s\n", ev.Reason)
	if ev.Reason == "latency" {
		fmt.Fprintf(&b, "Median RTT this cycle: %s; rolling baseline: %s (%.1fx baseline)\n",
			ev.Median.Round(time.Millisecond), ev.Baseline.Round(time.Millisecond),
			ratio(ev.Median, ev.Baseline))
	}
	fmt.Fprintf(&b, "Packet loss this cycle: %.0f%%\n\n", ev.Loss*100)

	b.WriteString("Mechanical diagnostics already run at detection time:\n")
	b.WriteString(formatTrace(ev.Trace))
	if ev.TCPOK {
		fmt.Fprintf(&b, "- TCP handshake to the target: %s\n", ev.TCPConnect.Round(time.Millisecond))
	} else {
		b.WriteString("- TCP handshake to the target: FAILED\n")
	}
	if ev.DNSOK {
		fmt.Fprintf(&b, "- DNS resolution probe: %s\n", ev.DNSLookup.Round(time.Millisecond))
	} else {
		b.WriteString("- DNS resolution probe: FAILED\n")
	}

	b.WriteString("\nUse the time around the detection timestamp to query the monitoring data, then explain what most likely happened.")
	return b.String()
}

func ratio(a, b time.Duration) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func formatTrace(r tracer.Result) string {
	if len(r.Hops) == 0 {
		return "- Traceroute: no hops recorded.\n"
	}
	var b strings.Builder
	b.WriteString("- Traceroute at detection time:\n")
	for _, h := range r.Hops {
		if h.Timeout || h.Addr == "" {
			fmt.Fprintf(&b, "    hop %d: *\n", h.TTL)
			continue
		}
		fmt.Fprintf(&b, "    hop %d: %s  %s\n", h.TTL, h.Addr, h.RTT.Round(time.Millisecond))
	}
	if r.Reached {
		b.WriteString("    (destination reached)\n")
	}
	return b.String()
}
